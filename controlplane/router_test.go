package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

// setupRegistry resets the registry and adds a small fixed set of agents
// for testing. Returns the names that were registered (in order).
func setupRegistry(t *testing.T) []string {
	t.Helper()
	registry.Reset()

	for _, spec := range []registry.AgentSpec{
		{
			Name:            "alpha",
			Description:     "test agent alpha",
			TypicalTriggers: "Changes to .alpha files",
			DomainFiles:     "alpha/*.go",
		},
		{
			Name:            "beta",
			Description:     "test agent beta",
			TypicalTriggers: "Changes to .beta files",
			DomainFiles:     "beta/*.go",
		},
		{
			Name:            "gamma",
			Description:     "test agent gamma",
			TypicalTriggers: "Changes to .gamma files",
		},
	} {
		registry.Register(spec)
	}

	t.Cleanup(func() {
		registry.Reset()
	})

	return []string{"alpha", "beta", "gamma"}
}

func TestRoute_EmptyDiffReturnsNoAgentsWithoutLLMCall(t *testing.T) {
	setupRegistry(t)

	// Sender that fails if called — empty diff should never reach LLM.
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		t.Fatal("LLM should not be called for empty diff")
		return llm.Message{}, llm.StopEndTurn, nil
	}

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 0 {
		t.Errorf("expected empty agents for empty diff, got %v", routing.Agents)
	}
}

func TestRoute_WhitespaceDiffReturnsNoAgentsWithoutLLMCall(t *testing.T) {
	setupRegistry(t)
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		t.Fatal("LLM should not be called for whitespace-only diff")
		return llm.Message{}, llm.StopEndTurn, nil
	}

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "   \n\t  \n")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 0 {
		t.Errorf("expected empty agents, got %v", routing.Agents)
	}
}

func TestRoute_ValidJSONResponseRoutesToNamedAgents(t *testing.T) {
	setupRegistry(t)

	sender := llm.ScriptedSender(
		llm.TextResponse(`{"agents": ["alpha", "beta"], "reasoning": "diff touches both alpha and beta files"}`),
	)

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "diff --git a/alpha/foo.go b/alpha/foo.go\n--- a/alpha/foo.go\n+++ b/alpha/foo.go\n@@ -1,1 +1,2 @@\n+new line\n")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d: %v", len(routing.Agents), routing.Agents)
	}
	if routing.Agents[0] != "alpha" || routing.Agents[1] != "beta" {
		t.Errorf("unexpected agents: %v", routing.Agents)
	}
	if !strings.Contains(routing.Reasoning, "alpha and beta") {
		t.Errorf("reasoning not preserved: %q", routing.Reasoning)
	}
}

func TestRoute_HallucinatedAgentNamesAreFiltered(t *testing.T) {
	setupRegistry(t)

	// alpha and beta are real; "delta" and "epsilon" are not registered.
	sender := llm.ScriptedSender(
		llm.TextResponse(`{"agents": ["alpha", "delta", "beta", "epsilon"], "reasoning": "..."}`),
	)

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "diff --git a/foo b/foo\n--- a/foo\n+++ b/foo\n@@ -1 +1 @@\n+x\n")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 2 {
		t.Errorf("expected 2 agents after filtering, got %d: %v", len(routing.Agents), routing.Agents)
	}
	for _, name := range routing.Agents {
		if name != "alpha" && name != "beta" {
			t.Errorf("unexpected agent in result: %q", name)
		}
	}
}

func TestRoute_EmptyAgentListIsValidResponse(t *testing.T) {
	setupRegistry(t)

	sender := llm.ScriptedSender(
		llm.TextResponse(`{"agents": [], "reasoning": "diff is doc-only and no docs agent registered"}`),
	)

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "diff --git a/README.md b/README.md\n+typo fix\n")
	if err != nil {
		t.Fatalf("Route returned error on empty agent list: %v", err)
	}
	if len(routing.Agents) != 0 {
		t.Errorf("expected empty agents, got %v", routing.Agents)
	}
}

func TestRoute_HandlesMarkdownFencedJSON(t *testing.T) {
	setupRegistry(t)

	// Some Claude responses wrap JSON in markdown fences. Router should
	// handle this gracefully.
	sender := llm.ScriptedSender(
		llm.TextResponse("```json\n{\"agents\": [\"alpha\"], \"reasoning\": \"...\"}\n```"),
	)

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "diff content")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 1 || routing.Agents[0] != "alpha" {
		t.Errorf("expected [alpha], got %v", routing.Agents)
	}
}

func TestRoute_HandlesProseBeforeJSON(t *testing.T) {
	setupRegistry(t)

	// Sometimes Claude prefixes JSON with prose. Should still parse.
	sender := llm.ScriptedSender(
		llm.TextResponse("Looking at the diff, here is my routing decision:\n\n{\"agents\": [\"beta\"], \"reasoning\": \"diff is in beta domain\"}"),
	)

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "diff content")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 1 || routing.Agents[0] != "beta" {
		t.Errorf("expected [beta], got %v", routing.Agents)
	}
}

func TestRoute_MalformedJSONReturnsError(t *testing.T) {
	setupRegistry(t)

	sender := llm.ScriptedSender(
		llm.TextResponse("This is not JSON and contains no JSON object."),
	)

	r := NewRouter(sender)
	_, err := r.Route(context.Background(), "diff content")
	if err == nil {
		t.Fatal("expected error on malformed response, got nil")
	}
	if !strings.Contains(err.Error(), "parsing response") {
		t.Errorf("expected parsing error, got: %v", err)
	}
}

func TestRoute_NoAgentsRegisteredReturnsEmpty(t *testing.T) {
	registry.Reset()
	t.Cleanup(func() { registry.Reset() })

	// LLM should not be called when registry is empty.
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		t.Fatal("LLM should not be called when registry is empty")
		return llm.Message{}, llm.StopEndTurn, nil
	}

	r := NewRouter(sender)
	routing, err := r.Route(context.Background(), "diff content")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if len(routing.Agents) != 0 {
		t.Errorf("expected empty agents, got %v", routing.Agents)
	}
}

func TestSystemPromptIncludesAgentMetadata(t *testing.T) {
	setupRegistry(t)
	specs := registry.All()

	prompt := buildRouterSystemPrompt(specs)

	// Verify each agent's name and description appears.
	for _, s := range specs {
		if !strings.Contains(prompt, s.Name) {
			t.Errorf("system prompt missing agent name %q", s.Name)
		}
		if !strings.Contains(prompt, s.Description) {
			t.Errorf("system prompt missing description for %q", s.Name)
		}
		if s.TypicalTriggers != "" && !strings.Contains(prompt, s.TypicalTriggers) {
			t.Errorf("system prompt missing typical_triggers for %q", s.Name)
		}
	}

	// Verify the prompt instructs JSON output.
	if !strings.Contains(prompt, `{"agents":`) {
		t.Error("system prompt should show JSON output format")
	}
}

func TestUserPromptIncludesDiff(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n+test\n"
	prompt := buildRouterUserPrompt(diff, "")

	if !strings.Contains(prompt, diff) {
		t.Error("user prompt should embed the diff")
	}
	if !strings.Contains(prompt, "JSON only") {
		t.Error("user prompt should re-emphasize JSON-only output")
	}
	if strings.Contains(prompt, "USER'S INTENT") {
		t.Error("with empty intent, prompt should not have intent section")
	}
}

func TestUserPromptWithIntent_IncludesIntent(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n+test\n"
	intent := "I removed Multiply by accident — please restore it"
	prompt := buildRouterUserPrompt(diff, intent)

	if !strings.Contains(prompt, diff) {
		t.Error("user prompt should still embed the diff")
	}
	if !strings.Contains(prompt, "USER'S INTENT") {
		t.Error("prompt should have intent header when intent is non-empty")
	}
	if !strings.Contains(prompt, "removed Multiply") {
		t.Error("prompt should contain the intent text")
	}
	if !strings.Contains(prompt, "disambiguat") {
		t.Error("prompt should explain how to use intent")
	}
}

// TestRouteWithIntent_PassesIntentToLLM verifies end-to-end that
// when RouteWithIntent is called, the user's intent appears in the
// prompt sent to the LLM.
func TestRouteWithIntent_PassesIntentToLLM(t *testing.T) {
	setupRegistry(t)

	var capturedUserPrompt string
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		// The router sends a single user message; capture its text.
		for _, m := range conv {
			if m.Role == llm.RoleUser {
				for _, b := range m.Blocks {
					capturedUserPrompt += b.Text
				}
			}
		}
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: `{"agents":["alpha"],"reasoning":"x"}`}},
		}, llm.StopEndTurn, nil
	}

	r := NewRouter(sender)
	intent := "I deleted Multiply by mistake; please restore the function"
	_, err := r.RouteWithIntent(context.Background(), "diff content here", intent)
	if err != nil {
		t.Fatalf("RouteWithIntent: %v", err)
	}

	if !strings.Contains(capturedUserPrompt, "deleted Multiply by mistake") {
		t.Errorf("LLM didn't see the intent in its prompt:\n%s", capturedUserPrompt)
	}
	if !strings.Contains(capturedUserPrompt, "USER'S INTENT") {
		t.Errorf("LLM prompt missing intent section:\n%s", capturedUserPrompt)
	}
}

func TestParseRoutingResponse_ExtractsCleanJSON(t *testing.T) {
	cases := map[string]string{
		"plain":               `{"agents":["a"],"reasoning":"x"}`,
		"with markdown fence": "```json\n{\"agents\":[\"a\"],\"reasoning\":\"x\"}\n```",
		"with prose before":   "Here you go:\n{\"agents\":[\"a\"],\"reasoning\":\"x\"}",
		"with prose after":    "{\"agents\":[\"a\"],\"reasoning\":\"x\"}\nHope this helps!",
		"with whitespace":     "  \n  {\"agents\":[\"a\"],\"reasoning\":\"x\"}  \n  ",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := parseRoutingResponse(input)
			if err != nil {
				t.Errorf("parse failed: %v", err)
				return
			}
			if len(r.Agents) != 1 || r.Agents[0] != "a" {
				t.Errorf("expected [a], got %v", r.Agents)
			}
		})
	}
}
