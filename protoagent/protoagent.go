// Package protoagent is the conversational agent for protobuf-generated
// Go code. Its job is to keep generated .pb.go (or equivalent) files
// consistent with .proto sources.
//
// This is a thin wrapper around agent.Agent. The interesting parts:
//   - Role: tells Claude what this agent's domain is and how to reason
//     about it. Hardcoded; never sent through user input.
//   - Domain: a predicate over file paths. The agent wakes when an
//     observed file change matches.
//   - Wakeup loop: subscribes to fsmonitor.FileChangeFact, formats the
//     observation as a user message, hands it to agent.Agent.Wake.
package protoagent

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

// Role is the system prompt that primes Claude on this agent's job.
// Deliberately specific about what this agent owns (proto/Go codegen)
// and what it doesn't (the rest of the project).
const Role = `You are the proto agent for a Go project. Your single job is to keep generated Go code from .proto files consistent and current.

The project may use any of these toolchains, possibly none:
  - buf (buf.yaml + 'buf generate')
  - protoc directly with protoc-gen-go
  - no proto tooling at all (rare; just observe and report)

Your method, in order:
  1. When you wake, identify the toolchain by reading buf.yaml, buf.gen.yaml, or relevant Makefile/script files.
  2. If you can't identify a toolchain, look for .proto files first; if there are none, the project has no proto domain — say so plainly and stop.
  3. If buf.gen.yaml exists, check whether the plugins it lists match what go.mod actually uses:
       - go.mod has google.golang.org/grpc → buf.gen.yaml should include protoc-gen-go-grpc.
       - go.mod has connectrpc.com/connect → buf.gen.yaml should include protoc-gen-connect-go.
     If a plugin is missing from buf.gen.yaml that the project's deps imply is needed, propose adding it (single-file edit to buf.gen.yaml, auditable via heredoc). The user's y/N gate decides; this is a suggestion, not an imposition.
  4. Once you know the toolchain, propose the appropriate regeneration command (typically 'buf generate'). Provide a one-sentence reason.
  5. After regeneration, verify the generated files exist and the project compiles in your domain (you may run 'go build ./...' as a sanity check, but don't fix non-proto build errors — those are someone else's domain).

Constraints:
  - You do NOT edit .proto files yourself. Schema changes are human decisions.
  - You do NOT write to wire_gen.go, sqlc generated files, or any non-proto generated code.
  - You DO own buf.gen.yaml (it controls proto codegen) — proposing additions to it is allowed when justified by go.mod deps.
  - You do NOT alter go.mod beyond what is required to add the protobuf runtime (e.g. 'go get google.golang.org/protobuf' is fine if the runtime is missing).
  - When you've reached a consistent state in your domain, return one short final message describing what you did. That signals equilibrium; you'll wake again on the next relevant file change.`

// Config configures a protoagent.
type Config struct {
	ID         string                              // stable agent ID
	ModuleRoot string                              // project root
	Sender     llm.Sender                          // backend
	FSBoard    *blackboard.Board[fsmonitor.FileChangeFact] // central fsnotify
	Confirm    agent.ConfirmFunc                   // y/N gate for exec
	Print      agent.PrintFunc                     // user output
	Settle     time.Duration                       // wait this long after a burst before waking; default 500ms
}

// Agent is the long-running protoagent.
type Agent struct {
	cfg   Config
	inner *agent.Agent

	mu      sync.Mutex
	pending bool          // a wake is queued; coalesce
	last    time.Time     // last wake-up time (for rate limiting)
}

// New constructs a protoagent.
func New(cfg Config) *Agent {
	if cfg.ID == "" {
		cfg.ID = "proto-agent"
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 500 * time.Millisecond
	}
	inner := agent.New(agent.Config{
		ID:      cfg.ID,
		Role:    Role,
		Tools:   agent.StandardTools(cfg.ModuleRoot),
		Sender:  cfg.Sender,
		Confirm: cfg.Confirm,
		Print:   cfg.Print,
	})
	return &Agent{cfg: cfg, inner: inner}
}

// IsRelevant reports whether a file path is in the proto agent's domain.
// Currently: any *.proto file, buf config files, or files under proto/.
func IsRelevant(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".proto") {
		return true
	}
	if base == "buf.yaml" || base == "buf.yml" || base == "buf.gen.yaml" || base == "buf.gen.yml" || base == "buf.lock" {
		return true
	}
	// Treat anything in a proto/ directory as relevant — covers the
	// case where someone moves a non-.proto file into the domain.
	slash := filepath.ToSlash(path)
	if strings.Contains(slash, "/proto/") || strings.HasPrefix(slash, "proto/") {
		return true
	}
	return false
}

// Run starts the agent's subscription loop. It blocks until ctx is
// cancelled. Wakes on relevant FileChangeFacts (debounced by Settle),
// runs one Wake call per debounced burst.
//
// On startup, it triggers an initial wake with observation "startup" so
// the agent can introspect the project state and bring it to equilibrium
// (this is the bootstrap case: empty module → propose buf generate).
func (a *Agent) Run(ctx context.Context) error {
	// Initial bootstrap wake.
	go a.wakeOnce(ctx, "startup: just attached. Survey the project and bring the proto domain to a healthy state.")

	if a.cfg.FSBoard == nil {
		// No fs board; agent runs only on bootstrap. That's a valid mode.
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

	// Debounce loop: collect changes, wake after Settle of quiet.
	var pendingFiles []string
	var timer *time.Timer
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(a.cfg.Settle, func() {
			files := pendingFiles
			pendingFiles = nil
			obs := formatObservation(files)
			a.wakeOnce(ctx, obs)
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
	// Coalesce: if a wake is already in progress, drop this one and let
	// the in-progress wake's terminal message cover it. The next file
	// change will trigger another wake if needed.
	a.mu.Lock()
	if a.pending {
		a.mu.Unlock()
		return
	}
	a.pending = true
	a.last = time.Now()
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.pending = false
		a.mu.Unlock()
	}()

	a.cfg.Print(fmt.Sprintf("\n  [%s] waking: %s\n", a.cfg.ID, truncate(observation, 80)))
	_, err := a.inner.Wake(ctx, observation)
	if err != nil {
		a.cfg.Print(fmt.Sprintf("  [%s] error: %v\n", a.cfg.ID, err))
	}
}

// HistoryLen exposes the inner agent's history length for tests.
func (a *Agent) HistoryLen() int { return a.inner.HistoryLen() }

func formatObservation(files []string) string {
	if len(files) == 0 {
		return "filesystem activity in your domain (no specific files identified)."
	}
	// Dedupe and sort for stable observations.
	seen := make(map[string]bool)
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

// init registers proto-agent with the v2 router. v1 cmd/demo wires this
// agent up via the -conv-proto flag directly, so registering here is
// purely so the v2 router has metadata for routing decisions.
func init() {
	registry.Register(registry.AgentSpec{
		Name:        "proto",
		Description: "regenerate proto Go bindings via buf and keep buf.gen.yaml aligned with go.mod deps",
		TypicalTriggers: "Changes to .proto files, buf.yaml, or buf.gen.yaml. Also new entries in go.mod that imply new buf plugins should be configured (connectrpc.com/connect → protoc-gen-connect-go).",
		DomainFiles:     "buf.gen.yaml, gen/<package>/v1/*.pb.go, gen/<package>/v1/*_grpc.pb.go (and connect output if configured).",
		AvoidsWhen:      "Skip if no .proto files exist or all generated files are current. Skip changes purely in test files or non-proto source code.",
		ExampleScenarios: "When proto/orders/v1/order.proto adds a new RPC, this agent runs buf generate to update gen/orders/v1/order.pb.go and order_grpc.pb.go (and connect bindings if configured).",
		Detect: func(goMod string, moduleRoot string) bool {
			// Active when proto/buf signal exists.
			return registry.HasDep(goMod, "google.golang.org/protobuf") ||
				registry.HasDep(goMod, "google.golang.org/grpc") ||
				registry.HasDep(goMod, "connectrpc.com/connect")
		},
		Build: func(deps registry.BuildDeps) registry.Runner {
			return New(Config{
				ID:         "proto-agent",
				ModuleRoot: deps.ModuleRoot,
				Sender:     deps.Sender,
				FSBoard:    deps.FSBoard,
				Confirm:    deps.Confirm,
				Print:      deps.Print,
				Settle:     1 * time.Second,
			})
		},
	})
}
