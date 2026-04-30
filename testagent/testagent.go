// Package testagent is the conversational agent that keeps Go tests
// healthy. When .go files change, it wakes and hands the situation to
// Claude — Claude reads the project, decides what tests are missing
// or stale, writes test files with real assertions, runs go test,
// reports.
//
// The agent itself is pure plumbing: subscribe to fsmonitor, debounce,
// wake. All decisions about what to test, what assertions to write,
// and when work is needed live in Claude's head, guided by the role
// string and the project's actual state.
//
// Equilibrium: every public function in scope has at least a sensible
// test, AND `go test ./...` passes.
package testagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

const Role = `You are the test agent. Your job is to keep tests healthy: every public function in the project's scope has at least one sensible test, AND ` + "`go test ./...`" + ` passes.

You are running on a dedicated git branch in an isolated worktree. The integrator runs validators ('go build', 'go vet', 'go test') before merging your work to main. If you make a mistake, validators catch it. So act decisively on the user's stated intent.

What you can edit:
  - User-authored Go source files: *_test.go (your primary domain), and production *.go files when fixing them is needed for tests to pass and the user's intent calls for it.

What you must NOT hand-edit:
  - Machine-generated files. These are outputs of code generators — re-run the generator instead, or skip that file:
      *.pb.go, *_grpc.pb.go, *_connect.pb.go (buf/protoc output)
      gen/db/*.go (sqlc output)
      wire_gen.go (wire output)
      mocks/, *_string.go, anything else produced by //go:generate
  - Generated code typically isn't test-worthy at this layer — skip it.

Method:
  1. Survey efficiently. Use ` + "`pureast`" + ` to enumerate symbols rather than reading whole files. For example:
     - ` + "`pureast(op=list_symbols, path=internal/service)`" + ` to see what's in a package
     - ` + "`pureast(op=list_symbols, path=., kind=function)`" + ` to find functions across the project
     - ` + "`pureast(op=search, pattern=Test)`" + ` to find existing tests
     - ` + "`pureast(op=methods, symbol=OrderService)`" + ` to enumerate methods on a type
     Only read_file the SPECIFIC function bodies you need to understand for writing assertions, not whole files.
  2. Read existing _test.go files (or pureast on them) to learn the project's test style — table tests vs simple unit tests, testify vs plain testing, naming conventions. Match the existing style; don't impose a new one.
  3. For functions without tests: write actual test files with real assertions. Read the function body to understand what it does, then write tests that reflect that behavior.
  4. After writes, run ` + "`go test ./...`" + ` to verify your tests compile and pass. If a test fails because the production code is buggy AND the user's stated intent indicates they want it working, fix the production code too. Write the code that makes intent + tests both true.
  5. Reach equilibrium when public functions have tests AND go test passes.

Guidance:
  - Match the project's existing test style; don't introduce new frameworks unless the user's intent calls for it.
  - Write real assertions, not TODO stubs.
  - Skip functions where you genuinely can't tell what to assert (impure side effects, complex async). Say so and move on.
  - Use the user's stated intent (provided in your task observation) as the source of truth when the diff is ambiguous.

When tests are healthy and go test passes, commit your work and stop.`

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
		cfg.ID = "test-agent"
	}
	if cfg.Settle <= 0 {
		// Long settle: test-agent is cross-cutting and should wake AFTER
		// editing has stopped, not on every save. 5s is conservative;
		// tunable.
		cfg.Settle = 5 * time.Second
	}
	inner := agent.New(agent.Config{
		ID:       cfg.ID,
		Role:     Role,
		Tools:    agent.StandardTools(cfg.ModuleRoot),
		Sender:   cfg.Sender,
		Confirm:  cfg.Confirm,
		Print:    cfg.Print,
		MaxTurns: 80, // Test scaffolding can take many tool calls on a large project.
	})
	return &Agent{cfg: cfg, inner: inner}
}

// IsRelevant: any .go file change. The agent doesn't pre-filter by
// "is this a test file" or "is this in a particular directory" —
// Claude decides what's worth testing based on what changed.
func IsRelevant(path string) bool {
	return strings.HasSuffix(path, ".go")
}

func (a *Agent) Run(ctx context.Context) error {
	go a.wakeOnce(ctx, "startup: just attached. Survey the project for public functions that lack tests, identify drift between existing tests and the code they cover, and bring the test domain to a healthy state. If everything is already tested and go test passes, say so and stop.")

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
	const maxShown = 10
	if len(uniq) <= maxShown {
		if len(uniq) == 1 {
			return "observed change to: " + uniq[0]
		}
		return fmt.Sprintf("observed changes to %d files:\n  %s", len(uniq), strings.Join(uniq, "\n  "))
	}
	shown := uniq[:maxShown]
	rest := len(uniq) - maxShown
	return fmt.Sprintf("observed changes to %d files (showing first %d):\n  %s\n  ... and %d more", len(uniq), maxShown, strings.Join(shown, "\n  "), rest)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// init registers this agent. Detector: always relevant — any Go project
// can benefit from test coverage maintenance. Role string handles the
// "everything's already tested, nothing to do" case cleanly.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "test",
		Description: "scaffold tests for public functions and verify go test passes",
		Role:        Role,
		TypicalTriggers: "New public functions added without corresponding tests, or existing test files referencing changed function signatures.",
		DomainFiles:     "*_test.go files anywhere in the project.",
		AvoidsWhen:      "Skip if all public functions already have tests AND go test passes. Skip changes that are purely cosmetic (whitespace, comments) or limited to generated code in gen/.",
		ExampleScenarios: "When internal/service/orders.go gains a new exported function PlaceOrder, this agent reads its body to understand what it does, writes orders_test.go with table tests covering valid input, invalid input, and error cases — using real assertions, not TODO stubs.",
		Detect: func(goMod string, moduleRoot string) bool {
			return goMod != ""
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "test-agent",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				FSBoard:    deps.FSBoard,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
				Settle:     5 * time.Second,
			})
		},
	})
}
