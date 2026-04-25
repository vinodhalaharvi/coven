// Package sqlcagent implements a codegen.Agent for sqlc.
//
// sqlc generates type-safe Go code from .sql files. Configuration lives
// in sqlc.yaml (typically at the module root). When .sql files change,
// sqlc regenerates the Go output.
//
// This agent fires on FileChangeFacts where .sql files (or sqlc.yaml)
// are touched. The runner invokes `sqlc generate` at the module root
// and reads back generated Go files for the codegen.Agent's diff-write
// to handle cascade-breaking.
package sqlcagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/codegen"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/ownership"
)

// Cfg is the sqlc-agent's per-config type.
type Cfg struct {
	ModuleRoot string // where sqlc.yaml lives; sqlc runs here
	GenRoot    string // base directory where sqlc writes outputs (for reading back)
	SqlcBin    string // path or name; defaults to "sqlc"
}

// Fact is the thin status fact the agent posts.
type Fact struct {
	AgentID      string        `json:"agent_id"`
	GenRoot      string        `json:"gen_root"`
	OK           bool          `json:"ok"`
	ChangedFiles []string      `json:"changed_files,omitempty"`
	Output       string        `json:"output,omitempty"`
	Duration     time.Duration `json:"duration"`
	ObservedAt   time.Time     `json:"observed_at"`
}

// Key returns the blackboard key for an sqlc fact.
func Key(genRoot string) string {
	return "sqlc:" + genRoot
}

// HasSqlcConfig reports whether moduleRoot has a sqlc config file.
func HasSqlcConfig(moduleRoot string) bool {
	for _, name := range []string{"sqlc.yaml", "sqlc.yml", "sqlc.json"} {
		if _, err := os.Stat(filepath.Join(moduleRoot, name)); err == nil {
			return true
		}
	}
	return false
}

// Runner returns a codegen.Runner that invokes sqlc at the module root
// and reads back .go files under genRoot.
func Runner() codegen.Runner[Cfg] {
	return func(ctx context.Context, cfg Cfg) (codegen.RunResult, error) {
		bin := cfg.SqlcBin
		if bin == "" {
			bin = "sqlc"
		}
		cmd := exec.CommandContext(ctx, bin, "generate")
		cmd.Dir = cfg.ModuleRoot
		out, runErr := cmd.CombinedOutput()
		output := string(out)
		if runErr != nil {
			return codegen.RunResult{Output: output}, fmt.Errorf("sqlc generate: %w", runErr)
		}

		// Walk genRoot collecting .go files.
		var files []codegen.GeneratedFile
		walkErr := filepath.Walk(cfg.GenRoot, func(path string, info os.FileInfo, werr error) error {
			if werr != nil {
				return nil // skip unreadable nodes
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			files = append(files, codegen.GeneratedFile{Path: path, Bytes: body})
			return nil
		})
		if walkErr != nil {
			return codegen.RunResult{Output: output}, walkErr
		}
		return codegen.RunResult{Files: files, Output: output}, nil
	}
}

// Project converts a codegen.RunResult into a Fact.
func Project(agentID string) codegen.Project[Cfg, Fact] {
	return func(cfg Cfg, res codegen.RunResult, changed []string, runErr error) Fact {
		return Fact{
			AgentID:      agentID,
			GenRoot:      cfg.GenRoot,
			OK:           runErr == nil,
			ChangedFiles: changed,
			Output:       res.Output,
			Duration:     res.Duration,
			ObservedAt:   time.Now(),
		}
	}
}

// BuildReactiveWorker constructs the worker. The agent fires when:
//   - any .sql file or sqlc.yaml changes anywhere under ModuleRoot
func BuildReactiveWorker(
	agentID string,
	cfg Cfg,
	fsBoard *blackboard.Board[fsmonitor.FileChangeFact],
	sqlcBoard *blackboard.Board[Fact],
	owner ownership.Registry,
) supervisor.ReactiveWorker[codegen.Trigger, Fact] {
	src := func(ctx context.Context) (<-chan codegen.Trigger, error) {
		// Filter: keep facts where any changed file is .sql or named sqlc.{yaml,yml,json}.
		filter := func(f fsmonitor.FileChangeFact) bool {
			for _, p := range f.ChangedFiles {
				base := filepath.Base(p)
				if strings.HasSuffix(p, ".sql") {
					return true
				}
				if base == "sqlc.yaml" || base == "sqlc.yml" || base == "sqlc.json" {
					return true
				}
			}
			return false
		}
		raw := fsmonitor.Subscribe(fsBoard, filter)
		ch, err := raw(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan codegen.Trigger, 4)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case f, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- codegen.Trigger{
						Reason:       "fs:sql-or-config",
						ChangedFiles: f.ChangedFiles,
						At:           f.ChangedAt,
					}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}

	return codegen.BuildReactiveWorker(codegen.Config[Cfg, Fact]{
		AgentID:        agentID,
		Cfg:            cfg,
		Runner:         Runner(),
		Project:        Project(agentID),
		Board:          sqlcBoard,
		BoardKey:       func(f Fact) string { return Key(f.GenRoot) },
		Owner:          owner,
		Source:         src,
		HealthFromFact: func(f Fact) bool { return f.OK },
	})
}
