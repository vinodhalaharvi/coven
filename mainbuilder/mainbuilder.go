// Package mainbuilder is a one-shot bootstrap agent. Unlike the other
// agents in coven, it doesn't watch for filesystem changes — it wakes
// exactly once at coven startup, surveys the project, and proposes
// starter main.go files for entry points that are missing.
//
// After its initial wake completes, it goes dormant. Restart coven with
// -bootstrap to re-run it.
//
// The role string is the architecture: tells Claude to read go.mod for
// established conventions, observe how the project actually uses its
// dependencies, and write starters that match that style rather than
// imposing new conventions.
package mainbuilder

import (
	"context"
	"fmt"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

// Role is the system prompt that primes Claude on the bootstrap job.
// The constraints are deliberate: read go.mod, match existing style,
// don't invent business logic, write only main.go files, leave
// existing source alone.
const Role = `You are the main-builder agent. Your job is to propose starter main.go file(s) for a Go project so the user can run and test the application.

You wake once at coven startup. After your work is reviewed, you go dormant. You do NOT watch for file changes after your initial run.

Your method:
  1. Survey: read go.mod to identify the module path AND the dependencies the project already uses (logging, CLI parsing, config, DI, etc.). Read a sampling of existing .go files to see HOW those dependencies are used — what logger constructor, what flag style, what shutdown pattern. The project's existing conventions are the ground truth.
  2. Identify entry points: what package(s) need a main.go? Look for cmd/*/ directories that lack one. Look for wire injectors (InitX functions) that aren't being called anywhere. Look for service struct constructors at the project root that imply a main.
  3. For EACH missing main, propose a starter that:
     - Imports what already exists in the project (services, wire injectors, generated code).
     - Uses the project's established libraries — its logger, its flag parser, its DI tool — exactly as other code in the project uses them. Match the existing style; do not introduce new conventions.
     - Falls back to the standard library only if no convention is established yet (e.g. project has no logging anywhere → use log/slog).
     - Is a starter, not a finished application. Reasonable size. Includes graceful shutdown if the project demonstrates a server-like pattern; skip it if the project is a one-shot CLI.
  4. Write each main.go via exec. The command must be auditable: the user should see exactly what's being written before approving (use heredoc, not opaque generation).
  5. After all approved writes, run 'go build ./...' to verify the bootstrap result compiles.

Constraints:
  - You write ONLY new main.go files in cmd/<name>/ directories. You do NOT modify existing source files. You do NOT touch wire.go, .proto, .sql, schema, queries, or any generated file.
  - You do NOT add dependencies to go.mod. If a starter would need something not in go.mod, propose using a stdlib equivalent or report that the user needs to add the dep first.
  - You do NOT invent business logic. The main wires together what exists; if the existing types don't compose into a runnable program without invention, say so plainly.
  - You do NOT propose tests, configuration files, Dockerfiles, or anything beyond the main.go itself.
  - If the project already has a runnable main and 'go build ./...' produces a binary in cmd/*/, report this and stop. Do not "improve" working starters.

If you can't produce a useful starter — e.g. the project is a pure library, or its existing types don't compose into a runnable shape — say so in one short message and stop. The user writes their own main.

When the starter(s) are in place and the module compiles, return a short final summary and go dormant.`

// Config configures a mainbuilder.
//
// Note: unlike other agents, no FSBoard. Mainbuilder is wake-once and
// has no domain matcher. It runs at startup, goes dormant.
type Config struct {
	ID         string
	ModuleRoot string
	Sender     llm.Sender
	Confirm    agent.ConfirmFunc
	Print      agent.PrintFunc
}

// Agent is the wake-once bootstrap agent.
type Agent struct {
	cfg   Config
	inner *agent.Agent
}

// New constructs a mainbuilder.
func New(cfg Config) *Agent {
	if cfg.ID == "" {
		cfg.ID = "main-builder"
	}
	inner := agent.New(agent.Config{
		ID:      cfg.ID,
		Role:    Role,
		Tools:   agent.StandardTools(cfg.ModuleRoot),
		Sender:  cfg.Sender,
		Confirm: cfg.Confirm,
		Print:   cfg.Print,
		// Generous turn cap because the agent may need to read several
		// files to understand the project's conventions before writing.
		MaxTurns: 50,
	})
	return &Agent{cfg: cfg, inner: inner}
}

// Run is wake-once: it triggers a single Wake with the bootstrap
// observation and then blocks on ctx.Done. It does NOT subscribe to
// FileChangeFacts. After equilibrium, the agent stays alive (so its
// conversation history is preserved within the process) but takes no
// further action until coven restarts.
//
// This is the architectural distinction from the steady-state agents:
// bootstrap is a one-shot operation, not a maintenance loop.
func (a *Agent) Run(ctx context.Context) error {
	a.cfg.Print(fmt.Sprintf("\n  [%s] waking: bootstrap survey\n", a.cfg.ID))

	observation := `startup: I am the main-builder agent. Survey the project: read go.mod, observe what conventions already exist (logging, flag parsing, DI), identify entry points that need a starter main.go (cmd/*/ directories without one, or wire injectors that aren't being called from anywhere). Propose starter main.go files matching the project's existing style. After the starters are in place, verify with go build ./... and stop.`

	if _, err := a.inner.Wake(ctx, observation); err != nil {
		a.cfg.Print(fmt.Sprintf("  [%s] error: %v\n", a.cfg.ID, err))
	}

	a.cfg.Print(fmt.Sprintf("  [%s] dormant — restart coven with -bootstrap to re-run\n", a.cfg.ID))

	// Block until shutdown so cmd/demo's lifecycle treats us like other agents.
	<-ctx.Done()
	return nil
}

// HistoryLen exposes inner conversation length for tests.
func (a *Agent) HistoryLen() int { return a.inner.HistoryLen() }

// init registers this agent with the central registry. Always relevant —
// every Go project benefits from having a runnable main.go scaffold if
// it doesn't already.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "main",
		Description: "starter main.go for missing entry points",
		Role:        Role,
		TypicalTriggers: "Changes to wire_gen.go (new providers ready to be invoked from main), or absence of cmd/<n>/main.go when one would be expected.",
		DomainFiles:     "cmd/<n>/main.go for each entry point.",
		AvoidsWhen:      "Skip if all required main.go files already exist and compile. Skip changes purely to test files or generated code that doesn't affect entry-point structure.",
		ExampleScenarios: "When wire_gen.go appears with a new InitializeApp function, this agent writes cmd/server/main.go that calls it, parses flags, sets up an HTTP server, and runs.",
		Detect: func(goMod string, moduleRoot string) bool {
			// mainbuilder always applies — it'll abstain if a working
			// main already exists (per its role string).
			return goMod != ""
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "main-builder",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
			})
		},
	})
}
