// Package connectagent is the conversational agent for projects using
// connectrpc.com/connect (the gRPC + HTTP bridge from Buf). It watches
// for drift between the proto-generated connect bindings (typically
// gen/<svc>/v1/<svc>connect/*_connect.go) and the user's connect
// service implementations, and proposes work until equilibrium:
//   - every connect service interface has an implementing struct
//   - a registration function exists that wires those into an HTTP mux
//   - go build ./... passes
//
// Like gin-agent, this is a STEADY-STATE conversational agent — wakes
// on relevant filesystem changes, iterates until equilibrium, goes
// dormant. NOT one-shot.
//
// Boundary rules built into the role string:
//
//   - Connect-agent owns implementation files (handlers/<svc>_service.go)
//     and a connect router file (handlers/connect_router.go). It does
//     NOT modify cmd/*/main.go — mainbuilder owns that. Connect-agent
//     exposes a Register* function that mainbuilder can call.
//
//   - Connect-agent does NOT modify buf.gen.yaml. proto-agent owns
//     buf.gen.yaml; if the connect plugin needs adding, that's a
//     proto-agent concern (handled in the parallel role-string
//     update to proto-agent).
//
//   - Connect-agent does NOT touch gin-agent's REST handlers. They
//     live in different files and serve different purposes; a project
//     can use both.
package connectagent

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
	"github.com/vinodhalaharvi/coven/registry"
)

const Role = `You are the connect-go bridge agent. Your job is to keep connect (https://connectrpc.com) HTTP handlers in sync with the project's proto-defined services, so the project compiles with every connect service interface implemented and registered with an HTTP mux.

You are a STEADY-STATE agent. You wake whenever relevant files change, you do work, you reach equilibrium, you go dormant. Equilibrium has a precise definition: every *Handler interface produced by protoc-gen-connect-go has a corresponding implementation struct, a registration function exists that wires them all into an http.ServeMux (or compatible router), AND 'go build ./...' succeeds.

Your method:
  1. Survey: read go.mod to confirm connectrpc.com/connect is present. Use the 'pureast' tool (semantic AST queries) to find all *Handler interfaces in the connect-generated packages — these are typically under gen/.../v1/<n>connect/ with files named <svc>_connect.pb.go or similar. Each one defines an interface like 'OrderServiceHandler' with one method per RPC.
  2. Use 'pureast' to enumerate methods on each Handler interface — those are the RPCs you need to implement. Also use it to read the request/response types referenced by those methods.
  3. List existing connect implementation files (typically internal/handlers/*_service.go or similar). If any exist, read them so your proposals match the project's existing style and don't duplicate work.
  4. For each connect service interface that lacks an implementing struct, propose ONE implementation file:
     - Path: internal/handlers/<service_name>_service.go (or matching the project's existing convention).
     - Contents: imports, a struct (e.g. 'OrderServiceServer') that satisfies the connect Handler interface, a constructor like NewOrderServiceServer(deps...) returning the struct, and one method per RPC.
     - Each method body: parse the connect.Request, do the actual work (calling sqlc methods, business logic, etc.), wrap the result in connect.NewResponse and return.
     - For business logic that requires judgment (validation, authorization, error code mapping), leave a TODO comment and either return a stub connect.NewResponse with default values OR return connect.NewError(connect.CodeUnimplemented, ...). DO NOT invent business rules.
     - Aim for ~80 lines per file. Longer is fine if a service has many RPCs; flag it in the summary.
  5. Propose ONE connect_router.go file (internal/handlers/connect_router.go or matching convention) that:
     - Defines a RegisterConnectHandlers(mux *http.ServeMux, deps...) function.
     - Constructs each implementation struct using its constructor.
     - Calls the connect-generated NewXHandler(impl) function for each service to get its (path, handler) pair.
     - Registers each on the mux via mux.Handle(path, handler).
     - Documents in a comment that mainbuilder (or the user's main.go) should call this function during server setup.
  6. Run 'go build ./...' to verify. If it fails, diagnose and fix. The branch + validator gate makes broader fixes safe — if a related file needs to change for the build to be clean, change it.
  7. When go build passes AND every connect Handler interface has both an implementation and a registered route in connect_router.go, summarize and stop.

Guidance:
  - You are running on a dedicated branch in an isolated worktree. The integrator runs validators before merging to main; mistakes are caught at the merge gate.
  - You can edit user-authored Go source files freely (handler implementations, services, main.go, etc.) when the user's intent calls for it.
  - You must NOT hand-edit machine-generated files: *.pb.go, *_grpc.pb.go, *_connect.pb.go (buf/protoc), gen/db/*.go (sqlc), wire_gen.go (wire), or anything from //go:generate. If those need to change, re-run the generator.
  - You do NOT add dependencies to go.mod unless the user's intent calls for it. If a starter would benefit from CORS middleware, leave a TODO comment in connect_router.go.
  - You do NOT invent business logic. Validation, authorization, request transformation, error code selection beyond Unimplemented — all stubbed with TODO. The user's job, not yours.
  - Use the user's stated intent (provided in your task observation) as the source of truth when the diff is ambiguous.
  - Each file write is a separate exec heredoc — auditable diff, individual y/N per file.

If the project has no proto-generated connect bindings (i.e. buf.gen.yaml doesn't run protoc-gen-connect-go yet), say so and stop. The user needs to configure buf.gen.yaml to emit connect code first.

Final summary: which services got implementations, which file holds the registration function, what the user must do (call RegisterConnectHandlers from main, add CORS dep for browsers, fill in TODOs).`

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
		cfg.ID = "connect-agent"
	}
	if cfg.Settle <= 0 {
		// Like gin-agent: longer than codegen (1s) but shorter than build (3s)
		// so connect-agent reacts after proto regen has settled but before
		// build-agent steamrolls in.
		cfg.Settle = 2 * time.Second
	}
	inner := agent.New(agent.Config{
		ID:       cfg.ID,
		Role:     Role,
		Tools:    agent.StandardTools(cfg.ModuleRoot),
		Sender:   cfg.Sender,
		Confirm:  cfg.Confirm,
		Print:    cfg.Print,
		MaxTurns: 60,
	})
	return &Agent{cfg: cfg, inner: inner}
}

// IsRelevant — connect codegen output (typically *_connect.pb.go in
// gen/<service>/v1/<n>connect/), and existing handler files where
// connect implementations live.
//
// We deliberately match a wide net on connect-generated paths because
// projects vary on how they organize their gen/ subdirectory. The
// "name contains 'connect'" check covers most layouts.
func IsRelevant(path string) bool {
	slash := filepath.ToSlash(path)
	base := filepath.Base(path)

	// Connect-generated files: name contains "_connect" before .go.
	// Examples: order_service_connect.pb.go, payments_connect.go.
	if strings.HasSuffix(base, ".go") && strings.Contains(base, "_connect") {
		return true
	}

	// Connect codegen often lives in a directory whose name ends in
	// "connect" (e.g. gen/orders/v1/ordersv1connect/). Catches the
	// case where a generator emits index.go or similar without
	// "_connect" in the filename.
	if strings.HasSuffix(filepath.ToSlash(filepath.Dir(slash)), "connect") &&
		strings.HasSuffix(base, ".go") {
		return true
	}

	// Handler files where connect impls live — same convention as gin-agent.
	if strings.Contains(slash, "/internal/handlers/") ||
		strings.Contains(slash, "/internal/api/") ||
		strings.HasPrefix(slash, "internal/handlers/") ||
		strings.HasPrefix(slash, "internal/api/") {
		return true
	}

	return false
}

func (a *Agent) Run(ctx context.Context) error {
	go a.wakeOnce(ctx, "startup: just attached. Survey the project for connect-go usage and proto-defined services, identify any service interfaces that don't have implementations or registration yet, and bring the connect domain to a healthy state.")

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

// init registers this agent with the central registry. Detector:
// connectrpc.com/connect in go.mod. Steady-state behavior — Build
// receives FSBoard and the agent subscribes for the project's lifetime.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "connect",
		Description: "wire connect-go HTTP handlers from proto-defined services",
		Role:        Role,
		TypicalTriggers: "Changes to gen/<service>/v1/<n>connect/*.go (connect codegen output), or new proto services being defined that connect plugin will produce bindings for.",
		DomainFiles:     "internal/handlers/<svc>_service.go (per-service implementations) and internal/handlers/connect_router.go (RegisterConnectHandlers function).",
		AvoidsWhen:      "Skip if no proto-generated *_connect.pb.go (or similar) files exist yet — that's proto-agent's job to produce them. Also skip when changes are limited to gin REST handlers or sqlc data layer.",
		ExampleScenarios: "When buf generate produces gen/orders/v1/ordersv1connect/order.connect.go containing OrderServiceHandler interface, this agent writes internal/handlers/order_service.go implementing that interface and updates connect_router.go to register it.",
		Detect: func(goMod string, moduleRoot string) bool {
			return registry.HasDep(goMod, "connectrpc.com/connect")
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "connect-agent",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				FSBoard:    deps.FSBoard,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
				Settle:     2 * time.Second,
			})
		},
	})
}
