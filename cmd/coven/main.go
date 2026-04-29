// coven is the v2 binary. Replaces cmd/demo from v1.
//
// v2 hides the per-agent flag soup behind a single ControlPlane.
// There is no -bootstrap-gin, -bootstrap-connect, -conv-proto, etc.
// The router decides which agents run based on what changed in the
// project. The user provides only the project root and a few global
// configuration knobs.
//
// Usage:
//
//	coven -root <path> [-llm sonnet|haiku|opus] [-auto-confirm]
//	      [-enable-repair]
//
// Flags:
//
//	-root           project root to watch (default ".")
//	-llm            Claude model: sonnet (default), haiku, opus
//	-auto-confirm   skip y/n prompts (dev/testing only)
//	-enable-repair  enable LLM-mediated repair for merge conflicts and
//	                validator failures (default off — repair is a
//	                best-effort optimization that costs LLM calls)
//
// Environment:
//
//	ANTHROPIC_API_KEY   required to call Claude
//
// Behavior:
//
//   - File changes trigger a 2.5s settle window, after which the
//     project's diff against HEAD is sent to Claude for routing.
//   - Each routed agent gets its own worktree under .coven/worktrees/
//     and works in isolation.
//   - Agent commits go to a per-agent branch; the integrator merges
//     them serially into main, gated by validators (go build/vet/test)
//     and a y/n approval prompt.
//   - Agents that don't actually commit anything are silently
//     cleaned up — no integration request is queued.
//
// Side-effects on the project:
//
//   - .coven/worktrees/ directory is created (gitignored automatically)
//   - .gitignore gets an entry for .coven/ if not present
//   - merges land on main (with user approval at the integrate step)
//
// To go back to v1, run cmd/demo with its existing flags. v2 lives
// alongside v1 — neither replaces the other yet. cmd/demo and the
// per-agent goroutine model will be removed in a later commit once
// v2 has proven itself on real projects.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/controlplane"
	"github.com/vinodhalaharvi/coven/llm"

	// Blank imports below cause each agent package's init() to run,
	// which registers the agent with the registry. The router reads
	// from registry.All() to know what agents are available.
	//
	// Without these imports, the registry is empty at startup and
	// the router will return "no agents registered" for every diff.
	//
	// Order doesn't matter — Go runs init()s in package dependency
	// order, but registry.Register is order-independent.
	_ "github.com/vinodhalaharvi/coven/buildhealth"
	_ "github.com/vinodhalaharvi/coven/connectagent"
	_ "github.com/vinodhalaharvi/coven/dockeragent"
	_ "github.com/vinodhalaharvi/coven/ginagent"
	_ "github.com/vinodhalaharvi/coven/gogenerateagent"
	_ "github.com/vinodhalaharvi/coven/mainbuilder"
	_ "github.com/vinodhalaharvi/coven/makefileagent"
	_ "github.com/vinodhalaharvi/coven/protoagent"
	_ "github.com/vinodhalaharvi/coven/sqlcagent"
	_ "github.com/vinodhalaharvi/coven/testagent"
	_ "github.com/vinodhalaharvi/coven/wireagent"
)

func main() {
	var (
		root         = flag.String("root", ".", "project root to watch")
		llmModel     = flag.String("llm", "sonnet", "Claude model: sonnet | haiku | opus")
		autoConfirm  = flag.Bool("auto-confirm", false, "skip y/n prompts (dev only)")
		enableRepair = flag.Bool("enable-repair", false, "enable LLM-mediated repair for merge conflicts and validator failures")
	)
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve root: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(absRoot); err != nil {
		fmt.Fprintf(os.Stderr, "root not accessible: %v\n", err)
		os.Exit(1)
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY is required")
		os.Exit(1)
	}

	sender := senderFor(*llmModel)
	confirm := makeStdinConfirm(*autoConfirm)
	print := makeStderrPrint()

	cp := controlplane.New(controlplane.Config{
		ProjectRoot:  absRoot,
		Sender:       sender,
		Confirm:      confirm,
		Print:        print,
		Settle:       2500 * time.Millisecond,
		EnableRepair: *enableRepair,
	})

	fmt.Fprintf(os.Stderr, "[coven v2] starting (root=%s model=%s auto-confirm=%v repair=%v)\n",
		absRoot, *llmModel, *autoConfirm, *enableRepair)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown on Ctrl-C / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n[coven v2] shutting down (Ctrl-C may need to be pressed again to interrupt active LLM calls)")
		cancel()
	}()

	if err := cp.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[coven v2] error: %v\n", err)
		os.Exit(1)
	}
}

// senderFor maps the -llm flag value to a Claude sender. Mirrors v1's
// pattern in cmd/demo.
func senderFor(model string) llm.Sender {
	switch strings.ToLower(model) {
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

// makeStderrPrint returns a PrintFunc that writes to stderr, mirroring
// v1's behavior. We use stderr (not stdout) so that any future output
// from agents on stdout (e.g., generated diffs piped into another
// tool) doesn't get polluted by status messages.
func makeStderrPrint() agent.PrintFunc {
	return func(s string) {
		fmt.Fprint(os.Stderr, s)
	}
}

// stdinMu serializes confirm prompts so concurrent agent requests
// don't intermingle their y/n prompts on the terminal. Same pattern
// as v1's cmd/demo.
var stdinMu sync.Mutex

// makeStdinConfirm returns a ConfirmFunc that gates exec calls behind
// a y/n prompt on stdin. autoConfirm bypasses the prompt — used for
// development and CI runs only.
//
// This is borrowed from v1's cmd/demo. Once v2 is the canonical entry
// point, we can extract this into a shared helper package, but for
// now keeping it inline avoids cross-package dependencies for the
// entry binary.
func makeStdinConfirm(autoConfirm bool) agent.ConfirmFunc {
	return func(ctx context.Context, toolName, summary string) bool {
		if autoConfirm {
			fmt.Fprintf(os.Stderr, "  [confirm] %s — auto-approved\n", summary)
			return true
		}
		stdinMu.Lock()
		defer stdinMu.Unlock()

		// Brief pause so prior narration settles before the prompt.
		time.Sleep(150 * time.Millisecond)

		const bar = "═══════════════════════════════════════════════════════════════"
		fmt.Fprint(os.Stderr, "\n\n")
		fmt.Fprintf(os.Stderr, "  %s\n", bar)
		fmt.Fprintf(os.Stderr, "  >>> CONFIRM: %s\n", summary)
		fmt.Fprintf(os.Stderr, "  %s\n", bar)

		// Require explicit y/n. No default.
		for {
			fmt.Fprint(os.Stderr, "  approve? type 'y' or 'n': ")
			line := readLineDirect()
			line = strings.TrimSpace(strings.ToLower(line))
			switch line {
			case "y", "yes":
				fmt.Fprintln(os.Stderr, "  → approved")
				return true
			case "n", "no":
				fmt.Fprintln(os.Stderr, "  → declined")
				return false
			default:
				fmt.Fprintln(os.Stderr, "  (please type 'y' or 'n' explicitly — no default)")
			}
		}
	}
}

// readLineDirect reads stdin one byte at a time until newline. Avoids
// bufio's hidden read-ahead buffer that can swallow keystrokes typed
// between prompts. Same pattern as v1.
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
