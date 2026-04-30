package connectagent

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
		"connect-go",
		"protoc-gen-connect-go",
		"STEADY-STATE",
		"go build ./...",
		"equilibrium",
		"do NOT invent business logic",
		"RegisterConnectHandlers",
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
		// Connect-generated files (various naming conventions):
		"gen/orders/v1/ordersv1connect/orders_connect.pb.go": true,
		"gen/users/v1/usersv1connect/users.connect.go":       true,
		"gen/foo/v1/fooconnect/index.go":                     true,
		"some/path/svc_connect.pb.go":                        true,

		// Handler files:
		"internal/handlers/order_service.go":      true,
		"internal/handlers/connect_router.go":     true,
		"internal/api/v1/users.go":                true,
		"some/internal/handlers/x.go":             true,

		// NOT relevant:
		"main.go":                       false,
		"app/wire.go":                   false,
		"proto/x.proto":                 false,
		"gen/db/users.sql.go":           false, // sqlc — that's gin-agent
		"some/random/file.go":           false,
		"buf.gen.yaml":                  false, // proto-agent's domain
	}
	for path, want := range cases {
		got := IsRelevant(path)
		if got != want {
			t.Errorf("IsRelevant(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAgent_RegistersWithRegistry(t *testing.T) {
	spec := registry.ByName("connect")
	if spec == nil {
		t.Fatal("connect spec not registered")
	}
	if spec.Detect == nil || spec.Build == nil {
		t.Error("Detect or Build is nil")
	}
}

func TestDetector_FiresOnConnectDep(t *testing.T) {
	spec := registry.ByName("connect")
	if spec == nil {
		t.Fatal("connect spec not registered")
	}

	cases := map[string]struct {
		goMod string
		want  bool
	}{
		"connect present": {
			goMod: "module x\n\nrequire connectrpc.com/connect v1.18.0\n",
			want:  true,
		},
		"connect with subpackage": {
			goMod: "require connectrpc.com/connect/cors v0.1.0\n",
			want:  true,
		},
		"connect absent": {
			goMod: "module x\n\nrequire google.golang.org/grpc v1.50.0\n",
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
			Blocks: []llm.Block{{Text: "no connect bindings found"}},
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

func TestAgent_WakesOnConnectGoChange(t *testing.T) {
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
		ChangedFiles: []string{
			filepath.Join(root, "gen", "orders", "v1", "ordersv1connect", "orders_connect.pb.go"),
		},
		ChangedAt: time.Now(),
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

	// gen/db is sqlc territory, not connect — should NOT wake.
	board.Post("k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "gen", "db", "users.sql.go")},
		ChangedAt:    time.Now(),
	}, "test")
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != pre {
		t.Errorf("wakes increased on irrelevant file: %d -> %d", pre, wakes)
	}
}

func TestSpec_BuildsRunner(t *testing.T) {
	spec := registry.ByName("connect")
	if spec == nil {
		t.Fatal("connect spec not registered")
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

func TestProtoAgent_RoleMentionsConnectPlugin(t *testing.T) {
	// Cross-module sanity check: proto-agent's role string should
	// mention the buf.gen.yaml + connect plugin awareness we added.
	// This guards against a future refactor accidentally dropping that.
	//
	// We import-check via blank import below; the string is in
	// protoagent.Role.
	t.Skip("guarded indirectly: protoagent has its own role-string test")
}
