// This file adds tool-use conversation support to the llm package,
// alongside the existing single-shot Static/Claude/Structured helpers.
// It does not modify any existing API; everything here is additive.
//
// The shape: a Conversation is a list of Messages. Each Message has a
// Role (user/assistant) and Blocks (text or tool_use or tool_result).
// A Sender takes a Conversation and a list of available Tools and returns
// the next assistant Message — which may contain tool_use blocks for the
// caller to execute and feed back as tool_results in the next turn.
//
// We deliberately model this as plain data + a function type rather than
// an interface, matching the style of LLM. ClaudeConversation is one
// concrete Sender; tests use a Scripted-style fake.

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Role identifies who produced a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Block is one segment of a message. Exactly one of Text, ToolUse, or
// ToolResult is populated.
type Block struct {
	Text       string         `json:"text,omitempty"`
	ToolUse    *ToolUseBlock  `json:"tool_use,omitempty"`
	ToolResult *ToolResultBlock `json:"tool_result,omitempty"`
}

// ToolUseBlock is the assistant's request to invoke a tool.
type ToolUseBlock struct {
	ID    string                 `json:"id"`
	Name  string                 `json:"name"`
	Input map[string]any         `json:"input"`
}

// ToolResultBlock carries the caller's response to a prior ToolUse.
type ToolResultBlock struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one turn in a conversation.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// ToolSpec describes a tool the assistant may call. The InputSchema is
// a JSON Schema fragment (object with properties); Anthropic's API
// validates calls against it.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// StopReason indicates why the assistant stopped.
type StopReason string

const (
	StopEndTurn  StopReason = "end_turn"   // model finished naturally
	StopToolUse  StopReason = "tool_use"   // model wants tools executed
	StopMaxTokens StopReason = "max_tokens" // truncated
)

// Sender is the function-typed seam over a tool-use-capable backend.
// Given a system prompt, conversation history, and tool catalog, returns
// the next assistant message and stop reason. Errors from this seam are
// transport/protocol errors, not the model's response.
type Sender = func(ctx context.Context, system string, conv []Message, tools []ToolSpec) (Message, StopReason, error)

// ClaudeConversation returns a Sender backed by the Anthropic Messages API.
// Reuses ClaudeConfig from the single-shot path.
func ClaudeConversation(cfg ClaudeConfig) Sender {
	if cfg.APIKey == "" {
		// fall through to env in the call (so tests can set after construction)
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 2048
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.anthropic.com/v1/messages"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.Model == "" {
		cfg.Model = ClaudeSonnet
	}

	return func(ctx context.Context, system string, conv []Message, tools []ToolSpec) (Message, StopReason, error) {
		body := map[string]any{
			"model":      cfg.Model,
			"max_tokens": cfg.MaxTokens,
			"messages":   convertMessages(conv),
		}
		if system != "" {
			body["system"] = system
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}

		buf, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, "POST", cfg.Endpoint, bytes.NewReader(buf))
		if err != nil {
			return Message{}, "", err
		}
		key := cfg.APIKey
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")

		resp, err := cfg.HTTP.Do(req)
		if err != nil {
			return Message{}, "", err
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return Message{}, "", fmt.Errorf("claude api: status %d: %s", resp.StatusCode, truncate(string(respBody), 500))
		}

		var parsed struct {
			Content []struct {
				Type  string `json:"type"`
				Text  string `json:"text,omitempty"`
				ID    string `json:"id,omitempty"`
				Name  string `json:"name,omitempty"`
				Input json.RawMessage `json:"input,omitempty"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return Message{}, "", fmt.Errorf("decode response: %w", err)
		}

		msg := Message{Role: RoleAssistant}
		for _, c := range parsed.Content {
			switch c.Type {
			case "text":
				msg.Blocks = append(msg.Blocks, Block{Text: c.Text})
			case "tool_use":
				var input map[string]any
				_ = json.Unmarshal(c.Input, &input)
				msg.Blocks = append(msg.Blocks, Block{ToolUse: &ToolUseBlock{
					ID: c.ID, Name: c.Name, Input: input,
				}})
			}
		}
		return msg, StopReason(parsed.StopReason), nil
	}
}

// convertMessages flattens our []Message into the JSON Anthropic expects.
// Each Block becomes a content array entry of the appropriate type.
func convertMessages(conv []Message) []map[string]any {
	out := make([]map[string]any, 0, len(conv))
	for _, m := range conv {
		content := make([]map[string]any, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch {
			case b.ToolUse != nil:
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    b.ToolUse.ID,
					"name":  b.ToolUse.Name,
					"input": b.ToolUse.Input,
				})
			case b.ToolResult != nil:
				entry := map[string]any{
					"type":         "tool_result",
					"tool_use_id":  b.ToolResult.ToolUseID,
					"content":      b.ToolResult.Content,
				}
				if b.ToolResult.IsError {
					entry["is_error"] = true
				}
				content = append(content, entry)
			default:
				content = append(content, map[string]any{
					"type": "text",
					"text": b.Text,
				})
			}
		}
		out = append(out, map[string]any{
			"role":    string(m.Role),
			"content": content,
		})
	}
	return out
}

// ScriptedSender returns a Sender that returns the given messages in
// order, regardless of input. Useful for tests.
func ScriptedSender(responses ...ScriptedResponse) Sender {
	i := 0
	return func(ctx context.Context, system string, conv []Message, tools []ToolSpec) (Message, StopReason, error) {
		if i >= len(responses) {
			return Message{}, "", fmt.Errorf("scripted sender exhausted (got %d calls)", i+1)
		}
		r := responses[i]
		i++
		return r.Message, r.Stop, nil
	}
}

// ScriptedResponse is one scripted assistant turn for tests.
type ScriptedResponse struct {
	Message Message
	Stop    StopReason
}

// TextResponse is a convenience for ScriptedSender: a final assistant
// message containing only text, with stop_reason=end_turn.
func TextResponse(text string) ScriptedResponse {
	return ScriptedResponse{
		Message: Message{Role: RoleAssistant, Blocks: []Block{{Text: text}}},
		Stop:    StopEndTurn,
	}
}

// ToolUseResponse is a convenience for ScriptedSender: an assistant
// message containing a tool_use block, with stop_reason=tool_use.
func ToolUseResponse(toolName string, input map[string]any, id string) ScriptedResponse {
	if id == "" {
		id = "test-tool-" + toolName
	}
	return ScriptedResponse{
		Message: Message{Role: RoleAssistant, Blocks: []Block{{
			ToolUse: &ToolUseBlock{ID: id, Name: toolName, Input: input},
		}}},
		Stop: StopToolUse,
	}
}

// (no extra env helpers; we just use os.Getenv inline above)
