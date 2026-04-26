package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScriptedSender_FinalText(t *testing.T) {
	sender := ScriptedSender(TextResponse("done"))
	msg, stop, err := sender(context.Background(), "system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stop != StopEndTurn {
		t.Errorf("stop = %s, want end_turn", stop)
	}
	if len(msg.Blocks) != 1 || msg.Blocks[0].Text != "done" {
		t.Errorf("unexpected msg: %+v", msg)
	}
}

func TestScriptedSender_ToolUseThenText(t *testing.T) {
	sender := ScriptedSender(
		ToolUseResponse("read_file", map[string]any{"path": "go.mod"}, "u1"),
		TextResponse("ok done"),
	)

	// First call returns a tool_use.
	msg1, stop1, _ := sender(context.Background(), "", nil, nil)
	if stop1 != StopToolUse {
		t.Errorf("stop1 = %s", stop1)
	}
	if msg1.Blocks[0].ToolUse == nil {
		t.Fatal("expected tool_use block")
	}
	if msg1.Blocks[0].ToolUse.Name != "read_file" {
		t.Errorf("name = %s", msg1.Blocks[0].ToolUse.Name)
	}

	// Second call returns final text.
	msg2, stop2, _ := sender(context.Background(), "", nil, nil)
	if stop2 != StopEndTurn {
		t.Errorf("stop2 = %s", stop2)
	}
	if msg2.Blocks[0].Text != "ok done" {
		t.Errorf("text = %s", msg2.Blocks[0].Text)
	}
}

func TestScriptedSender_Exhausted(t *testing.T) {
	sender := ScriptedSender(TextResponse("first"))
	_, _, _ = sender(context.Background(), "", nil, nil)
	_, _, err := sender(context.Background(), "", nil, nil)
	if err == nil {
		t.Error("expected exhaustion error")
	}
}

func TestConvertMessages_HandlesAllBlockKinds(t *testing.T) {
	conv := []Message{
		{Role: RoleUser, Blocks: []Block{{Text: "hello"}}},
		{Role: RoleAssistant, Blocks: []Block{
			{Text: "let me check"},
			{ToolUse: &ToolUseBlock{ID: "u1", Name: "read", Input: map[string]any{"x": 1}}},
		}},
		{Role: RoleUser, Blocks: []Block{
			{ToolResult: &ToolResultBlock{ToolUseID: "u1", Content: "the contents"}},
		}},
	}
	out := convertMessages(conv)
	if len(out) != 3 {
		t.Fatalf("got %d messages", len(out))
	}
	// Last should be a tool_result.
	last := out[2]["content"].([]map[string]any)[0]
	if last["type"] != "tool_result" {
		t.Errorf("last block type = %v", last["type"])
	}
	if last["tool_use_id"] != "u1" {
		t.Errorf("tool_use_id = %v", last["tool_use_id"])
	}
}

func TestClaudeConversation_HappyPath_TextOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity: tools field present in body if we sent any.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "hello back"}],
			"stop_reason": "end_turn"
		}`))
	}))
	defer server.Close()

	send := ClaudeConversation(ClaudeConfig{
		Model:    "test",
		APIKey:   "test-key",
		Endpoint: server.URL,
		HTTP:     server.Client(),
	})
	msg, stop, err := send(context.Background(), "system", []Message{
		{Role: RoleUser, Blocks: []Block{{Text: "hi"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stop != StopEndTurn {
		t.Errorf("stop = %s", stop)
	}
	if len(msg.Blocks) != 1 || msg.Blocks[0].Text != "hello back" {
		t.Errorf("unexpected msg: %+v", msg)
	}
}

func TestClaudeConversation_ToolUseRoundtrip(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [
				{"type": "text", "text": "let me check"},
				{"type": "tool_use", "id": "u_xyz", "name": "read_file", "input": {"path": "go.mod"}}
			],
			"stop_reason": "tool_use"
		}`))
	}))
	defer server.Close()

	send := ClaudeConversation(ClaudeConfig{
		Model: "t", APIKey: "k", Endpoint: server.URL, HTTP: server.Client(),
	})
	msg, stop, err := send(context.Background(), "you are an agent", []Message{
		{Role: RoleUser, Blocks: []Block{{Text: "what's in go.mod?"}}},
	}, []ToolSpec{
		{
			Name:        "read_file",
			Description: "read a file",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stop != StopToolUse {
		t.Errorf("stop = %s", stop)
	}
	// Should have text + tool_use blocks.
	if len(msg.Blocks) != 2 {
		t.Fatalf("got %d blocks: %+v", len(msg.Blocks), msg.Blocks)
	}
	if msg.Blocks[0].Text != "let me check" {
		t.Errorf("first block text = %s", msg.Blocks[0].Text)
	}
	if msg.Blocks[1].ToolUse == nil {
		t.Fatal("second block should be tool_use")
	}
	if msg.Blocks[1].ToolUse.Name != "read_file" {
		t.Errorf("tool name = %s", msg.Blocks[1].ToolUse.Name)
	}
	if msg.Blocks[1].ToolUse.Input["path"] != "go.mod" {
		t.Errorf("input path = %v", msg.Blocks[1].ToolUse.Input["path"])
	}

	// The body sent to the server should include tools and system.
	if capturedBody["system"] != "you are an agent" {
		t.Errorf("system = %v", capturedBody["system"])
	}
	tools, _ := capturedBody["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools count = %d", len(tools))
	}
}

func TestClaudeConversation_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limit"}`))
	}))
	defer server.Close()

	send := ClaudeConversation(ClaudeConfig{
		Model: "t", APIKey: "k", Endpoint: server.URL, HTTP: server.Client(),
	})
	_, _, err := send(context.Background(), "", []Message{
		{Role: RoleUser, Blocks: []Block{{Text: "hi"}}},
	}, nil)
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v", err)
	}
}

// Sanity: existing single-shot LLM helpers still compile and work
// alongside the new Sender type.
func TestExistingStaticAndStructuredStillWork(t *testing.T) {
	raw := Static(`{"x":42}`)
	type T struct {
		X int `json:"x"`
	}
	get := Structured[T](raw, "")
	v, err := get(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if v.X != 42 {
		t.Errorf("got %d", v.X)
	}
}
