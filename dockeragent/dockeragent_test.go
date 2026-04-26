package dockeragent

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

func TestRoleString_HasKeyConcepts(t *testing.T) {
	must := []string{
		"Dockerfile",
		"docker-compose",
		"go.mod",
		"multi-stage",
		"docker compose config",
		"final authority",
		"dormant",
	}
	for _, frag := range must {
		if !strings.Contains(Role, frag) {
			t.Errorf("Role missing required concept: %q", frag)
		}
	}
}

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
		if len(conv) > 0 {
			for _, b := range conv[0].Blocks {
				if b.Text != "" {
					observations = append(observations, b.Text)
				}
			}
		}
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "no cmd/*/ directories; project is a library"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root, Sender: sender, Print: silentPrint, Confirm: alwaysDeny,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()

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

	select {
	case <-done:
		t.Fatal("Run returned before ctx was cancelled")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(observations) == 0 {
		t.Fatal("no observation captured")
	}
	for _, frag := range []string{"docker", "Dockerfile", "go.mod"} {
		if !strings.Contains(observations[0], frag) {
			t.Errorf("observation missing %q: %q", frag, observations[0])
		}
	}
}

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
		ModuleRoot: root, Sender: sender, Print: silentPrint, Confirm: alwaysConfirm,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	go a.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != 1 {
		t.Errorf("got %d wakes, want exactly 1", wakes)
	}
}
