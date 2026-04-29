// Package makefileagent is a wake-once bootstrap agent for a starter
// Makefile. The user gets a canonical, batteries-included Makefile so
// they have something to run rather than staring at an empty file.
//
// Like the other bootstrap agents, this is one-shot: wake at startup,
// propose, go dormant. The user accepts or declines via y/N.
package makefileagent

import (
	"context"
	"fmt"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

const Role = `You are the makefile-bootstrap agent. Your job is to propose a starter Makefile for a Go project so the user can type 'make' and have something useful happen. Batteries-included — the canonical targets most Go projects have.

You wake once at coven startup, propose, then go dormant. The user is the final authority via y/N — propose what you think is a good starter; if the user declines, they decline.

Your method:
  1. Survey: read go.mod for the module path. List cmd/*/ to find binary entry points. Check for buf.yaml, sqlc.yaml, wire.go anywhere — these imply codegen targets in the Makefile. If any existing Makefile is present, read it so you can mention what you saw in your final summary.
  2. Propose a Makefile with the canonical targets most Go projects have. The exact target list depends on what the project actually has, but typically includes:
     - help (default; lists targets via grep on the Makefile itself)
     - build (go build for each cmd/*)
     - run (go run, or runs the primary built binary)
     - test (go test ./... -race)
     - vet (go vet ./...)
     - tidy (go mod tidy)
     - fmt (gofmt or goimports)
     - clean (rm bin/)
     - generate (only if the project has codegen — protos, sqlc, wire)
     - docker-build / docker-up (only if a Dockerfile / compose exists)
  3. Use canonical Make conventions: .PHONY for non-file targets, BIN_DIR variable for build output, simple recipes.
  4. The Makefile body should be readable, not clever. Avoid recursive make. Avoid shell tricks beyond what GNU make handles natively.
  5. Write the Makefile via exec heredoc so the full content is visible in the y/N prompt.
  6. After write, run 'make help' (or 'make -n build') to verify the file parses correctly.

Constraints — soft, the user has final say on edge cases:
  - Don't add targets for tools the project doesn't use. No protoc target if the project uses buf. No wire target if no wire.go exists.
  - Don't fabricate paths. If you can't tell what cmd/* to default 'run' to, pick one and note it in the summary.
  - Single Makefile at project root.

Final summary: list the targets you included and why, what you defaulted to (e.g. 'run' targets cmd/server). Mention any existing Makefile you observed.`

type Config struct {
	ID         string
	ModuleRoot string
	Sender     llm.Sender
	Confirm    agent.ConfirmFunc
	Print      agent.PrintFunc
}

type Agent struct {
	cfg   Config
	inner *agent.Agent
}

func New(cfg Config) *Agent {
	if cfg.ID == "" {
		cfg.ID = "makefile-agent"
	}
	inner := agent.New(agent.Config{
		ID:       cfg.ID,
		Role:     Role,
		Tools:    agent.StandardTools(cfg.ModuleRoot),
		Sender:   cfg.Sender,
		Confirm:  cfg.Confirm,
		Print:    cfg.Print,
		MaxTurns: 30,
	})
	return &Agent{cfg: cfg, inner: inner}
}

func (a *Agent) Run(ctx context.Context) error {
	a.cfg.Print(fmt.Sprintf("\n  [%s] waking: makefile bootstrap survey\n", a.cfg.ID))

	observation := `startup: I am the makefile-bootstrap agent. Survey the project: read go.mod, list cmd/*/ for entry points, check for buf.yaml/sqlc.yaml/wire.go to know what codegen targets are needed, check for Dockerfile/docker-compose for docker-* targets. Propose a canonical batteries-included Makefile with help/build/run/test/vet/tidy/fmt/clean and codegen/docker targets when applicable. The user accepts or declines.`

	if _, err := a.inner.Wake(ctx, observation); err != nil {
		a.cfg.Print(fmt.Sprintf("  [%s] error: %v\n", a.cfg.ID, err))
	}

	a.cfg.Print(fmt.Sprintf("  [%s] dormant — restart coven with -bootstrap-make to re-run\n", a.cfg.ID))

	<-ctx.Done()
	return nil
}

func (a *Agent) HistoryLen() int { return a.inner.HistoryLen() }

// init registers this agent. Always relevant — every Go project can
// benefit from a starter Makefile.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "make",
		Description: "starter Makefile with canonical Go targets",
		Role:        Role,
		TypicalTriggers: "Project missing Makefile, or existing Makefile lacks targets for the project's actual toolchain (e.g., has no buf-generate target despite buf.yaml present).",
		DomainFiles:     "Makefile.",
		AvoidsWhen:      "Skip if Makefile already exists with appropriate targets. Don't run for Go source changes, schema changes, or test edits.",
		ExampleScenarios: "On a new project with buf, sqlc, and wire all present, this agent proposes a Makefile with help, generate (calling buf-generate, sqlc-generate, wire-generate), build, run, test, vet, fmt, tidy, clean targets.",
		Detect: func(goMod string, moduleRoot string) bool {
			return goMod != ""
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "makefile-agent",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
			})
		},
	})
}
