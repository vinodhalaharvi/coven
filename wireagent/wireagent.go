// Package wireagent implements a codegen.Agent for Google Wire.
//
// Wire is a Go DI codegen tool: you write `wire.go` declaring providers
// and bindings (under a `//go:build wireinject` constraint), run `wire`,
// and it produces `wire_gen.go` containing the actual injectors.
//
// This agent is a codegen.Agent[Cfg, Fact] specialized for Wire:
//   - Source: subscribes to FileChangeFact, filters for .go file changes
//     in any of the configured wire-using package directories
//   - Runner: invokes the `wire` binary at module root with package paths
//   - Project: produces a thin WireFact (no schema; outputs are on disk)
//
// Wire writes wire_gen.go itself; we read it back, claim ownership, and
// rely on the codegen.Agent's diff-write to suppress no-op cascades when
// wire's output didn't change.
package wireagent

import (
	"bytes"
	"context"
	"errors"
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

// Cfg is the wire-agent's per-config type (one per wire-using package).
type Cfg struct {
	PkgDir     string // absolute path to the Go package containing wire.go
	ModuleRoot string // module root (for `wire` invocation)
	WireBin    string // path or name; defaults to "wire"
}

// Fact is the thin status fact the agent posts.
type Fact struct {
	AgentID      string        `json:"agent_id"`
	PkgDir       string        `json:"pkg_dir"`
	OK           bool          `json:"ok"`
	ChangedFiles []string      `json:"changed_files,omitempty"`
	Output       string        `json:"output,omitempty"`
	Duration     time.Duration `json:"duration"`
	ObservedAt   time.Time     `json:"observed_at"`
}

// Key returns the blackboard key for a wire fact.
func Key(pkgDir string) string {
	return "wire:" + pkgDir
}

// IsWirePackage reports whether dir contains a wire injector source —
// a .go file with the `//go:build wireinject` constraint. This is the
// idiomatic way to mark a Wire injector file so it's excluded from
// normal builds.
func IsWirePackage(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		// Both the modern and legacy build constraint forms.
		if bytes.Contains(body, []byte("//go:build wireinject")) ||
			bytes.Contains(body, []byte("// +build wireinject")) {
			return true
		}
	}
	return false
}

// DiscoverWirePackages walks root and returns directories that contain
// a wire injector file.
func DiscoverWirePackages(root string) ([]string, error) {
	var pkgs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == "vendor" || base == "node_modules" || (strings.HasPrefix(base, ".") && base != ".") {
			return filepath.SkipDir
		}
		if IsWirePackage(path) {
			pkgs = append(pkgs, path)
		}
		return nil
	})
	return pkgs, err
}

// Runner returns a codegen.Runner that invokes the wire binary in the
// module root with the package's relative path. After wire writes
// wire_gen.go, the runner reads it back as a GeneratedFile so the
// codegen.Agent's diff-write owns the cascade-breaker.
func Runner() codegen.Runner[Cfg] {
	return func(ctx context.Context, cfg Cfg) (codegen.RunResult, error) {
		bin := cfg.WireBin
		if bin == "" {
			bin = "wire"
		}
		// Compute the package path relative to module root for `wire`.
		rel, err := filepath.Rel(cfg.ModuleRoot, cfg.PkgDir)
		if err != nil || strings.HasPrefix(rel, "..") {
			return codegen.RunResult{}, fmt.Errorf("PkgDir %q is not under ModuleRoot %q", cfg.PkgDir, cfg.ModuleRoot)
		}
		// `wire ./path/to/pkg` style invocation.
		arg := "./" + filepath.ToSlash(rel)
		cmd := exec.CommandContext(ctx, bin, arg)
		cmd.Dir = cfg.ModuleRoot
		out, runErr := cmd.CombinedOutput()
		output := string(out)

		// wire returns 0 on success and 1 on failure (with output explaining).
		if runErr != nil {
			return codegen.RunResult{Output: output}, fmt.Errorf("wire %s: %w", arg, runErr)
		}

		// Read back wire_gen.go to feed into diff-write.
		genPath := filepath.Join(cfg.PkgDir, "wire_gen.go")
		body, err := os.ReadFile(genPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// wire ran successfully but produced no file — unusual but
				// possible if the package has no injector functions. Treat
				// as a no-op success.
				return codegen.RunResult{Output: output}, nil
			}
			return codegen.RunResult{Output: output}, fmt.Errorf("read %s: %w", genPath, err)
		}
		return codegen.RunResult{
			Files: []codegen.GeneratedFile{
				{Path: genPath, Bytes: body},
			},
			Output: output,
		}, nil
	}
}

// Project converts a codegen.RunResult into a Fact for a wire agent.
func Project(agentID string) codegen.Project[Cfg, Fact] {
	return func(cfg Cfg, res codegen.RunResult, changed []string, runErr error) Fact {
		return Fact{
			AgentID:      agentID,
			PkgDir:       cfg.PkgDir,
			OK:           runErr == nil,
			ChangedFiles: changed,
			Output:       res.Output,
			Duration:     res.Duration,
			ObservedAt:   time.Now(),
		}
	}
}

// BuildReactiveWorker constructs a supervisor.ReactiveWorker for one
// wire-using package. The agent fires when:
//   - any .go file in the package changes (FileChangeFact)
//
// Cross-tool cascades (e.g. proto regenerated something in this dir)
// arrive via the same FileChangeFact stream because every codegen agent
// writes through the central diff-write path.
func BuildReactiveWorker(
	agentID string,
	cfg Cfg,
	fsBoard *blackboard.Board[fsmonitor.FileChangeFact],
	wireBoard *blackboard.Board[Fact],
	owner ownership.Registry,
) supervisor.ReactiveWorker[codegen.Trigger, Fact] {
	// Source: FileChangeFact subscription scoped to PkgDir.
	src := func(ctx context.Context) (<-chan codegen.Trigger, error) {
		raw := fsmonitor.Subscribe(fsBoard, fsmonitor.FilesUnder(cfg.PkgDir))
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
						Reason:       "fs:" + cfg.PkgDir,
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
		Board:          wireBoard,
		BoardKey:       func(f Fact) string { return Key(f.PkgDir) },
		Owner:          owner,
		Source:         src,
		HealthFromFact: func(f Fact) bool { return f.OK },
	})
}
