// Package buildhealth is the conversational agent for module-wide
// Go compilation health. Its job is to run 'go build ./...' (and
// optionally 'go vet ./...') after the codegen agents settle, surface
// errors, and propose fixes.
//
// Unlike the codegen agents, build-agent doesn't generate anything.
// It's a verifier — the cross-cutting truth that tells you whether the
// project compiles together regardless of what the per-domain agents
// thought their work looked like.
package buildhealth

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

const Role = `You are the build-health agent for a Go project. Your single job is to run 'go build ./...' at the module root and report whether the project compiles end-to-end.

Why this matters: per-domain agents (proto, wire, sqlc) each ensure THEIR generated code is current and self-consistent. But cross-cutting drift — where a consumer references a field that the generated code no longer has — only shows up at module-level compilation. You catch that.

Your method, in order:
  1. When you wake (after upstream codegen agents have settled), run 'go build ./...' from the module root.
  2. If it succeeds, report 'module compiles cleanly' and stop. That's equilibrium.
  3. If it fails, read the error output and diagnose:
       - 'cannot find module providing package X' → propose 'go get X' (the simplest case)
       - 'undefined: X.FieldY' or 'X.FieldY undefined' → cross-tool drift. The consumer references a field that doesn't exist in some generated code. This is a HUMAN ISSUE — explain plainly which file and which line, suggest looking at the proto/sql/wire definitions, but do NOT try to fix it automatically. The fix requires a human deciding whether to update the schema or the consumer.
       - syntax errors in generated files → could be stale generation. Suggest re-triggering the relevant codegen agent (don't run it yourself; that's the codegen agent's domain).
       - other errors → describe them, don't try to fix them.

Constraints:
  - You do NOT modify .go, .proto, .sql, or any source/generated files. Your only outputs are diagnostics and at most a 'go get'/'go mod tidy' proposal for missing deps.
  - You do NOT run 'go test'. Tests are not in your scope.
  - You wake AFTER codegen edits settle, so spurious failures from mid-cascade transient states are unlikely. If a build fails immediately after a codegen agent finished, the failure is probably real drift.

When the module builds clean, return one short final message and stop.`

type Config struct {
	ID         string
	ModuleRoot string
	Sender     llm.Sender
	FSBoard    *blackboard.Board[fsmonitor.FileChangeFact]
	Confirm    agent.ConfirmFunc
	Print      agent.PrintFunc
	Settle     time.Duration // longer than codegen agents - we want to wake AFTER they settle. Default 3s.
}

type Agent struct {
	cfg     Config
	inner   *agent.Agent
	mu      sync.Mutex
	pending bool
}

func New(cfg Config) *Agent {
	if cfg.ID == "" {
		cfg.ID = "build-agent"
	}
	if cfg.Settle <= 0 {
		// 3s default: gives the codegen agents (1s settle) time to do
		// their work before we run a build. The cost of being slightly
		// late is small; the cost of running mid-cascade is wasted compile.
		cfg.Settle = 3 * time.Second
	}
	inner := agent.New(agent.Config{
		ID:      cfg.ID,
		Role:    Role,
		Tools:   agent.StandardTools(cfg.ModuleRoot),
		Sender:  cfg.Sender,
		Confirm: cfg.Confirm,
		Print:   cfg.Print,
	})
	return &Agent{cfg: cfg, inner: inner}
}

// IsRelevant: any .go file or go.mod. The build-agent cares about
// effectively everything that affects compilation, but not .proto/.sql
// directly (those wake the codegen agents instead, which then write
// .go files which DO wake us).
func IsRelevant(path string) bool {
	base := filepath.Base(path)
	if base == "go.mod" || base == "go.sum" {
		return true
	}
	if strings.HasSuffix(base, ".go") {
		return true
	}
	return false
}

func (a *Agent) Run(ctx context.Context) error {
	go a.wakeOnce(ctx, "startup: just attached. Run 'go build ./...' to establish baseline build health.")

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
	// For build-agent specifically, large bursts of .go file changes
	// from codegen are common; just say "many changes" rather than list
	// 50 paths.
	if len(uniq) == 1 {
		return "observed change to: " + uniq[0]
	}
	if len(uniq) <= 5 {
		return fmt.Sprintf("observed changes to %d files:\n  %s", len(uniq), strings.Join(uniq, "\n  "))
	}
	return fmt.Sprintf("observed changes to %d files (%s and %d others)", len(uniq), uniq[0], len(uniq)-1)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// init registers build-agent with the v2 router. v1 cmd/demo wires this
// up via -conv-build directly; this registration is for v2 router metadata.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "build",
		Description: "verify go build ./... passes and report drift",
		TypicalTriggers: "Any .go file change. Acts as a cross-cutting verifier rather than a producer.",
		DomainFiles:     "(none — build-agent is read-only; never writes files)",
		AvoidsWhen:      "Skip when changes are limited to docs, README, or non-Go config files that can't affect compilation.",
		ExampleScenarios: "After proto-agent regenerates *.pb.go files, this agent runs go build ./... to verify the project still compiles. If it fails with errors that point to wire_gen.go or sqlc output, the report surfaces which agent should fix it.",
		Detect: func(goMod string, moduleRoot string) bool {
			// Always applicable for any Go project.
			return goMod != ""
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "build-agent",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				FSBoard:    deps.FSBoard,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
				Settle:     3 * time.Second,
			})
		},
	})
}
