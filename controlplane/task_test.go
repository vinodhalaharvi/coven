package controlplane

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

// registerTestAgent registers a minimal agent for task tests. The
// agent's Build is unused (v2 doesn't use Build), but Role is used by
// the task adapter.
func registerTestAgent(t *testing.T, name, role string) {
	t.Helper()
	registry.Register(registry.AgentSpec{
		Name:        name,
		Description: "test agent " + name,
		Role:        role,
		// No Detect/Build/etc — task adapter only uses Role.
	})
}

func TestTaskRunner_Run_RejectsMissingAgentName(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)

	tr := NewTaskRunner(nil, nil, nil, nil)
	_, err := tr.Run(context.Background(), Task{})
	if err == nil || !strings.Contains(err.Error(), "AgentName") {
		t.Errorf("expected AgentName error, got %v", err)
	}
}

func TestTaskRunner_Run_RejectsMissingWorktree(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)

	tr := NewTaskRunner(nil, nil, nil, nil)
	_, err := tr.Run(context.Background(), Task{AgentName: "anything"})
	if err == nil || !strings.Contains(err.Error(), "Worktree") {
		t.Errorf("expected Worktree error, got %v", err)
	}
}

func TestTaskRunner_Run_RejectsUnknownAgent(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)

	tr := NewTaskRunner(nil, nil, nil, nil)
	_, err := tr.Run(context.Background(), Task{
		AgentName: "nonexistent",
		Worktree:  "/tmp",
	})
	if err == nil || !strings.Contains(err.Error(), "no registered agent") {
		t.Errorf("expected unknown-agent error, got %v", err)
	}
}

func TestTaskRunner_Run_RejectsAgentMissingRole(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)

	registry.Register(registry.AgentSpec{
		Name: "no-role",
		// Role: "" — deliberately empty
	})

	tr := NewTaskRunner(nil, nil, nil, nil)
	_, err := tr.Run(context.Background(), Task{
		AgentName: "no-role",
		Worktree:  "/tmp",
	})
	if err == nil || !strings.Contains(err.Error(), "Role") {
		t.Errorf("expected missing-Role error, got %v", err)
	}
}

func TestTaskRunner_Run_InvokesAgentWithObservation(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	registry.Reset()
	t.Cleanup(registry.Reset)
	registerTestAgent(t, "alpha", "I am the alpha test agent. I do alpha things.")

	// Capture the observation Claude saw, by inspecting the conv passed
	// to our scripted sender.
	var capturedSystem string
	var capturedUser string
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		capturedSystem = system
		if len(conv) > 0 {
			for _, b := range conv[0].Blocks {
				if b.Text != "" {
					capturedUser += b.Text
				}
			}
		}
		// Return a simple final-text response — agent terminates.
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: "I have analyzed the task and concluded no work is needed."}},
		}, llm.StopEndTurn, nil
	}

	tr := NewTaskRunner(sender, nil, nil, silentPrint)

	final, err := tr.Run(context.Background(), Task{
		AgentName:  "alpha",
		Worktree:   wt.Path,
		Branch:     wt.Branch,
		Diff:       "diff --git a/foo b/foo\n+content\n",
		Why:        "alpha files were modified",
		BaseCommit: "abc123",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(final, "no work is needed") {
		t.Errorf("final output unexpected: %q", final)
	}

	// System prompt should contain the agent's role string.
	if !strings.Contains(capturedSystem, "I am the alpha test agent") {
		t.Errorf("system prompt missing role: %q", capturedSystem)
	}

	// User observation should reference task fields.
	for _, want := range []string{
		"alpha-agent",
		wt.Branch,
		wt.Path,
		"abc123",
		"alpha files were modified",
		"diff --git a/foo",
	} {
		if !strings.Contains(capturedUser, want) {
			t.Errorf("observation missing %q:\n%s", want, capturedUser)
		}
	}
}

func TestTaskRunner_PreConfirmHookGatesExec(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	registry.Reset()
	t.Cleanup(registry.Reset)
	registerTestAgent(t, "alpha", "alpha agent.")

	allowList := NewAllowList()

	// Track whether the user's ConfirmFunc was ever called.
	confirmCalled := 0
	confirm := func(ctx context.Context, name, summary string) bool {
		confirmCalled++
		return false // would deny if reached
	}

	// Script: first turn, Claude requests "go build" (in allow-list);
	// second turn, Claude returns final text.
	sender := llm.ScriptedSender(
		llm.ToolUseResponse("exec", map[string]any{"command": "go build ./..."}, "u1"),
		llm.TextResponse("done"),
	)

	tr := NewTaskRunner(sender, allowList, confirm, silentPrint)

	_, err = tr.Run(context.Background(), Task{
		AgentName: "alpha",
		Worktree:  wt.Path,
		Branch:    wt.Branch,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if confirmCalled != 0 {
		t.Errorf("Confirm was called %d times for allow-listed exec; should be 0", confirmCalled)
	}
}

func TestTaskRunner_PreConfirmFallsThroughForUnknownExec(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	registry.Reset()
	t.Cleanup(registry.Reset)
	registerTestAgent(t, "alpha", "alpha agent.")

	allowList := NewAllowList()

	confirmCalled := 0
	confirm := func(ctx context.Context, name, summary string) bool {
		confirmCalled++
		return true // approve
	}

	// Claude requests an out-of-allow-list command.
	sender := llm.ScriptedSender(
		llm.ToolUseResponse("exec", map[string]any{"command": "curl https://example.com"}, "u1"),
		llm.TextResponse("done"),
	)

	tr := NewTaskRunner(sender, allowList, confirm, silentPrint)

	_, err = tr.Run(context.Background(), Task{
		AgentName: "alpha",
		Worktree:  wt.Path,
		Branch:    wt.Branch,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if confirmCalled != 1 {
		t.Errorf("Confirm calls = %d, want 1 (out-of-allow-list curl should fall through)", confirmCalled)
	}
}

func TestTaskRunner_NilAllowListMeansNoPreConfirm(t *testing.T) {
	tr := NewTaskRunner(nil, nil, nil, nil)
	hook := tr.preConfirmFromAllowList()
	if hook != nil {
		t.Error("with nil allow-list, preConfirmFromAllowList should return nil")
	}
}

func TestBuildTaskObservation_IncludesAllFields(t *testing.T) {
	obs := buildTaskObservation(Task{
		AgentName:  "proto",
		Worktree:   "/tmp/wt",
		Branch:     "agent-proto/abc",
		Diff:       "diff content here",
		Why:        "proto file changed",
		BaseCommit: "deadbeef",
	})

	for _, want := range []string{
		"proto-agent",
		"/tmp/wt",
		"agent-proto/abc",
		"diff content here",
		"proto file changed",
		"deadbeef",
		"commit",
	} {
		if !strings.Contains(obs, want) {
			t.Errorf("observation missing %q:\n%s", want, obs)
		}
	}
}

func TestBuildTaskObservation_HandlesEmptyOptionalFields(t *testing.T) {
	// No Why, no BaseCommit, no Diff — should still produce a usable
	// observation, not crash.
	obs := buildTaskObservation(Task{
		AgentName: "test",
		Worktree:  "/tmp/x",
		Branch:    "agent-test/zz",
	})
	if obs == "" {
		t.Error("empty observation for minimal task")
	}
	if strings.Contains(obs, "Routing reason:") {
		t.Error("should not show 'Routing reason:' when Why is empty")
	}
}

// TestTaskRunner_ToolsAreRootedAtWorktree verifies the agent's
// StandardTools see the worktree, not the parent project. This is
// the core isolation property — agents can't accidentally read or
// write files outside their assigned worktree.
//
// We test this end-to-end: provision a worktree, write a file in it
// that doesn't exist in the parent, have Claude invoke a tool that
// reads it, verify the tool finds the worktree-scoped file.
func TestTaskRunner_ToolsAreRootedAtWorktree(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Create a file inside the worktree only.
	worktreeOnly := filepath.Join(wt.Path, "only-in-worktree.txt")
	if err := exec.Command("sh", "-c", "echo worktree-content > "+worktreeOnly).Run(); err != nil {
		t.Fatalf("setup: %v", err)
	}

	registry.Reset()
	t.Cleanup(registry.Reset)
	registerTestAgent(t, "alpha", "alpha agent.")

	// Track the tool input the agent received.
	var toolReadResult string

	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		// First call: ask agent to read the file.
		// Second call: extract result from previous tool_result.
		// Third call: emit final text.
		// Use turn count via conv length as a heuristic.
		userTurns := 0
		for _, m := range conv {
			if m.Role == llm.RoleUser {
				userTurns++
			}
		}
		switch userTurns {
		case 1:
			return llm.ToolUseResponse("read_file", map[string]any{"path": "only-in-worktree.txt"}, "u1").Message, llm.StopToolUse, nil
		case 2:
			// The tool_result from prev turn should contain the file's contents.
			for _, m := range conv {
				if m.Role == llm.RoleUser {
					for _, b := range m.Blocks {
						if b.ToolResult != nil {
							toolReadResult = b.ToolResult.Content
						}
					}
				}
			}
			return llm.Message{
				Role:   llm.RoleAssistant,
				Blocks: []llm.Block{{Text: "done"}},
			}, llm.StopEndTurn, nil
		default:
			return llm.Message{
				Role:   llm.RoleAssistant,
				Blocks: []llm.Block{{Text: "fallback"}},
			}, llm.StopEndTurn, nil
		}
	}

	tr := NewTaskRunner(sender, nil, agent.ConfirmFunc(func(context.Context, string, string) bool { return true }), silentPrint)

	_, err = tr.Run(context.Background(), Task{
		AgentName: "alpha",
		Worktree:  wt.Path,
		Branch:    wt.Branch,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(toolReadResult, "worktree-content") {
		t.Errorf("read_file did not see worktree-only file; got %q", toolReadResult)
	}
}

// silentPrint is duplicated from agent_test.go for use in this package.
func silentPrint(string) {}

func TestBuildTaskObservation_IncludesIntent(t *testing.T) {
	obs := buildTaskObservation(Task{
		AgentName: "test",
		Worktree:  "/tmp/wt",
		Branch:    "agent-test/abc",
		Diff:      "some diff",
		Intent:    "I removed Multiply by accident, please restore it",
	})
	if !strings.Contains(obs, "USER'S STATED INTENT:") {
		t.Errorf("observation should contain intent header:\n%s", obs)
	}
	if !strings.Contains(obs, "removed Multiply by accident") {
		t.Errorf("observation should contain intent text:\n%s", obs)
	}
	// Should also explain how to use it.
	if !strings.Contains(obs, "disambiguat") {
		t.Errorf("observation should explain how intent disambiguates:\n%s", obs)
	}
}

func TestBuildTaskObservation_NoIntentSectionWhenEmpty(t *testing.T) {
	obs := buildTaskObservation(Task{
		AgentName: "test",
		Worktree:  "/tmp/wt",
		Branch:    "agent-test/abc",
		Diff:      "some diff",
		// Intent left empty
	})
	if strings.Contains(obs, "USER'S STATED INTENT") {
		t.Errorf("observation should not have intent section when empty:\n%s", obs)
	}
}
