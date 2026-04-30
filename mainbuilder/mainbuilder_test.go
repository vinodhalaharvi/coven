package mainbuilder

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/llm"
)

func silentPrint(string)                                 {}
func alwaysConfirm(context.Context, string, string) bool { return true }
func alwaysDeny(context.Context, string, string) bool    { return false }

// TestRoleString_HasKeyConstraints — guards against accidental role
// drift. The agent's behavior is defined by the role string; if these
// fundamental concepts disappear we want a test failure.
func TestRoleString_HasKeyConstraints(t *testing.T) {
	must := []string{
		"go.mod",                  // surveys go.mod for conventions
		"existing conventions",    // matches the project's style
		"do NOT invent business",  // no fabrication
		"go build ./...",          // verifies its own work
		"dormant",                 // wake-once semantic
	}
	for _, frag := range must {
		if !strings.Contains(Role, frag) {
			t.Errorf("Role missing required constraint: %q", frag)
		}
	}
}

// TestAgent_RunWakesOnceAndBlocks — the architectural contract: Run
// triggers exactly one Wake with the bootstrap observation, and then
// blocks until ctx is cancelled.
func TestAgent_RunWakesOnceAndBlocks(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	var wakes int
	var observations []string
	var mu sync.Mutex
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		wakes++
		// First message in conv is the user observation that triggered the wake.
		if len(conv) > 0 {
			for _, b := range conv[0].Blocks {
				if b.Text != "" {
					observations = append(observations, b.Text)
				}
			}
		}
		mu.Unlock()
		// Return a final message immediately — no tool calls — so the
		// inner agent loop terminates cleanly.
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "no missing entry points; project already has main."}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root,
		Sender:     sender,
		Print:      silentPrint,
		Confirm:    alwaysDeny,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()

	// Give the wake time to fire.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatal("wake never fired")
		default:
		}
		mu.Lock()
		w := wakes
		mu.Unlock()
		if w >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Run should still be blocked on ctx.Done at this point.
	select {
	case <-done:
		t.Fatal("Run returned before ctx was cancelled")
	default:
		// expected: still blocking
	}

	// Now cancel and verify Run returns.
	cancel()
	select {
	case <-done:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancel")
	}

	// Verify the observation mentions bootstrap-relevant terms.
	mu.Lock()
	defer mu.Unlock()
	if len(observations) == 0 {
		t.Fatal("no observation captured")
	}
	obs := observations[0]
	for _, frag := range []string{"main-builder", "go.mod", "starter"} {
		if !strings.Contains(obs, frag) {
			t.Errorf("observation missing %q: %q", frag, obs)
		}
	}
}

// TestAgent_DoesNotWakeAgainWhileRunning — even if we leave Run going
// for a while, no further wakes happen. (This is the "no fsmonitor
// subscription" property.)
func TestAgent_DoesNotWakeAgainWhileRunning(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	var wakes int
	var mu sync.Mutex
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		wakes++
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "done"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root, Sender: sender, Print: silentPrint, Confirm: alwaysDeny,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	go a.Run(ctx)

	// Wait for the initial wake plus ample slack for any spurious wakes.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != 1 {
		t.Errorf("got %d wakes, want exactly 1", wakes)
	}
}

// TestAgent_UsesStandardToolset — verifies via a tool-using sender
// that the agent has read_file, list_files, search_text, and exec.
func TestAgent_UsesStandardToolset(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	var seenTools []string
	var mu sync.Mutex
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		// First call: capture tool catalog. Then return a final message.
		if len(seenTools) == 0 {
			for _, tool := range tools {
				seenTools = append(seenTools, tool.Name)
			}
		}
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "ack"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root, Sender: sender, Print: silentPrint, Confirm: alwaysConfirm,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go a.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	expected := []string{"read_file", "list_files", "search_text", "exec"}
	for _, e := range expected {
		found := false
		for _, t := range seenTools {
			if t == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tool %q not exposed (got: %v)", e, seenTools)
		}
	}
}

// TestAgent_ConversationGrowsWithToolUse — sanity check that the
// inner agent loop is wired correctly: a tool-use response causes
// the conversation to grow beyond just the initial user message.
func TestAgent_ConversationGrowsWithToolUse(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	scripted := llm.ScriptedSender(
		llm.ToolUseResponse("list_files", map[string]any{"glob": "**/*.go"}, "u1"),
		llm.TextResponse("project is empty; nothing to bootstrap"),
	)

	a := New(Config{
		ModuleRoot: root, Sender: scripted, Print: silentPrint, Confirm: alwaysConfirm,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go a.Run(ctx)

	deadline := time.After(800 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatalf("conversation never grew. HistoryLen=%d", a.HistoryLen())
		default:
		}
		// user obs + assistant tool_use + user tool_result + assistant final = 4
		if a.HistoryLen() >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}
