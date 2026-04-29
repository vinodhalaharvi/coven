// Package controlplane is the v2 orchestration root.
//
// v2 architecture (see docs/v2-architecture.svg):
//
//	fsnotify → debounce → git diff → LLM router → parallel agents in
//	worktrees → serial integrator with repair loop → merge into main.
//
// The package hosts:
//
//   - Router (router.go): takes a git diff, asks Claude which agents
//     should handle it.
//   - WorktreeMgr (worktree.go): provisions/cleans up git worktrees per
//     agent invocation.
//   - AllowList (allowlist.go): policy gate that auto-approves routine
//     exec calls and falls through to user confirmation otherwise.
//   - TaskRunner (task.go): adapter that runs an agent against a
//     Task in its dedicated worktree.
//   - Integrator (integrator.go): serial merge queue, runs validators,
//     handles repair loop.
//   - Validators (validators.go): per-extension validator commands
//     (go build/vet/test, etc.).
//   - Repair (repair.go): LLM-mediated repair for merge conflicts and
//     validator failures.
//   - Run (run.go): the orchestration loop that wires everything
//     together.
//
// Agents are reused from v1 (protoagent, sqlcagent, etc.). Their
// internal agent.Agent runtime stays the same. What changes is HOW
// they're invoked: v1 had each agent subscribe to fsmonitor and run as
// a long-lived goroutine. v2 invokes them via Tasks dispatched from
// the router, with their work scoped to a per-invocation worktree.
package controlplane

import (
	"context"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/llm"
)

// ControlPlane is the top-level orchestrator. A program (cmd/coven)
// creates one instance, calls Run, and the control plane handles the
// full lifecycle: detecting changes, routing them to agents, running
// agents in worktrees, merging results.
//
// The interface is deliberately small. v1 had a sprawling cmd/demo
// with flag parsing, agent instantiation, fsmonitor setup, and stdin
// handling all inline. v2 hides all of that behind a single Run
// method. The cmd/ binary's job is just to construct a Config, call
// New, and Run.
type ControlPlane interface {
	// Run blocks until ctx is cancelled or an unrecoverable error
	// occurs. File changes detected during Run trigger the routing
	// pipeline.
	Run(ctx context.Context) error
}

// Config configures a ControlPlane instance.
//
// Required fields: ProjectRoot, Sender. Without these, New() returns
// a stub that does nothing useful.
//
// Optional fields all have sensible defaults — see withDefaults.
type Config struct {
	// ProjectRoot is the absolute path to the git repository v2
	// watches. fsnotify watches this recursively. Worktrees are
	// provisioned under <ProjectRoot>/.coven/worktrees/.
	ProjectRoot string

	// Sender is the Claude API client used by the router, repair
	// loops, and agent task runs. Required for any meaningful
	// operation.
	Sender llm.Sender

	// Confirm is the y/n prompt for non-allow-listed exec calls
	// inside agent worktrees, AND for the "merge to main?" prompt
	// at the integration step. Defaults to "always deny" (safe).
	Confirm agent.ConfirmFunc

	// Print is the user-facing output channel. Defaults to a no-op
	// — most callers want to set this to fmt.Print or a logger.
	Print agent.PrintFunc

	// Settle is the debounce window for file change events. Multiple
	// events within this window are coalesced into a single routing
	// decision. Default 2.5 seconds.
	Settle time.Duration

	// Validators (optional) overrides the default validator set
	// (go build/vet/test). Pass a custom registry to add proto/sql/
	// other validators, or to use a leaner set.
	Validators *ValidatorRegistry

	// EnableRepair turns on LLM-mediated repair for merge conflicts
	// and validator failures. Default false (off) — repair adds
	// LLM cost and is a best-effort optimization, not a correctness
	// primitive. Recommend enabling once basic v2 is proven.
	EnableRepair bool
}

// stub is a placeholder ControlPlane returned when Config doesn't
// have the minimum required fields. Run blocks until ctx is
// cancelled, then exits cleanly.
type stub struct{}

func (s *stub) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
