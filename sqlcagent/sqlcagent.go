// Package sqlcagent is the conversational agent for sqlc-generated
// type-safe Go code from .sql files. Its job is to keep the generated
// Go output consistent with .sql sources and sqlc.yaml configuration.
package sqlcagent

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
)

const Role = `You are the sqlc agent for a Go project. Your single job is to keep generated Go code from SQL files consistent and current.

A sqlc-using project has:
  - sqlc.yaml (or sqlc.yml, sqlc.json) at the project root with version, engine, queries, schema, and gen settings.
  - .sql files for queries and schema migrations.
  - Generated .go files (typically under an internal/db/ or gen/db/ directory specified by sqlc.yaml).

Your method:
  1. When you wake, look for sqlc config (sqlc.yaml, sqlc.yml, sqlc.json) at project root. If absent, the project has no sqlc domain — say so and stop.
  2. Read sqlc.yaml to understand the schema/query paths and the output directory.
  3. Check whether the generated files exist and look current. If a .sql file was edited or generated files are missing, regenerate.
  4. Regeneration: 'sqlc generate' from the project root.
  5. Diagnose failures from sqlc output:
       - 'sqlc: command not found' → propose 'go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest' (note: this requires a working Go install)
       - schema parse errors → human SQL to fix; explain plainly and stop
       - missing dependency in generated code (e.g. github.com/jackc/pgx/v5) → propose 'go get <module>'
  6. After regeneration, the project should compile in your domain (you may run 'go build ./...' as a sanity check, but don't fix non-sqlc build errors).

Constraints:
  - You do NOT edit .sql files yourself. Schema and query design are human decisions.
  - You do NOT touch *.pb.go, wire_gen.go, or any output owned by other agents.
  - You do NOT alter go.mod beyond adding sqlc-runtime dependencies.
  - When you've reached a consistent state, return one short final message describing what you did.`

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
		cfg.ID = "sqlc-agent"
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 1 * time.Second
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

// IsRelevant: .sql files, sqlc config files.
func IsRelevant(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".sql") {
		return true
	}
	if base == "sqlc.yaml" || base == "sqlc.yml" || base == "sqlc.json" {
		return true
	}
	return false
}

func (a *Agent) Run(ctx context.Context) error {
	go a.wakeOnce(ctx, "startup: just attached. Survey the project for sqlc config and bring the sqlc domain to a healthy state.")

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
