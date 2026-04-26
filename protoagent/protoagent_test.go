package protoagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/llm"
)

func silentPrint(string) {}
func alwaysConfirm(context.Context, string, string) bool { return true }
func alwaysDeny(context.Context, string, string) bool    { return false }

func TestIsRelevant(t *testing.T) {
	cases := map[string]bool{
		"proto/user/v1/user.proto":  true,
		"buf.yaml":                  true,
		"buf.gen.yaml":              true,
		"buf.lock":                  true,
		"proto/foo.txt":             true, // anything under proto/ counts
		"main.go":                   false,
		"go.mod":                    false,
		"gen/user/v1/user.pb.go":    false,
		"some/random/file.txt":      false,
	}
	for path, want := range cases {
		got := IsRelevant(path)
		if got != want {
			t.Errorf("IsRelevant(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestFormatObservation(t *testing.T) {
	if got := formatObservation(nil); !strings.Contains(got, "no specific files") {
		t.Errorf("empty files: %q", got)
	}
	if got := formatObservation([]string{"a.proto"}); !strings.Contains(got, "a.proto") {
		t.Errorf("single file: %q", got)
	}
	got := formatObservation([]string{"a.proto", "b.proto", "a.proto"})
	if !strings.Contains(got, "2 files") {
		t.Errorf("dedupe should yield 2 files: %q", got)
	}
}

// TestAgent_BootstrapWakesOnStartup - the agent should run the conversation
// once at startup so empty/broken projects get diagnosed.
func TestAgent_BootstrapWakesOnStartup(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0644)

	var wakes int
	var mu sync.Mutex
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		wakes++
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "no proto files; nothing to do"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ID:         "proto-agent",
		ModuleRoot: root,
		Sender:     sender,
		Print:      silentPrint,
		Confirm:    alwaysDeny,
		Settle:     50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go a.Run(ctx)

	// Wait for the bootstrap wake.
	deadline := time.After(800 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatal("bootstrap wake never happened")
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
	if a.HistoryLen() < 2 {
		t.Errorf("history len = %d (expected at least 2: observation + response)", a.HistoryLen())
	}
}

// TestAgent_WakesOnFileChangeFact - posting a relevant FileChangeFact
// triggers a wake after the settle window.
func TestAgent_WakesOnFileChangeFact(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	board := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{})

	var wakes int
	var observations []string
	var mu sync.Mutex
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		wakes++
		// Capture what observation triggered this wake
		if len(conv) > 0 {
			lastUser := conv[len(conv)-1]
			for _, b := range lastUser.Blocks {
				if b.Text != "" {
					observations = append(observations, b.Text)
				}
			}
		}
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "ack"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ID:         "proto-agent",
		ModuleRoot: root,
		Sender:     sender,
		FSBoard:    board,
		Print:      silentPrint,
		Confirm:    alwaysConfirm,
		Settle:     100 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go a.Run(ctx)

	// Wait for bootstrap to complete (bootstrap wake comes first).
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	preBootstrap := wakes
	mu.Unlock()

	// Now post a relevant FileChangeFact.
	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "proto", "user.proto")},
		ChangedAt:    time.Now(),
	}, "test")

	// Wait for the debounced wake.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("file-change wake never happened. wakes=%d", wakes)
		default:
		}
		mu.Lock()
		w := wakes
		mu.Unlock()
		if w > preBootstrap {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	// The most recent observation should mention the proto file.
	found := false
	for _, obs := range observations {
		if strings.Contains(obs, "user.proto") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected observation to mention user.proto. observations: %v", observations)
	}
}

// TestAgent_IgnoresIrrelevantFileChange - changes to non-proto files
// should NOT trigger a wake.
func TestAgent_IgnoresIrrelevantFileChange(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	board := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{})

	var wakes int
	var mu sync.Mutex
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		wakes++
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "ack"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ID:         "proto-agent",
		ModuleRoot: root,
		Sender:     sender,
		FSBoard:    board,
		Print:      silentPrint,
		Confirm:    alwaysConfirm,
		Settle:     100 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go a.Run(ctx)

	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	preBootstrap := wakes
	mu.Unlock()

	// Post an IRRELEVANT change (a .go file).
	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "main.go")},
		ChangedAt:    time.Now(),
	}, "test")

	// Wait long enough for any wake to have fired.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != preBootstrap {
		t.Errorf("wakes increased from %d to %d on irrelevant file change", preBootstrap, wakes)
	}
}

// TestAgent_ConsultsTools - end-to-end: agent gets observation, asks
// Claude, Claude calls list_files, agent dispatches, Claude returns text.
func TestAgent_ConsultsTools(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.MkdirAll(filepath.Join(root, "proto"), 0755)
	os.WriteFile(filepath.Join(root, "proto", "x.proto"), []byte("syntax = \"proto3\";"), 0644)

	scripted := llm.ScriptedSender(
		llm.ToolUseResponse("list_files", map[string]any{"glob": "**/*.proto"}, "u1"),
		llm.ToolUseResponse("read_file", map[string]any{"path": "proto/x.proto"}, "u2"),
		llm.TextResponse("found 1 proto file; no buf config so I won't run anything yet."),
	)

	a := New(Config{
		ID:         "proto-agent",
		ModuleRoot: root,
		Sender:     scripted,
		Print:      silentPrint,
		Confirm:    alwaysConfirm,
		Settle:     100 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go a.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("agent never reached final state")
		default:
		}
		// HistoryLen grows with each turn: user obs + 3 turns (2 tool, 1 final)
		// = user + assistant_use + user_result + assistant_use + user_result + assistant_text = 6
		if a.HistoryLen() >= 6 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}
