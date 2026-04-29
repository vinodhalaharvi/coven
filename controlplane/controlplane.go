// Package controlplane is the v2 orchestration root.
//
// v2 architecture (see docs/v2-architecture.svg):
//
//	fsnotify → debounce → git diff → LLM router → parallel agents in
//	worktrees → serial integrator with repair loop → merge into main.
//
// This package will host the four major components:
//
//   - Router: takes a git diff, asks Claude which agents should handle it.
//   - WorktreeMgr: provisions/cleans up git worktrees per agent invocation.
//   - Integrator: serial merge queue, runs validators, handles repair loop.
//   - Validators: per-extension validator commands (go build/vet/test, etc.).
//
// Agents are reused from v1 (protoagent, sqlcagent, etc.). Their internal
// agent.Agent runtime stays the same. What changes is HOW they're invoked:
// v1 had each agent subscribe to fsmonitor and run as a long-lived goroutine.
// v2 invokes them via Tasks dispatched from the router, with their work
// scoped to a per-invocation worktree.
//
// This file establishes the top-level ControlPlane interface. Subsequent
// commits fill in the components.
package controlplane

import (
	"context"
)

// ControlPlane is the top-level orchestrator. A program (cmd/coven) creates
// one instance, calls Run, and the control plane handles the full lifecycle:
// detecting changes, routing them to agents, running agents in worktrees,
// merging results.
//
// The interface is deliberately small. v1 had a sprawling cmd/demo with
// flag parsing, agent instantiation, fsmonitor setup, and stdin handling
// all inline. v2 hides all of that behind a single Run method. The cmd/
// binary's job is just to construct a Config, call New, and Run.
type ControlPlane interface {
	// Run blocks until ctx is cancelled or an unrecoverable error occurs.
	// File changes detected during Run trigger the routing pipeline.
	Run(ctx context.Context) error
}

// Config configures a ControlPlane instance. Filled in as components land.
//
// Currently empty as a placeholder. Subsequent commits add fields:
//   - ModuleRoot string  // the repo to watch
//   - Sender llm.Sender  // for the router and any LLM-mediated repair
//   - Confirm agent.ConfirmFunc  // for out-of-allowlist exec confirmations
//   - Print agent.PrintFunc  // user-facing output
//   - Settle time.Duration  // debounce window (default 2-3s)
type Config struct{}

// New constructs a ControlPlane. Returns a stub implementation for now;
// real implementation lands as components are built.
func New(cfg Config) ControlPlane {
	return &stub{}
}

// stub is a placeholder ControlPlane that logs and exits cleanly. Lets us
// wire up cmd/coven and verify the integration shape before the real
// components exist.
type stub struct{}

func (s *stub) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
