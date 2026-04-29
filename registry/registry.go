// Package registry holds the catalog of bootstrap agents and the
// detection logic that decides which ones apply to a given project.
//
// Each agent that wants to participate in -bootstrap-auto registers
// itself via Register(), supplying:
//   - a Detect function that inspects go.mod (and optionally the
//     filesystem) to decide whether the agent is relevant
//   - a Build function that constructs the agent given shared deps
//
// cmd/demo, when run with -bootstrap-auto, reads go.mod once, asks
// Detect on every registered spec, and instantiates each agent whose
// detector returns true.
//
// This generalizes the "one agent per go.mod dependency" pattern: as
// projects pull in new tools (gqlgen, oapi-codegen, mockgen, gin, etc.)
// new agents register themselves and become eligible automatically.
package registry

import (
	"context"
	"strings"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/llm"
)

// Runner is the minimal interface a registered agent exposes: a Run
// method that the dispatcher launches in a goroutine. Concrete agents
// (gin-agent, mainbuilder, etc.) satisfy this naturally.
type Runner interface {
	Run(ctx context.Context) error
}

// BuildDeps are the shared dependencies passed to every agent factory.
// Both wake-once (mainbuilder, docker, make) and steady-state (gin,
// proto, sqlc, wire, build) agents receive the same struct — wake-once
// agents simply ignore FSBoard.
type BuildDeps struct {
	ModuleRoot string
	Sender     llm.Sender
	Confirm    agent.ConfirmFunc
	Print      agent.PrintFunc
	FSBoard    *blackboard.Board[fsmonitor.FileChangeFact]
}

// AgentSpec is the registry entry for one bootstrap agent.
type AgentSpec struct {
	// Name is the agent's identifier, used for logs and the explicit
	// -bootstrap-<name> flag.
	Name string

	// Description is a one-line summary shown in help output.
	Description string

	// Role is the system prompt fragment that primes Claude on this
	// agent's job. v2's task adapter uses this to construct a fresh
	// agent.Agent for each task invocation. v1 ignores it (each agent
	// package wires its own Role into agent.Config directly via Build).
	//
	// Empty Role is valid for agents that won't be invoked via v2's
	// task adapter — but currently every agent should set this so the
	// router can dispatch to it.
	Role string

	// --- v2 router context fields ---
	//
	// These give the v2 router (controlplane.Router) enough information
	// to decide whether to invoke this agent for a given diff. v1
	// ignores them entirely. v2 reads them when constructing the
	// router prompt.
	//
	// Empty values are valid; the router degrades gracefully on agents
	// that haven't been annotated yet (treats them as best-effort
	// candidates whenever Description matches the change). Filling
	// these in for each agent improves routing accuracy substantially.

	// TypicalTriggers describes what kinds of file changes typically
	// wake this agent. One sentence, listing concrete examples.
	// Example: "Changes to .proto files, buf.yaml, or buf.gen.yaml."
	TypicalTriggers string

	// DomainFiles lists the file paths or patterns this agent writes
	// or modifies. Helps the router avoid invoking agents that
	// wouldn't have anything to do.
	// Example: "internal/handlers/*_service.go, internal/handlers/connect_router.go"
	DomainFiles string

	// AvoidsWhen describes situations where this agent should NOT be
	// invoked, even if its TypicalTriggers seem to match. Helps the
	// router make negative decisions.
	// Example: "Skip if the only changes are inside test files."
	AvoidsWhen string

	// ExampleScenarios is one or two short examples of what this agent
	// does end-to-end. Concrete examples help the router calibrate
	// when its abstract description matches the situation.
	// Example: "When a new service is added to a .proto, this agent
	//   writes the corresponding handler stub in internal/handlers/."
	ExampleScenarios string

	// Detect inspects the go.mod content (full text) and returns true
	// if this agent is relevant to the project. May also inspect the
	// filesystem via moduleRoot for richer detection (e.g. presence of
	// .graphqls files), but should keep this fast — it's called for
	// every spec at startup.
	Detect func(goMod string, moduleRoot string) bool

	// Build constructs the agent from shared deps. Should not panic;
	// errors should be deferred to Run.
	Build func(BuildDeps) Runner
}

// registered is the package-private catalog. Specs are added at init
// time by each agent package's init() function.
var registered []AgentSpec

// Register adds a spec to the catalog. Idempotent on Name — if a spec
// with the same Name is already registered, it's replaced (useful for
// tests).
func Register(spec AgentSpec) {
	for i, s := range registered {
		if s.Name == spec.Name {
			registered[i] = spec
			return
		}
	}
	registered = append(registered, spec)
}

// All returns a copy of all registered specs in registration order.
// Tests use this; cmd/demo uses Detect.
func All() []AgentSpec {
	out := make([]AgentSpec, len(registered))
	copy(out, registered)
	return out
}

// Detect returns the subset of registered specs whose Detect function
// returns true for the given go.mod content and module root. Order is
// preserved from registration order.
func Detect(goMod string, moduleRoot string) []AgentSpec {
	var matched []AgentSpec
	for _, s := range registered {
		if s.Detect != nil && s.Detect(goMod, moduleRoot) {
			matched = append(matched, s)
		}
	}
	return matched
}

// ByName returns the spec with the given name, or nil if not found.
// Used by cmd/demo to handle explicit -bootstrap-<name> flags.
func ByName(name string) *AgentSpec {
	for i := range registered {
		if registered[i].Name == name {
			return &registered[i]
		}
	}
	return nil
}

// Reset clears the registry. Test helper only — not for production use.
func Reset() {
	registered = nil
}

// HasDep is a convenience helper for Detect implementations: returns
// true if the given module path appears anywhere in go.mod's content.
// Substring match — sufficient for the typical case where the path is
// distinctive enough that false positives are unlikely.
func HasDep(goMod, modulePath string) bool {
	return strings.Contains(goMod, modulePath)
}
