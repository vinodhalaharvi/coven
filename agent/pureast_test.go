package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vinodhalaharvi/coven/llm"
)

// makePackage writes a tiny Go package into a temp dir for pureast to
// analyze. Returns the absolute path to the package directory.
func makePackage(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	pkgDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package demo

// User represents an account.
type User struct {
	ID    int
	Email string
}

// NewUser constructs a User.
func NewUser(id int, email string) *User {
	return &User{ID: id, Email: email}
}

// Greet returns a salutation.
func (u *User) Greet() string {
	return "hi " + u.Email
}

// Deactivate flips the user off.
func (u *User) Deactivate() {}

// Reader is a simple reader interface.
type Reader interface {
	Read() string
}

// Helper is a free function unrelated to User.
func Helper() int { return 42 }
`
	if err := os.WriteFile(filepath.Join(pkgDir, "demo.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPureAst_ToolSpec(t *testing.T) {
	tool := PureAstTool("/tmp")
	if tool.Spec.Name != "pureast" {
		t.Errorf("name = %q", tool.Spec.Name)
	}
	if !tool.Pure {
		t.Error("pureast should be pure (no confirmation)")
	}
	props := tool.Spec.InputSchema["properties"].(map[string]any)
	for _, expected := range []string{"op", "path", "pattern", "symbol", "kind", "max_results", "minimal", "grouped"} {
		if _, ok := props[expected]; !ok {
			t.Errorf("schema missing property %q", expected)
		}
	}
	op := props["op"].(map[string]any)
	enum := op["enum"].([]string)
	if len(enum) != 5 {
		t.Errorf("op enum has %d entries, want 5", len(enum))
	}
}

func TestPureAst_RequiresOp(t *testing.T) {
	tool := PureAstTool("/tmp")
	_, err := tool.Run(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "op is required") {
		t.Errorf("expected 'op is required' error, got: %v", err)
	}
}

func TestPureAst_PerOpRequiredFields(t *testing.T) {
	tool := PureAstTool("/tmp")
	cases := []struct {
		name    string
		input   map[string]any
		errFrag string
	}{
		{"search needs pattern", map[string]any{"op": "search"}, "requires 'pattern'"},
		{"extract needs symbol", map[string]any{"op": "extract"}, "requires 'symbol'"},
		{"deps needs symbol", map[string]any{"op": "deps"}, "requires 'symbol'"},
		{"methods needs symbol", map[string]any{"op": "methods"}, "requires 'symbol'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.errFrag) {
				t.Errorf("expected %q in error, got: %v", tc.errFrag, err)
			}
		})
	}
}

func TestPureAst_UnknownOp(t *testing.T) {
	tool := PureAstTool("/tmp")
	_, err := tool.Run(context.Background(), map[string]any{"op": "frobnicate"})
	if err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Errorf("expected 'unknown op' error, got: %v", err)
	}
}

func TestPureAst_PathEscapeBlocked(t *testing.T) {
	tool := PureAstTool("/tmp")
	_, err := tool.Run(context.Background(), map[string]any{
		"op":   "list_symbols",
		"path": "../../../etc",
	})
	if err == nil {
		t.Error("expected escape rejection")
	}
	if err != nil && !strings.Contains(err.Error(), "escapes project root") {
		t.Errorf("expected escape error, got: %v", err)
	}
}

// TestPureAst_ListSymbols_Real - actually parses our test package
// with pureast and verifies the response mentions the symbols we put in.
func TestPureAst_ListSymbols_Real(t *testing.T) {
	root := makePackage(t)
	tool := PureAstTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"op":   "list_symbols",
		"path": "demo",
	})
	if err != nil {
		t.Fatalf("list_symbols failed: %v", err)
	}
	for _, expected := range []string{"User", "NewUser", "Reader", "Helper"} {
		if !strings.Contains(out, expected) {
			t.Errorf("output missing %q\nOutput was:\n%s", expected, out)
		}
	}
}

// TestPureAst_Search_Real - fuzzy search for "User" should find at
// least the User type and NewUser constructor.
func TestPureAst_Search_Real(t *testing.T) {
	root := makePackage(t)
	tool := PureAstTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"op":      "search",
		"pattern": "User",
		"path":    "demo",
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(out, "User") {
		t.Errorf("search output missing 'User':\n%s", out)
	}
}

// TestPureAst_Methods_Real - methods on User should include Greet
// and Deactivate, but NOT Helper (which is a free function).
func TestPureAst_Methods_Real(t *testing.T) {
	root := makePackage(t)
	tool := PureAstTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"op":     "methods",
		"symbol": "User",
		"path":   "demo",
	})
	if err != nil {
		t.Fatalf("methods failed: %v", err)
	}
	for _, m := range []string{"Greet", "Deactivate"} {
		if !strings.Contains(out, m) {
			t.Errorf("methods output missing %q\nOutput:\n%s", m, out)
		}
	}
	if strings.Contains(out, "Helper") {
		t.Errorf("methods output should NOT contain Helper (free function), got:\n%s", out)
	}
}

// TestPureAst_Extract_Real - extract User and verify the output
// contains the type definition.
func TestPureAst_Extract_Real(t *testing.T) {
	root := makePackage(t)
	tool := PureAstTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"op":     "extract",
		"symbol": "User",
		"path":   "demo",
	})
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if !strings.Contains(out, "type User") {
		t.Errorf("extract output missing 'type User' definition:\n%s", out)
	}
}

// TestPureAst_Deps_Real - deps on NewUser should at minimum mention
// User (the type it returns).
func TestPureAst_Deps_Real(t *testing.T) {
	root := makePackage(t)
	tool := PureAstTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"op":     "deps",
		"symbol": "NewUser",
		"path":   "demo",
	})
	if err != nil {
		t.Fatalf("deps failed: %v", err)
	}
	if !strings.Contains(out, "User") {
		t.Errorf("deps output missing 'User' (NewUser returns it):\n%s", out)
	}
}

func TestStandardTools_IncludesPureast(t *testing.T) {
	tools := StandardTools("/tmp")
	found := false
	for _, t := range tools {
		if t.Spec.Name == "pureast" {
			found = true
			break
		}
	}
	if !found {
		t.Error("StandardTools should include pureast")
	}
}

// TestPureAst_ToolDispatchedThroughAgent - end-to-end: scripted Claude
// requests pureast, agent runtime dispatches, conversation grows.
func TestPureAst_ToolDispatchedThroughAgent(t *testing.T) {
	root := makePackage(t)
	scripted := llm.ScriptedSender(
		llm.ToolUseResponse("pureast", map[string]any{
			"op":   "list_symbols",
			"path": "demo",
		}, "u1"),
		llm.TextResponse("done"),
	)
	a := New(Config{
		ID:      "test",
		Tools:   StandardTools(root),
		Sender:  scripted,
		Print:   func(string) {},
		Confirm: func(context.Context, string, string) bool { return false },
	})
	if _, err := a.Wake(context.Background(), "list demo symbols"); err != nil {
		t.Fatalf("agent wake failed: %v", err)
	}
	if got := a.HistoryLen(); got != 4 {
		t.Errorf("history len = %d, want 4 (obs + tool_use + result + final)", got)
	}
}
