package wireagent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/llm"
)

func silentPrint(string)                                   {}
func alwaysConfirm(context.Context, string, string) bool   { return true }
func alwaysDeny(context.Context, string, string) bool      { return false }

func TestIsRelevant(t *testing.T) {
	cases := map[string]bool{
		"app/wire.go":         true,
		"auth/wire.go":        true,
		"app/wire_gen.go":     true,
		"go.mod":              true,
		"main.go":             false,
		"proto/x.proto":       false,
		"gen/user/v1/user.pb.go": false,
		"app/services.go":     false,
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
		t.Errorf("empty: %q", got)
	}
	if got := formatObservation([]string{"app/wire.go"}); !strings.Contains(got, "wire.go") {
		t.Errorf("single: %q", got)
	}
	got := formatObservation([]string{"a/wire.go", "b/wire.go", "a/wire.go"})
	if !strings.Contains(got, "2 files") {
		t.Errorf("dedupe: %q", got)
	}
}

// TestAgent_BootstrapWakesOnStartup - verify agent runs once at startup.
func TestAgent_BootstrapWakesOnStartup(t *testing.T) {
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
			Blocks: []llm.Block{{Text: "no wire packages, nothing to do"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root,
		Sender:     sender,
		Print:      silentPrint,
		Confirm:    alwaysDeny,
		Settle:     50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go a.Run(ctx)

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
}

// TestAgent_WakesOnWireGoEdit - editing wire.go triggers a wake.
func TestAgent_WakesOnWireGoEdit(t *testing.T) {
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

	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	preBootstrap := wakes
	mu.Unlock()

	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "app", "wire.go")},
		ChangedAt:    time.Now(),
	}, "test")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("wake never happened. wakes=%d", wakes)
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
	found := false
	for _, obs := range observations {
		if strings.Contains(obs, "wire.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected observation to mention wire.go: %v", observations)
	}
}

// TestAgent_IgnoresIrrelevantFiles - non-wire files shouldn't wake.
func TestAgent_IgnoresIrrelevantFiles(t *testing.T) {
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

	// Post .proto change - irrelevant to wire agent.
	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "proto", "x.proto")},
		ChangedAt:    time.Now(),
	}, "test")

	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != preBootstrap {
		t.Errorf("wakes increased from %d to %d on irrelevant file", preBootstrap, wakes)
	}
}
