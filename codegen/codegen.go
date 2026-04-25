// Package codegen provides a generic agent for code-generating tools
// (buf, wire, sqlc, mockgen, ent, ...). It captures the shape we
// observed in protogen and lets new tools slot in by supplying just a
// Runner function and a fact projector.
//
// The agent is a Worker[Trigger, Fact] in the supervisor's algebra.
// Its Source comes from one of:
//   - the central fsmonitor (subscribe to FileChangeFact, filter)
//   - any other blackboard via BlackboardSource (for fact-driven agents
//     like a tool that reacts to upstream regenerations)
//   - a merge of the two via sources.MergeSources
//
// The agent's Program is a Free applicative composition:
//
//   filter (FlatMap break-out)
//     → run (Lift)
//     → diff-write outputs (Lift)
//     → claim ownership (Lift)
//     → project to Fact + post (Lift)
//
// FlatMap is used only at filter (early-out for irrelevant triggers)
// and after run (skip the rest if nothing was produced). All other
// steps are Lift, preserving the ability to analyze the agent's plan
// statically via freeap.NodeDesc.
package codegen

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/ownership"
)

// GeneratedFile is one output of a codegen run. Path is absolute.
type GeneratedFile struct {
	Path  string
	Bytes []byte
}

// RunResult is everything a Runner produces in one invocation.
type RunResult struct {
	Files    []GeneratedFile
	Output   string        // captured stdout/stderr; for diagnostics
	Duration time.Duration // wall time for the runner itself
}

// runOutcome carries both the result and the runner's error through the
// FlatMap chain. The Free applicative's FlatMap can't carry an error
// alongside a successful continuation — so we tuck the error into the
// outcome and let the projector decide whether the agent is healthy.
type runOutcome struct {
	Result RunResult
	Err    error
}

// Runner is the function-typed seam over the actual generator. It
// receives the agent's Cfg and returns the produced files plus
// diagnostic output. An error here means "the runner failed to
// invoke" — it is NOT used to signal "the runner produced no files",
// which is a healthy outcome that just means nothing changed.
type Runner[Cfg any] func(ctx context.Context, cfg Cfg) (RunResult, error)

// Project converts a RunResult plus context into the agent's Fact type.
// Projectors have access to ChangedFiles (which files actually got
// written this run, after diff-write filtering) so the fact can be
// minimal/thin without losing causal information.
//
// runErr is non-nil iff the Runner itself failed; the projector should
// produce an unhealthy fact in that case (and changedFiles will be
// empty).
type Project[Cfg, Fact any] func(cfg Cfg, result RunResult, changedFiles []string, runErr error) Fact

// Trigger is the agent's wake-up event. Trigger filtering is the
// caller's responsibility (typically via fsmonitor.Subscribe with a
// filter, or BlackboardSource with a typed projection).
type Trigger struct {
	Reason       string    // free-form: "fs:proto edit", "blackboard:proto-fact", ...
	ChangedFiles []string  // hint; may be empty when triggered by fact rather than file
	At           time.Time
}

// Config configures a codegen.Agent.
type Config[Cfg, Fact any] struct {
	AgentID  string
	Cfg      Cfg
	Runner   Runner[Cfg]
	Project  Project[Cfg, Fact]
	Board    *blackboard.Board[Fact]
	BoardKey func(Fact) string // how to key the published fact; required
	Owner    ownership.Registry
	Source   func(ctx context.Context) (<-chan Trigger, error)
	// HealthFromFact reports whether a Fact represents a healthy state.
	// Used by the supervisor's Report. Default: true.
	HealthFromFact func(Fact) bool
}

// BuildReactiveWorker assembles a supervisor.ReactiveWorker for this agent.
func BuildReactiveWorker[Cfg, Fact any](cfg Config[Cfg, Fact]) supervisor.ReactiveWorker[Trigger, Fact] {
	a := &agent[Cfg, Fact]{cfg: cfg}
	return supervisor.ReactiveWorker[Trigger, Fact]{
		ID:     cfg.AgentID,
		Source: cfg.Source,
		Handle: a.handle,
		Report: a.report,
	}
}

type agent[Cfg, Fact any] struct {
	cfg Config[Cfg, Fact]
}

// handle returns the Free applicative Program for one trigger.
// Composition shape:
//   FlatMap( Lift(run) , \outcome ->
//     FlatMap( Lift(diffWrite(outcome)) , \changed ->
//       Lift( claim+post(outcome, changed) ) ) )
func (a *agent[Cfg, Fact]) handle(t Trigger) freeap.Program[Fact] {
	cfg := a.cfg

	runOp := freeap.Lift(freeap.Op[runOutcome]{
		Name: "codegen.run:" + cfg.AgentID,
		Kind: freeap.KindIO,
		Run: func(ctx context.Context, w freeap.World) (runOutcome, error) {
			start := time.Now()
			res, err := cfg.Runner(ctx, cfg.Cfg)
			res.Duration = time.Since(start)
			return runOutcome{Result: res, Err: err}, nil
		},
	})

	return freeap.FlatMap(runOp, func(outcome runOutcome) freeap.Program[Fact] {
		// If the Runner itself failed, skip diff-write entirely and post an
		// unhealthy fact directly. The projector decides what unhealthy
		// looks like for this Fact type.
		if outcome.Err != nil {
			return freeap.Lift(freeap.Op[Fact]{
				Name: "codegen.post-failure:" + cfg.AgentID,
				Kind: freeap.KindIO,
				Run: func(ctx context.Context, w freeap.World) (Fact, error) {
					fact := cfg.Project(cfg.Cfg, outcome.Result, nil, outcome.Err)
					if cfg.Board != nil && cfg.BoardKey != nil {
						cfg.Board.Post(cfg.BoardKey(fact), fact, cfg.AgentID)
					}
					return fact, nil
				},
			})
		}

		// Diff-write: compare each generated file's bytes against on-disk
		// bytes; keep only those that actually changed. This is the
		// cascade-breaker.
		diffWriteOp := freeap.Lift(freeap.Op[[]string]{
			Name: "codegen.diff-write:" + cfg.AgentID,
			Kind: freeap.KindIO,
			Run: func(ctx context.Context, w freeap.World) ([]string, error) {
				return diffWrite(outcome.Result.Files)
			},
		})

		return freeap.FlatMap(diffWriteOp, func(changed []string) freeap.Program[Fact] {
			// Claim ownership of every output (whether it changed this run
			// or not — an unchanged file is still ours).
			claimAndPost := freeap.Lift(freeap.Op[Fact]{
				Name: "codegen.claim+post:" + cfg.AgentID,
				Kind: freeap.KindIO,
				Run: func(ctx context.Context, w freeap.World) (Fact, error) {
					if cfg.Owner != nil {
						for _, gf := range outcome.Result.Files {
							cfg.Owner.Claim(gf.Path, ownership.AgentID(cfg.AgentID))
						}
					}
					fact := cfg.Project(cfg.Cfg, outcome.Result, changed, nil)
					if cfg.Board != nil && cfg.BoardKey != nil {
						cfg.Board.Post(cfg.BoardKey(fact), fact, cfg.AgentID)
					}
					return fact, nil
				},
			})
			return claimAndPost
		})
	})
}

func (a *agent[Cfg, Fact]) report(f Fact, err error) supervisor.Report {
	r := supervisor.Report{WorkerID: a.cfg.AgentID, At: time.Now()}
	if err != nil {
		r.OK = false
		r.Detail = "error: " + err.Error()
		return r
	}
	healthy := true
	if a.cfg.HealthFromFact != nil {
		healthy = a.cfg.HealthFromFact(f)
	}
	r.OK = healthy
	if healthy {
		r.Detail = "ok"
	} else {
		r.Detail = "unhealthy"
	}
	return r
}

// diffWrite writes each file iff its bytes differ from what's on disk.
// Returns the paths of files that were actually written. Files that
// already match disk are silently skipped (this is the cascade-breaker
// — generators like buf rewrite all outputs every run).
func diffWrite(files []GeneratedFile) ([]string, error) {
	changed := make([]string, 0, len(files))
	for _, gf := range files {
		existing, err := os.ReadFile(gf.Path)
		if err == nil && bytes.Equal(existing, gf.Bytes) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(gf.Path), 0755); err != nil {
			return changed, fmt.Errorf("mkdir for %s: %w", gf.Path, err)
		}
		if err := os.WriteFile(gf.Path, gf.Bytes, 0644); err != nil {
			return changed, fmt.Errorf("write %s: %w", gf.Path, err)
		}
		changed = append(changed, gf.Path)
	}
	return changed, nil
}
