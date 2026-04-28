package testagent

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
		"test agent",
		"go test ./...",
		"pureast",
		"_test.go",
		"real assertions, not TODO stubs",
		"do NOT modify production code",
		"public function",
	}
	for _, frag := range must {
		if !strings.Contains(Role, frag) {
			t.Errorf("Role missing required concept: %q", frag)
		}
	}
}

func TestRoleString_DoesNOTSayDontInventAssertions(t *testing.T) {
	// This test exists to guard against role drift back toward the
	// over-cautious framing we explicitly moved away from. The test
	// agent SHOULD write real assertions; if a future edit adds back
	// "don't invent assertions" or "use TODO stubs", this test fails.
	prohibited := []string{
		"don't invent assertion",
		"do not invent assertion",
		"TODO: assert",
	}
	low := strings.ToLower(Role)
	for _, frag := range prohibited {
		if strings.Contains(low, strings.ToLower(frag)) {
			t.Errorf("Role contains over-cautious language %q — agent should write real assertions, not TODO stubs", frag)
		}
	}
}

func TestIsRelevant(t *testing.T) {
	cases := map[string]bool{
		"main.go":                     true,
		"internal/handlers/users.go":  true,
		"internal/handlers/users_test.go": true, // test files also relevant — agent might need to update them
		"some/path/foo.go":            true,

		"main.txt":           false,
		"go.mod":             false,
		"buf.yaml":           false,
		"proto/order.proto":  false,
		"schema/users.sql":   false,
	}
	for path, want := range cases {
		got := IsRelevant(path)
		if got != want {
			t.Errorf("IsRelevant(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAgent_RegistersWithRegistry(t *testing.T) {
	spec := registry.ByName("test")
	if spec == nil {
		t.Fatal("test spec not registered")
	}
	if spec.Detect == nil || spec.Build == nil {
		t.Error("Detect or Build is nil")
	}
}

func TestDetector_AlwaysApplicable(t *testing.T) {
	spec := registry.ByName("test")
	if spec == nil {
		t.Fatal("test spec not registered")
	}
	if !spec.Detect("module x\n", "/tmp") {
		t.Error("expected detector to fire on any non-empty go.mod")
	}
	if spec.Detect("", "/tmp") {
		t.Error("expected detector to NOT fire on empty go.mod")
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
			Blocks: []llm.Block{{Text: "no public functions to test"}},
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

func TestAgent_WakesOnGoFileChange(t *testing.T) {
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
		ChangedFiles: []string{filepath.Join(root, "internal", "service.go")},
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

func TestAgent_IgnoresNonGoFiles(t *testing.T) {
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
		ChangedFiles: []string{
			filepath.Join(root, "proto", "x.proto"),
			filepath.Join(root, "schema", "users.sql"),
		},
		ChangedAt: time.Now(),
	}, "test")
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != pre {
		t.Errorf("wakes increased on non-Go files: %d -> %d", pre, wakes)
	}
}

func TestSpec_BuildsRunner(t *testing.T) {
	spec := registry.ByName("test")
	if spec == nil {
		t.Fatal("test spec not registered")
	}
	root := t.TempDir()
	deps := registry.BuildDeps{
		ModuleRoot: root,
		Sender:     llm.ScriptedSender(llm.TextResponse("done")),
		Confirm:    alwaysDeny,
		Print:      silentPrint,
	}
	r := spec.Build(deps)
	if r == nil {
		t.Fatal("Build returned nil")
	}
	if _, ok := r.(*Agent); !ok {
		t.Errorf("Build returned wrong type: %T", r)
	}
}
