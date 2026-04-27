// Demo CLI for coven: a multi-agent Go package monitor.
//
// Each flag below enables a conversational agent. They run independently,
// each owning its domain (proto, wire, sqlc, module-wide build) and
// each holding its own running Claude conversation.
//
// All agents share:
//   - one fsmonitor (single fsnotify owner, broadcasts FileChangeFact)
//   - one LLM sender (model selectable via -llm)
//   - one stdin confirmation channel (mutex-serialized so prompts don't interleave)
//
// Examples:
//
//	coven -root . -conv-proto                         # just proto codegen
//	coven -root . -conv-proto -conv-wire -conv-build  # full triad
//	coven -root . -conv-proto -conv-auto              # skip y/N (DEV ONLY)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/buildhealth"
	"github.com/vinodhalaharvi/coven/connectagent"
	"github.com/vinodhalaharvi/coven/dockeragent"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/ginagent"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/mainbuilder"
	"github.com/vinodhalaharvi/coven/makefileagent"
	"github.com/vinodhalaharvi/coven/protoagent"
	"github.com/vinodhalaharvi/coven/registry"
	"github.com/vinodhalaharvi/coven/sqlcagent"
	"github.com/vinodhalaharvi/coven/wireagent"
)

func main() {
	var (
		root      = flag.String("root", ".", "Go module root to watch")
		debounce  = flag.Duration("debounce", 200*time.Millisecond, "fsnotify debounce")
		llmModel  = flag.String("llm", "sonnet", "LLM model: haiku|sonnet|opus")
		convProto = flag.Bool("conv-proto", false, "enable conversational proto-agent")
		convWire  = flag.Bool("conv-wire", false, "enable conversational wire-agent")
		convSqlc  = flag.Bool("conv-sqlc", false, "enable conversational sqlc-agent")
		convBuild = flag.Bool("conv-build", false, "enable conversational build-health agent")
		bootstrap     = flag.Bool("bootstrap", false, "wake-once: propose starter main.go file(s) for missing entry points, then go dormant")
		bootstrapDock = flag.Bool("bootstrap-docker", false, "wake-once: propose Dockerfile + docker-compose.yaml + .dockerignore")
		bootstrapMake = flag.Bool("bootstrap-make", false, "wake-once: propose a starter Makefile with canonical Go-project targets")
		bootstrapGin     = flag.Bool("bootstrap-gin", false, "steady-state: keep gin handlers in sync with sqlc data layer")
		bootstrapConnect = flag.Bool("bootstrap-connect", false, "steady-state: keep connect-go HTTP handlers in sync with proto services")
		bootstrapAuto    = flag.Bool("bootstrap-auto", false, "auto-detect from go.mod: enable every registered agent whose detector fires")
		convAuto      = flag.Bool("conv-auto", false, "skip y/N confirmation prompts (DANGEROUS)")
		verbose   = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if !*convProto && !*convWire && !*convSqlc && !*convBuild && !*bootstrap && !*bootstrapDock && !*bootstrapMake && !*bootstrapGin && !*bootstrapConnect && !*bootstrapAuto {
		log.Error("no agents enabled. Try -bootstrap-auto, or specific flags like -conv-proto, -bootstrap, -bootstrap-gin, -bootstrap-connect")
		flag.Usage()
		os.Exit(1)
	}

	absRoot, err := absoluteRoot(*root)
	if err != nil {
		log.Error("resolving root", "err", err)
		os.Exit(1)
	}
	fmt.Printf("coven: watching %s\n", absRoot)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ─── Shared substrate ─────────────────────────────────────────────
	// One fsmonitor, one sender, one confirm — all conv agents share these.
	fsBoard := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{
		QuietFor: 1 * time.Second, Rounds: 3,
	})
	go func() {
		err := fsmonitor.Run(context.Background(), fsmonitor.Config{
			Root:     absRoot,
			Debounce: *debounce,
			Board:    fsBoard,
		})
		if err != nil {
			log.Error("fsmonitor exited", "err", err)
		}
	}()
	fmt.Printf("fsmonitor: watching %s (central FileChangeFact stream)\n", absRoot)

	sender := buildSender(*llmModel)
	confirm := makeStdinConfirm(*convAuto)
	printf := func(s string) { fmt.Print(s) }

	// ─── Agents ───────────────────────────────────────────────────────
	if *convProto {
		pa := protoagent.New(protoagent.Config{
			ID: "proto-agent", ModuleRoot: absRoot,
			Sender: sender, FSBoard: fsBoard,
			Confirm: confirm, Print: printf,
			Settle: 1 * time.Second,
		})
		go runAgent(ctx, log, "proto-agent", pa.Run)
		fmt.Printf("conv proto-agent: model=%s auto=%v\n", *llmModel, *convAuto)
	}

	if *convWire {
		wa := wireagent.New(wireagent.Config{
			ID: "wire-agent", ModuleRoot: absRoot,
			Sender: sender, FSBoard: fsBoard,
			Confirm: confirm, Print: printf,
			Settle: 1 * time.Second,
		})
		go runAgent(ctx, log, "wire-agent", wa.Run)
		fmt.Printf("conv wire-agent: model=%s auto=%v\n", *llmModel, *convAuto)
	}

	if *convSqlc {
		sa := sqlcagent.New(sqlcagent.Config{
			ID: "sqlc-agent", ModuleRoot: absRoot,
			Sender: sender, FSBoard: fsBoard,
			Confirm: confirm, Print: printf,
			Settle: 1 * time.Second,
		})
		go runAgent(ctx, log, "sqlc-agent", sa.Run)
		fmt.Printf("conv sqlc-agent: model=%s auto=%v\n", *llmModel, *convAuto)
	}

	if *convBuild {
		ba := buildhealth.New(buildhealth.Config{
			ID: "build-agent", ModuleRoot: absRoot,
			Sender: sender, FSBoard: fsBoard,
			Confirm: confirm, Print: printf,
			Settle: 3 * time.Second, // wakes after the codegen agents
		})
		go runAgent(ctx, log, "build-agent", ba.Run)
		fmt.Printf("conv build-agent: model=%s auto=%v\n", *llmModel, *convAuto)
	}

	if *bootstrap {
		// Wake-once bootstrap. Surveys the project, proposes starter
		// main.go for missing entry points, goes dormant after.
		mb := mainbuilder.New(mainbuilder.Config{
			ID: "main-builder", ModuleRoot: absRoot,
			Sender: sender, Confirm: confirm, Print: printf,
		})
		go runAgent(ctx, log, "main-builder", mb.Run)
		fmt.Printf("bootstrap main-builder: model=%s auto=%v (wakes once at startup)\n", *llmModel, *convAuto)
	}

	if *bootstrapDock {
		da := dockeragent.New(dockeragent.Config{
			ID: "docker-agent", ModuleRoot: absRoot,
			Sender: sender, Confirm: confirm, Print: printf,
		})
		go runAgent(ctx, log, "docker-agent", da.Run)
		fmt.Printf("bootstrap docker-agent: model=%s auto=%v (wakes once at startup)\n", *llmModel, *convAuto)
	}

	if *bootstrapMake {
		ma := makefileagent.New(makefileagent.Config{
			ID: "makefile-agent", ModuleRoot: absRoot,
			Sender: sender, Confirm: confirm, Print: printf,
		})
		go runAgent(ctx, log, "makefile-agent", ma.Run)
		fmt.Printf("bootstrap makefile-agent: model=%s auto=%v (wakes once at startup)\n", *llmModel, *convAuto)
	}

	if *bootstrapGin {
		ga := ginagent.New(ginagent.Config{
			ID: "gin-agent", ModuleRoot: absRoot,
			Sender: sender, FSBoard: fsBoard,
			Confirm: confirm, Print: printf,
			Settle: 2 * time.Second,
		})
		go runAgent(ctx, log, "gin-agent", ga.Run)
		fmt.Printf("conv gin-agent: model=%s auto=%v (steady-state)\n", *llmModel, *convAuto)
	}

	if *bootstrapConnect {
		ca := connectagent.New(connectagent.Config{
			ID: "connect-agent", ModuleRoot: absRoot,
			Sender: sender, FSBoard: fsBoard,
			Confirm: confirm, Print: printf,
			Settle: 2 * time.Second,
		})
		go runAgent(ctx, log, "connect-agent", ca.Run)
		fmt.Printf("conv connect-agent: model=%s auto=%v (steady-state)\n", *llmModel, *convAuto)
	}

	if *bootstrapAuto {
		// Generic dispatch: read go.mod, ask the registry which agents
		// apply, instantiate each. This is the path that scales — adding
		// a new agent (gqlgen, oapi-codegen, mockgen, etc.) requires only
		// registering it in its package's init(); no cmd/demo edit needed.
		goModPath := filepath.Join(absRoot, "go.mod")
		goModBytes, err := os.ReadFile(goModPath)
		if err != nil {
			log.Error("bootstrap-auto: cannot read go.mod", "path", goModPath, "err", err)
		} else {
			goMod := string(goModBytes)
			matched := registry.Detect(goMod, absRoot)
			deps := registry.BuildDeps{
				ModuleRoot: absRoot,
				Sender:     sender,
				Confirm:    confirm,
				Print:      printf,
				FSBoard:    fsBoard,
			}
			fmt.Printf("\nbootstrap-auto: %d agent(s) detected from go.mod\n", len(matched))
			for _, spec := range matched {
				agentRunner := spec.Build(deps)
				fmt.Printf("  • %s — %s\n", spec.Name, spec.Description)
				name := spec.Name
				go runAgent(ctx, log, name+"-agent", agentRunner.Run)
			}
		}
	}

	fmt.Println("\nwatching for changes (Ctrl-C to stop)…")
	fmt.Println()

	<-ctx.Done()
	fmt.Println("\nshutting down...")
	// Give in-flight agents a moment to finish their current Wake.
	time.Sleep(500 * time.Millisecond)
}

// runAgent runs an agent's Run loop with exponential-backoff retry on
// failure. The most common failure mode in practice is a transient
// Claude API hiccup (rate limit, brief network outage); without retry
// the agent silently dies and stays dead until coven restarts.
//
// Retry policy:
//   - First retry after 1s, then 2s, 4s, 8s, capped at 30s.
//   - Backoff resets to 1s if the agent ran for ≥60s before failing
//     (i.e. the failure is transient, not a tight loop).
//   - No max-retries cap. The user can Ctrl-C if they want to stop.
//   - ctx cancellation always wins; we never retry past a cancelled ctx.
//
// We deliberately don't classify failures (network vs. config vs. bug).
// Repeated identical failures become visible in the log and the user
// decides; classifying inside the helper would be too clever.
func runAgent(ctx context.Context, log *slog.Logger, name string, run func(context.Context) error) {
	const (
		baseBackoff  = 1 * time.Second
		maxBackoff   = 30 * time.Second
		resetAfter   = 60 * time.Second // long-enough run resets backoff
	)
	backoff := baseBackoff

	for {
		started := time.Now()
		err := run(ctx)

		// Context cancelled (Ctrl-C, signal): exit cleanly, no retry.
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Run returned nil without ctx cancellation. This is unusual
			// for a steady-state agent — it implies the agent decided
			// it's done. Don't restart; respect that.
			return
		}

		// Reset backoff if the agent ran for a while before failing.
		if time.Since(started) >= resetAfter {
			backoff = baseBackoff
		}

		log.Warn(name+" failed; restarting after backoff",
			"err", err,
			"backoff", backoff,
			"ran_for", time.Since(started).Round(time.Millisecond))

		// Sleep, but interruptible by ctx cancellation.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Exponential growth, capped.
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// buildSender returns the LLM sender. Defaults to sonnet.
func buildSender(model string) llm.Sender {
	switch model {
	case "haiku":
		return llm.ClaudeConversation(llm.ClaudeConfig{Model: llm.ClaudeHaiku})
	case "opus":
		return llm.ClaudeConversation(llm.ClaudeConfig{Model: llm.ClaudeOpus})
	case "sonnet", "":
		return llm.ClaudeConversation(llm.ClaudeConfig{Model: llm.ClaudeSonnet})
	default:
		fmt.Fprintf(os.Stderr, "unknown -llm model %q; defaulting to sonnet\n", model)
		return llm.ClaudeConversation(llm.ClaudeConfig{Model: llm.ClaudeSonnet})
	}
}

// makeStdinConfirm returns a ConfirmFunc that gates exec calls behind
// a y/N prompt. autoConfirm bypasses the prompt (dev only).
//
// stdin handling: we deliberately do NOT use bufio. bufio's read-ahead
// would consume bytes typed BETWEEN prompts (e.g. stray y's typed after
// one prompt completed but before the next started), causing later
// prompts to read pre-buffered garbage instead of the user's current
// intent. Reading one byte at a time directly from os.Stdin until '\n'
// is slower but behaves correctly under concurrent agent activity.
//
// We also drain stdin before each prompt to discard any keystrokes
// the user typed while no prompt was active.
var stdinMu sync.Mutex

func makeStdinConfirm(autoConfirm bool) agent.ConfirmFunc {
	return func(ctx context.Context, toolName, summary string) bool {
		if autoConfirm {
			fmt.Printf("  [confirm] %s — auto-approved\n", summary)
			return true
		}
		stdinMu.Lock()
		defer stdinMu.Unlock()

		// Discard any pre-buffered keystrokes from between prompts.
		drainStdin()

		// Brief pause so the agent's narration above settles before the prompt.
		time.Sleep(150 * time.Millisecond)

		const bar = "═══════════════════════════════════════════════════════════════"
		fmt.Print("\n\n")
		fmt.Printf("  %s\n", bar)
		fmt.Printf("  >>> CONFIRM: %s\n", summary)
		fmt.Printf("  %s\n", bar)
		fmt.Print("  run? [y/N] ")

		line := readLineDirect()
		line = strings.TrimSpace(strings.ToLower(line))
		ok := line == "y" || line == "yes"
		if ok {
			fmt.Print("  → approved\n\n")
		} else {
			fmt.Print("  → declined\n\n")
		}
		return ok
	}
}

// readLineDirect reads from os.Stdin one byte at a time until newline
// or EOF. Avoids bufio's hidden read-ahead buffer.
func readLineDirect() string {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 || err != nil {
			break
		}
		if buf[0] == '\n' {
			break
		}
		if buf[0] == '\r' {
			continue
		}
		b.WriteByte(buf[0])
	}
	return b.String()
}

// drainStdin consumes any bytes already sitting in stdin's pipe buffer.
// Sets stdin non-blocking, reads everything available, restores blocking.
// Best-effort: silent no-op if non-blocking can't be set (e.g. on Windows).
func drainStdin() {
	fd := int(os.Stdin.Fd())
	if err := setFdNonblock(fd, true); err != nil {
		return
	}
	defer setFdNonblock(fd, false)

	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 || err != nil {
			return
		}
	}
}

// absoluteRoot expands ~ and resolves to an absolute, symlink-clean path.
func absoluteRoot(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = home + p[1:]
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	return abs, nil
}
