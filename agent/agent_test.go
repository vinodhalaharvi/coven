package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vinodhalaharvi/coven/llm"
)

// silentPrint discards agent output for tests that don't care.
func silentPrint(string) {}

// alwaysConfirm is a ConfirmFunc that says yes to everything.
func alwaysConfirm(context.Context, string, string) bool { return true }

// alwaysDeny says no.
func alwaysDeny(context.Context, string, string) bool { return false }

// TestAgent_TextOnlyCompletion - simplest case: agent gets observation,
// Claude responds with final text, conversation ends.
func TestAgent_TextOnlyCompletion(t *testing.T) {
	a := New(Config{
		ID:      "test",
		Role:    "you are a test agent",
		Sender:  llm.ScriptedSender(llm.TextResponse("done, nothing to do")),
		Print:   silentPrint,
		Confirm: alwaysConfirm,
	})

	final, err := a.Wake(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if final != "done, nothing to do" {
		t.Errorf("final = %q", final)
	}
	// Conversation has: user observation + assistant response = 2 messages.
	if got := a.HistoryLen(); got != 2 {
		t.Errorf("history len = %d, want 2", got)
	}
}

// TestAgent_PureToolDispatch - Claude requests a pure tool, agent runs it
// without asking, feeds result back, Claude returns final text.
func TestAgent_PureToolDispatch(t *testing.T) {
	called := 0
	mockTool := Tool{
		Pure: true,
		Spec: llm.ToolSpec{
			Name: "list_things",
			Description: "list the things",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "thing1\nthing2", nil
		},
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{mockTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("list_things", map[string]any{}, "u1"),
			llm.TextResponse("found two things; done"),
		),
		Print:   silentPrint,
		Confirm: alwaysDeny, // pure tools shouldn't need confirm
	})

	final, err := a.Wake(context.Background(), "what's there?")
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("tool called %d times, want 1", called)
	}
	if !strings.Contains(final, "two things") {
		t.Errorf("final = %q", final)
	}
}

// TestAgent_MutatingToolRequiresConfirmation - exec-style tool requires y.
func TestAgent_MutatingToolRequiresConfirmation(t *testing.T) {
	called := 0
	execTool := Tool{
		Pure: false, // mutating
		Spec: llm.ToolSpec{
			Name: "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "ran ok", nil
		},
	}

	// User denies the first call.
	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "rm -rf /"}, "u1"),
			llm.TextResponse("user denied"),
		),
		Print:   silentPrint,
		Confirm: alwaysDeny,
	})

	final, err := a.Wake(context.Background(), "do something")
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Errorf("tool called %d times when denied, want 0", called)
	}
	if !strings.Contains(final, "denied") {
		t.Errorf("final = %q", final)
	}
}

// TestAgent_MutatingToolRunsOnConfirm - same scenario, user says yes.
func TestAgent_MutatingToolRunsOnConfirm(t *testing.T) {
	called := 0
	execTool := Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name: "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "ran", nil
		},
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "echo hi"}, "u1"),
			llm.TextResponse("ok"),
		),
		Print:   silentPrint,
		Confirm: alwaysConfirm,
	})

	_, err := a.Wake(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("tool called %d times, want 1", called)
	}
}

// TestAgent_MaxTurns - if Claude keeps asking for tools forever, we cap.
func TestAgent_MaxTurns(t *testing.T) {
	loopTool := Tool{
		Pure: true,
		Spec: llm.ToolSpec{Name: "loop", Description: "loops", InputSchema: map[string]any{"type": "object"}},
		Run:  func(ctx context.Context, _ map[string]any) (string, error) { return "looped", nil },
	}
	// Ten tool_use responses with no final text — should hit cap.
	scripts := make([]llm.ScriptedResponse, 0, 10)
	for i := 0; i < 10; i++ {
		scripts = append(scripts, llm.ToolUseResponse("loop", map[string]any{}, "u"))
	}

	a := New(Config{
		ID:       "test",
		Tools:    []Tool{loopTool},
		Sender:   llm.ScriptedSender(scripts...),
		Print:    silentPrint,
		MaxTurns: 5,
	})
	_, err := a.Wake(context.Background(), "go")
	if err == nil {
		t.Error("expected max-turns error")
	}
	if !strings.Contains(err.Error(), "max turns") {
		t.Errorf("err = %v", err)
	}
}

// TestAgent_ConversationPersistsAcrossWakes - calling Wake twice continues
// the same conversation rather than starting fresh.
func TestAgent_ConversationPersistsAcrossWakes(t *testing.T) {
	a := New(Config{
		ID: "test",
		Sender: llm.ScriptedSender(
			llm.TextResponse("first done"),
			llm.TextResponse("second done"),
		),
		Print: silentPrint,
	})
	_, _ = a.Wake(context.Background(), "first observation")
	_, _ = a.Wake(context.Background(), "second observation")
	// 2 user msgs + 2 assistant msgs = 4
	if got := a.HistoryLen(); got != 4 {
		t.Errorf("history len = %d, want 4", got)
	}
}

func TestExtractText_MultipleBlocks(t *testing.T) {
	m := llm.Message{Blocks: []llm.Block{
		{Text: "alpha"},
		{ToolUse: &llm.ToolUseBlock{Name: "x"}},
		{Text: "beta"},
	}}
	got := extractText(m)
	if got != "alpha beta" {
		t.Errorf("got %q", got)
	}
}

func TestSummarizeToolCall(t *testing.T) {
	use := &llm.ToolUseBlock{
		Name:  "read_file",
		Input: map[string]any{"path": "go.mod"},
	}
	got := summarizeToolCall(use)
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "go.mod") {
		t.Errorf("got %q", got)
	}
}

// ─── Tools tests ───

func TestReadFileTool(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi there"), 0644)

	tool := ReadFileTool(root)
	out, err := tool.Run(context.Background(), map[string]any{"path": "hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi there" {
		t.Errorf("got %q", out)
	}
}

func TestReadFileTool_RejectsEscape(t *testing.T) {
	root := t.TempDir()
	tool := ReadFileTool(root)
	_, err := tool.Run(context.Background(), map[string]any{"path": "../../../etc/passwd"})
	if err == nil {
		t.Error("expected escape rejection")
	}
}

func TestReadFileTool_RejectsAbsolute(t *testing.T) {
	root := t.TempDir()
	tool := ReadFileTool(root)
	_, err := tool.Run(context.Background(), map[string]any{"path": "/etc/passwd"})
	if err == nil {
		t.Error("expected absolute-path rejection")
	}
}

func TestListFilesTool_Glob(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.MkdirAll(filepath.Join(root, "proto", "user"), 0755)
	os.WriteFile(filepath.Join(root, "proto", "user", "user.proto"), []byte(""), 0644)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0644)

	tool := ListFilesTool(root)
	out, err := tool.Run(context.Background(), map[string]any{"glob": "**/*.proto"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "user.proto") {
		t.Errorf("got %q", out)
	}
	if strings.Contains(out, "main.go") {
		t.Errorf("globbed too broadly: %q", out)
	}
}

func TestSearchTextTool_FindsPattern(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package x\nfunc Hello() {}\n"), 0644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package y\n"), 0644)

	tool := SearchTextTool(root)
	out, err := tool.Run(context.Background(), map[string]any{"pattern": "func Hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("got %q", out)
	}
}

func TestExecTool_RunsCommand(t *testing.T) {
	root := t.TempDir()
	tool := ExecTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"command": "echo hello",
		"reason":  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("got %q", out)
	}
}

func TestExecTool_FailureIncludesOutput(t *testing.T) {
	root := t.TempDir()
	tool := ExecTool(root)
	out, err := tool.Run(context.Background(), map[string]any{
		"command": "this_command_does_not_exist_xyz",
	})
	if err == nil {
		t.Fatal("expected exit error")
	}
	_ = out
}

// TestStandardTools_ConcurrentAccess - sanity check that the agent's
// internal mutex doesn't deadlock with concurrent tool runs (tools run
// inside Wake under lock — so this just ensures back-to-back Wakes work).
func TestStandardTools_BackToBackWakes(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.WriteFile(filepath.Join(root, "x.txt"), []byte("body"), 0644)

	a := New(Config{
		ID:    "test",
		Tools: StandardTools(root),
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("read_file", map[string]any{"path": "x.txt"}, "u1"),
			llm.TextResponse("read it"),
			llm.ToolUseResponse("list_files", map[string]any{"glob": "*"}, "u2"),
			llm.TextResponse("listed"),
		),
		Print:   silentPrint,
		Confirm: alwaysConfirm,
	})

	if _, err := a.Wake(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wake(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
}

// Sanity that the agent type can be used from goroutines (it's protected
// by mutex; we just need a smoke test).
func TestAgent_GoroutineSafe(t *testing.T) {
	a := New(Config{
		ID: "test",
		Sender: llm.ScriptedSender(
			llm.TextResponse("a"),
			llm.TextResponse("b"),
		),
		Print: silentPrint,
	})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.Wake(context.Background(), "go")
		}()
	}
	wg.Wait()
}

// neverConfirm is a ConfirmFunc that always denies. Used to verify that
// PreConfirmAllow really skips the user prompt — if it doesn't, this
// would deny.
func neverConfirm(context.Context, string, string) bool { return false }

// TestAgent_PreConfirmAllow_SkipsUserConfirm verifies the v2 policy hook:
// PreConfirmAllow lets a tool call run without invoking ConfirmFunc.
//
// We set Confirm = neverConfirm to prove the user prompt was skipped —
// if it had been called, the tool wouldn't have run.
func TestAgent_PreConfirmAllow_SkipsUserConfirm(t *testing.T) {
	called := 0
	execTool := Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name:        "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "ran", nil
		},
	}

	preConfirmCalls := 0
	preConfirm := func(ctx context.Context, use *llm.ToolUseBlock) PreConfirmDecision {
		preConfirmCalls++
		return PreConfirmAllow
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "go build"}, "u1"),
			llm.TextResponse("ok"),
		),
		Print:      silentPrint,
		Confirm:    neverConfirm, // would deny if ever called
		PreConfirm: preConfirm,
	})

	_, err := a.Wake(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if preConfirmCalls != 1 {
		t.Errorf("PreConfirm calls = %d, want 1", preConfirmCalls)
	}
	if called != 1 {
		t.Errorf("tool calls = %d, want 1 (PreConfirmAllow should let it through)", called)
	}
}

// TestAgent_PreConfirmDeny_SkipsBothToolAndConfirm verifies that
// PreConfirmDeny rejects the tool without prompting the user.
func TestAgent_PreConfirmDeny_SkipsBothToolAndConfirm(t *testing.T) {
	called := 0
	execTool := Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name:        "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "ran", nil
		},
	}

	confirmCalls := 0
	confirm := func(ctx context.Context, name, summary string) bool {
		confirmCalls++
		return true
	}

	preConfirm := func(ctx context.Context, use *llm.ToolUseBlock) PreConfirmDecision {
		return PreConfirmDeny
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "rm -rf /"}, "u1"),
			llm.TextResponse("denied"),
		),
		Print:      silentPrint,
		Confirm:    confirm,
		PreConfirm: preConfirm,
	})

	_, err := a.Wake(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if confirmCalls != 0 {
		t.Errorf("Confirm should not have been called, got %d calls", confirmCalls)
	}
	if called != 0 {
		t.Errorf("Tool should not have run, got %d calls", called)
	}
}

// TestAgent_PreConfirmAsk_FallsThroughToConfirm verifies the default
// path: PreConfirmAsk delegates to ConfirmFunc.
func TestAgent_PreConfirmAsk_FallsThroughToConfirm(t *testing.T) {
	called := 0
	execTool := Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name:        "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "ran", nil
		},
	}

	confirmCalls := 0
	confirm := func(ctx context.Context, name, summary string) bool {
		confirmCalls++
		return true
	}

	preConfirm := func(ctx context.Context, use *llm.ToolUseBlock) PreConfirmDecision {
		return PreConfirmAsk
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "unusual-tool foo"}, "u1"),
			llm.TextResponse("ok"),
		),
		Print:      silentPrint,
		Confirm:    confirm,
		PreConfirm: preConfirm,
	})

	_, err := a.Wake(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if confirmCalls != 1 {
		t.Errorf("Confirm should have been called once, got %d", confirmCalls)
	}
	if called != 1 {
		t.Errorf("Tool should have run after Confirm approval, got %d calls", called)
	}
}

// TestAgent_PreConfirmNil_PreservesV1Behavior ensures that without a
// PreConfirm hook, the agent behaves exactly as v1: every mutating
// tool goes to ConfirmFunc.
func TestAgent_PreConfirmNil_PreservesV1Behavior(t *testing.T) {
	called := 0
	execTool := Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name:        "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			called++
			return "ran", nil
		},
	}

	confirmCalls := 0
	confirm := func(ctx context.Context, name, summary string) bool {
		confirmCalls++
		return true
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "go build"}, "u1"),
			llm.TextResponse("ok"),
		),
		Print:   silentPrint,
		Confirm: confirm,
		// PreConfirm: nil
	})

	_, err := a.Wake(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if confirmCalls != 1 {
		t.Errorf("Without PreConfirm, Confirm should fire once; got %d", confirmCalls)
	}
	if called != 1 {
		t.Errorf("tool calls = %d, want 1", called)
	}
}

// TestAgent_PreConfirm_ReceivesRawToolUse verifies the hook gets the
// actual ToolUseBlock with input map intact, not a stringified summary.
// This is what makes the v2 allow-list possible — it needs to inspect
// the raw command string, not a truncated summary.
func TestAgent_PreConfirm_ReceivesRawToolUse(t *testing.T) {
	execTool := Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name:        "exec",
			Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			return "ran", nil
		},
	}

	var receivedUse *llm.ToolUseBlock
	preConfirm := func(ctx context.Context, use *llm.ToolUseBlock) PreConfirmDecision {
		receivedUse = use
		return PreConfirmAllow
	}

	a := New(Config{
		ID:    "test",
		Tools: []Tool{execTool},
		Sender: llm.ScriptedSender(
			llm.ToolUseResponse("exec", map[string]any{"command": "go build ./...", "reason": "verify"}, "u1"),
			llm.TextResponse("ok"),
		),
		Print:      silentPrint,
		Confirm:    alwaysConfirm,
		PreConfirm: preConfirm,
	})

	_, err := a.Wake(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if receivedUse == nil {
		t.Fatal("PreConfirm did not receive a ToolUseBlock")
	}
	if receivedUse.Name != "exec" {
		t.Errorf("Name = %q, want exec", receivedUse.Name)
	}
	cmd, _ := receivedUse.Input["command"].(string)
	if cmd != "go build ./..." {
		t.Errorf("input[command] = %q, want 'go build ./...'", cmd)
	}
}
