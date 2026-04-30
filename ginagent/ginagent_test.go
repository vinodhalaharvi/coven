package ginagent

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
	"github.com/vinodhalaharvi/coven/registry"
)

func silentPrint(string)                                 {}
func alwaysConfirm(context.Context, string, string) bool { return true }
func alwaysDeny(context.Context, string, string) bool    { return false }

func TestRoleString_HasKeyConcepts(t *testing.T) {
	must := []string{
		"gin",
		"sqlc",
		"STEADY-STATE",
		"go build ./...",
		"equilibrium",
		"do NOT invent business logic",
	}
	for _, frag := range must {
		if !strings.Contains(Role, frag) {
			t.Errorf("Role missing required concept: %q", frag)
		}
	}
	// Generated-files rule (from 0038).
	if !strings.Contains(Role, "must NOT hand-edit") && !strings.Contains(Role, "must not hand-edit") {
		t.Errorf("Role should mention not hand-editing generated files")
	}
}

func TestIsRelevant(t *testing.T) {
	cases := map[string]bool{
		"gen/db/users.sql.go":              true,
		"gen/db/orders.sql.go":             true,
		"internal/handlers/users.go":       true,
		"internal/api/v1/orders.go":        true,
		"internal/controllers/users.go":    true,
		"some/path/internal/handlers/x.go": true,

		"gen/db/db.go":           false, // sqlc boilerplate
		"gen/db/models.go":       false,
		"main.go":                false,
		"app/wire.go":            false,
		"proto/x.proto":          false,
		"some/random/file.go":    false,
	}
	for path, want := range cases {
		got := IsRelevant(path)
		if got != want {
			t.Errorf("IsRelevant(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAgent_RegistersWithRegistry(t *testing.T) {
	spec := registry.ByName("gin")
	if spec == nil {
		t.Fatal("gin spec not registered")
	}
	if spec.Detect == nil || spec.Build == nil {
		t.Error("Detect or Build is nil")
	}
}

func TestDetector_FiresOnGinDep(t *testing.T) {
	spec := registry.ByName("gin")
	if spec == nil {
		t.Fatal("gin spec not registered")
	}

	cases := map[string]struct {
		goMod string
		want  bool
	}{
		"gin present": {
			goMod: "module x\n\nrequire github.com/gin-gonic/gin v1.10.0\n",
			want:  true,
		},
		"gin absent": {
			goMod: "module x\n\nrequire github.com/labstack/echo/v4 v4.11.0\n",
			want:  false,
		},
		"empty": {goMod: "", want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := spec.Detect(tc.goMod, "/tmp"); got != tc.want {
				t.Errorf("Detect = %v, want %v", got, tc.want)
			}
		})
	}
}

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
			Blocks: []llm.Block{{Text: "no sqlc output, nothing to wire"}},
		}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root, Sender: sender, Print: silentPrint,
		Confirm: alwaysDeny, Settle: 50 * time.Millisecond,
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

func TestAgent_WakesOnSqlGoChange(t *testing.T) {
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
		return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Text: "ack"}}}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root, Sender: sender, FSBoard: board, Print: silentPrint,
		Confirm: alwaysConfirm, Settle: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go a.Run(ctx)

	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	pre := wakes
	mu.Unlock()

	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "gen", "db", "users.sql.go")},
		ChangedAt:    time.Now(),
	}, "test")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("wake never fired. wakes=%d", wakes)
		default:
		}
		mu.Lock()
		w := wakes
		mu.Unlock()
		if w > pre {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

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
		return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Text: "ack"}}}, llm.StopEndTurn, nil
	}

	a := New(Config{
		ModuleRoot: root, Sender: sender, FSBoard: board, Print: silentPrint,
		Confirm: alwaysConfirm, Settle: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go a.Run(ctx)

	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	pre := wakes
	mu.Unlock()

	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "proto", "x.proto")},
		ChangedAt:    time.Now(),
	}, "test")
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != pre {
		t.Errorf("wakes increased on irrelevant file: %d -> %d", pre, wakes)
	}
}
