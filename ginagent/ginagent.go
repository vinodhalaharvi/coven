// Package ginagent is the conversational agent for projects using
// the gin HTTP framework. Unlike the bootstrap agents (mainbuilder,
// docker, makefile), this is a steady-state agent: it watches for
// drift between the data layer (sqlc output) and the HTTP handlers,
// and proposes work until the project compiles with handlers wired.
//
// Domain triggers:
//   - sqlc output files (gen/db/*.sql.go or wherever sqlc writes)
//   - existing handler files (internal/handlers/*.go and similar)
//   - anything in a directory matching common handler conventions
//
// Equilibrium = sqlc methods have corresponding handlers AND go build
// ./... succeeds. The role string makes this explicit so Claude
// iterates (propose → diagnose build error → propose fix) rather than
// stopping at "I wrote some files, hope they compile."
package ginagent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

const Role = `You are the gin HTTP handler agent. Your job is to keep gin route handlers in sync with the project's data layer (typically sqlc-generated *.sql.go files) so the project compiles with all data-layer methods exposed via HTTP routes.

You are a STEADY-STATE agent like proto-agent and sqlc-agent. You wake whenever relevant files change, you do work, you reach equilibrium, you go dormant until the next change. Equilibrium has a precise definition: every CRUD method on the sqlc Queries struct has a corresponding HTTP handler, AND 'go build ./...' succeeds with the handlers in place.

Your method:
  1. Survey: read go.mod to confirm gin is present. List the sqlc output directory's *.sql.go files (excluding db.go and models.go boilerplate) — each one defines a resource (users.sql.go → user resource, orders.sql.go → order resource). Read those files to enumerate the methods on the Queries struct (GetUser, CreateUser, ListActiveUsers, etc.).
  2. List existing handler files. If any exist, read them to understand the project's conventions (file layout, naming, middleware, error handling, request/response types).
  3. Compare: for each sqlc method, is there a handler that calls it? Identify gaps (methods without handlers) and stale handlers (handlers calling methods that no longer exist on Queries).
  4. Propose ONE handler file per resource that needs work. Each file:
     - Path: internal/handlers/<resource>.go (or matching the project's existing convention if one exists).
     - Contents: imports, a Handler struct with *db.Queries (or whatever the sqlc package is named), constructor, one method per CRUD operation, route binding via gin context, JSON request/response.
     - For business logic that requires judgment (validation rules, authorization, error code mapping), leave a TODO comment — DO NOT invent rules. The user knows what to validate.
     - Aim for ~80 lines per file. If a resource has many sqlc methods, a longer file is fine, but flag it in your summary.
  5. Propose ONE router file (internal/handlers/router.go or matching convention) with a SetupRouter function that constructs each Handler and registers routes.
  6. Run 'go build ./...' to verify your handlers compile. If it fails:
     - If the failure is in YOUR handlers (wrong import, wrong signature, type mismatch), diagnose, propose a fix, iterate.
     - If the failure is elsewhere (proto, sqlc-generated, wire, app code), DO NOT touch it. Report and stop — that's another agent's or a human's domain.
  7. When go build passes AND every sqlc method has a handler, summarize and stop. Equilibrium reached.

Constraints:
  - You write/edit ONLY files under internal/handlers/ (or the existing handlers directory). You do NOT modify cmd/*/main.go, sqlc-generated files, wire.go, .proto files, or any other source.
  - You do NOT add dependencies to go.mod. If a starter would benefit from validator/v10 or similar, use stdlib equivalent (e.g. fmt-based validation) and leave a comment.
  - You do NOT invent business logic. Validation rules, authorization checks, rate limiting, retries, error code mapping — all stubbed with TODO. The user's job, not yours.
  - You do NOT propose tests, OpenAPI specs, or middleware beyond what's clearly already established.
  - If the project doesn't use gin, has no sqlc output (no data layer to wire), or every sqlc method already has a working handler, say so plainly and stop.
  - Each file write is a separate exec heredoc — auditable diff, individual y/N per file.

Final summary: which files you wrote/edited, which sqlc methods they wired, what TODO comments the user needs to fill in (validation, auth), and whether 'go build ./...' passed.`

type Config struct {
	ID         string
	ModuleRoot string
	Sender     llm.Sender
	FSBoard    *blackboard.Board[fsmonitor.FileChangeFact]
	Confirm    agent.ConfirmFunc
	Print      agent.PrintFunc
	Settle     time.Duration
}

type Agent struct {
	cfg     Config
	inner   *agent.Agent
	mu      sync.Mutex
	pending bool
}

func New(cfg Config) *Agent {
	if cfg.ID == "" {
		cfg.ID = "gin-agent"
	}
	if cfg.Settle <= 0 {
		// Settle longer than codegen agents (1s) but shorter than build
		// (3s) — gin-agent wants codegen to finish first, but should
		// react before the user has moved on.
		cfg.Settle = 2 * time.Second
	}
	inner := agent.New(agent.Config{
		ID:       cfg.ID,
		Role:     Role,
		Tools:    agent.StandardTools(cfg.ModuleRoot),
		Sender:   cfg.Sender,
		Confirm:  cfg.Confirm,
		Print:    cfg.Print,
		MaxTurns: 60, // Generous: handler scaffolding may need many tool calls.
	})
	return &Agent{cfg: cfg, inner: inner}
}

// IsRelevant: sqlc output files (*.sql.go), existing handler files
// (anything under internal/handlers/, internal/api/, internal/controllers/).
// We're conservative — better to wake unnecessarily than miss real drift.
func IsRelevant(path string) bool {
	slash := filepath.ToSlash(path)
	base := filepath.Base(path)

	// sqlc output: *.sql.go files. Don't fire on db.go or models.go
	// (those are boilerplate that doesn't change with new methods).
	if strings.HasSuffix(base, ".sql.go") {
		return true
	}

	// Anything in a handlers/api/controllers directory.
	if strings.Contains(slash, "/internal/handlers/") ||
		strings.Contains(slash, "/internal/api/") ||
		strings.Contains(slash, "/internal/controllers/") ||
		strings.HasPrefix(slash, "internal/handlers/") ||
		strings.HasPrefix(slash, "internal/api/") ||
		strings.HasPrefix(slash, "internal/controllers/") {
		return true
	}

	return false
}

func (a *Agent) Run(ctx context.Context) error {
	go a.wakeOnce(ctx, "startup: just attached. Survey the project for gin usage and sqlc output, identify any data-layer methods that don't have handlers yet, and bring the gin domain to a healthy state.")

	if a.cfg.FSBoard == nil {
		<-ctx.Done()
		return nil
	}

	sub := fsmonitor.Subscribe(a.cfg.FSBoard, func(f fsmonitor.FileChangeFact) bool {
		for _, p := range f.ChangedFiles {
			if IsRelevant(p) {
				return true
			}
		}
		return false
	})
	ch, err := sub(ctx)
	if err != nil {
		return err
	}

	var pendingFiles []string
	var timer *time.Timer
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(a.cfg.Settle, func() {
			files := pendingFiles
			pendingFiles = nil
			a.wakeOnce(ctx, formatObservation(files))
		})
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-ch:
			if !ok {
				return nil
			}
			for _, p := range f.ChangedFiles {
				if IsRelevant(p) {
					pendingFiles = append(pendingFiles, p)
				}
			}
			resetTimer()
		}
	}
}

func (a *Agent) wakeOnce(ctx context.Context, observation string) {
	a.mu.Lock()
	if a.pending {
		a.mu.Unlock()
		return
	}
	a.pending = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.pending = false
		a.mu.Unlock()
	}()

	a.cfg.Print(fmt.Sprintf("\n  [%s] waking: %s\n", a.cfg.ID, truncate(observation, 80)))
	if _, err := a.inner.Wake(ctx, observation); err != nil {
		a.cfg.Print(fmt.Sprintf("  [%s] error: %v\n", a.cfg.ID, err))
	}
}

func (a *Agent) HistoryLen() int { return a.inner.HistoryLen() }

func formatObservation(files []string) string {
	if len(files) == 0 {
		return "filesystem activity in your domain (no specific files identified)."
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(files))
	for _, f := range files {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	if len(uniq) == 1 {
		return "observed change to: " + uniq[0]
	}
	return fmt.Sprintf("observed changes to %d files in your domain:\n  %s", len(uniq), strings.Join(uniq, "\n  "))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// init registers this agent with the central registry. Detector:
// gin in go.mod. Steady-state behavior — Build receives FSBoard and
// the agent subscribes for the project's lifetime.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "gin",
		Description: "keep gin HTTP handlers in sync with sqlc-generated data layer",
		Detect: func(goMod string, moduleRoot string) bool {
			return registry.HasDep(goMod, "github.com/gin-gonic/gin")
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "gin-agent",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				FSBoard:    deps.FSBoard,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
				Settle:     2 * time.Second,
			})
		},
	})
}
