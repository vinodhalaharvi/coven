// Package agent provides the conversational agent runtime. A
// ConversationalAgent has a hardcoded role, a domain regex (used to
// decide when it wakes up), a toolbox, and a persistent conversation
// with Claude.
//
// The loop is simple:
//
//   wake(observation)
//     → append observation to conversation as user message
//     → loop:
//         send conversation + tools to Claude
//         if response has tool_use: dispatch each, append results, continue
//         if response is final text: agent at equilibrium, stop
//
// The exec tool is the only one that requires user confirmation. Every
// other tool (read_file, list_files, etc.) runs without prompting because
// they are pure observations — they don't mutate the project.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/llm"
)

// Tool is one capability the agent exposes to Claude. Pure tools (Pure=true)
// run without confirmation; mutating tools require user y/N.
type Tool struct {
	Spec    llm.ToolSpec
	Pure    bool
	Run     func(ctx context.Context, input map[string]any) (string, error)
}

// ConfirmFunc gates execution of mutating tools. Returns true to allow.
// The default is os.Stdin-based; tests inject a fake.
type ConfirmFunc func(ctx context.Context, toolName, summary string) bool

// PrintFunc is how the agent communicates with the user. Defaults to fmt.Print.
type PrintFunc func(string)

// Config configures a ConversationalAgent.
type Config struct {
	ID       string         // stable identifier, used in fact keys and logs
	Role     string         // hardcoded system prompt fragment ("I am the proto agent...")
	Tools    []Tool         // toolbox; must include all tools the role might need
	Sender   llm.Sender     // backend
	Confirm  ConfirmFunc    // gate on mutating tools; nil = always deny (safe default)
	Print    PrintFunc      // user output; nil = fmt.Print
	MaxTurns int            // safety cap on conversation turns per wakeup; default 30
}

// Agent is a long-lived conversational agent.
type Agent struct {
	cfg          Config
	mu           sync.Mutex
	conversation []llm.Message // grows across wakeups; survives equilibrium
}

// New constructs a ConversationalAgent.
func New(cfg Config) *Agent {
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = 30
	}
	if cfg.Print == nil {
		cfg.Print = func(s string) { fmt.Print(s) }
	}
	if cfg.Confirm == nil {
		// Safe default: no confirmation function = no mutating tool runs.
		cfg.Confirm = func(ctx context.Context, name, summary string) bool {
			return false
		}
	}
	return &Agent{cfg: cfg}
}

// Wake feeds an observation to the agent and runs the conversation
// loop until Claude returns a final non-tool message. Each call to Wake
// continues the same conversation (the agent's memory persists across
// wakeups within a process).
//
// Returns the final assistant text and any error from the run.
func (a *Agent) Wake(ctx context.Context, observation string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// On first wake, no system has been sent yet — but the system prompt
	// is sent on every API call (Anthropic's design); we just need to
	// seed the conversation with the user's observation.
	a.conversation = append(a.conversation, llm.Message{
		Role:   llm.RoleUser,
		Blocks: []llm.Block{{Text: observation}},
	})

	tools := make([]llm.ToolSpec, 0, len(a.cfg.Tools))
	for _, t := range a.cfg.Tools {
		tools = append(tools, t.Spec)
	}

	// System prompt: role + a short note about how tools and confirmation work.
	system := a.cfg.Role + "\n\n" + systemSuffix

	for turn := 0; turn < a.cfg.MaxTurns; turn++ {
		msg, stop, err := a.cfg.Sender(ctx, system, a.conversation, tools)
		if err != nil {
			return "", fmt.Errorf("sender turn %d: %w", turn, err)
		}
		// Always append the assistant message to history (even if it has tool_use).
		a.conversation = append(a.conversation, msg)

		// If the assistant included any text alongside tool calls, surface it
		// to the user — this is how Claude narrates its plan ("let me check
		// the buf config first") and waiting agents shouldn't look hung.
		if midText := extractText(msg); midText != "" && stop == llm.StopToolUse {
			a.cfg.Print(fmt.Sprintf("  [%s] %s\n", a.cfg.ID, midText))
		}

		if stop != llm.StopToolUse {
			// Final response. Print any text and return.
			final := extractText(msg)
			if final != "" {
				a.cfg.Print(fmt.Sprintf("  [%s] %s\n", a.cfg.ID, final))
			} else {
				a.cfg.Print(fmt.Sprintf("  [%s] (no final message; stop_reason=%s)\n", a.cfg.ID, stop))
			}
			return final, nil
		}

		// Stop reason was tool_use: dispatch every tool_use block in the message.
		toolResults := make([]llm.Block, 0)
		for _, b := range msg.Blocks {
			if b.ToolUse == nil {
				continue
			}
			result, isErr := a.runTool(ctx, b.ToolUse)
			toolResults = append(toolResults, llm.Block{
				ToolResult: &llm.ToolResultBlock{
					ToolUseID: b.ToolUse.ID,
					Content:   result,
					IsError:   isErr,
				},
			})
		}
		if len(toolResults) == 0 {
			// Sender said tool_use but no tool_use blocks — protocol bug.
			return "", fmt.Errorf("turn %d: stop_reason=tool_use but no tool_use blocks", turn)
		}
		// Feed results back as a user-role message and continue the loop.
		a.conversation = append(a.conversation, llm.Message{
			Role:   llm.RoleUser,
			Blocks: toolResults,
		})
	}

	return "", fmt.Errorf("agent %s exceeded max turns (%d) — possible loop", a.cfg.ID, a.cfg.MaxTurns)
}

// runTool dispatches a single tool_use call. Returns (content, isError).
func (a *Agent) runTool(ctx context.Context, use *llm.ToolUseBlock) (string, bool) {
	tool, ok := a.findTool(use.Name)
	if !ok {
		return fmt.Sprintf("unknown tool: %s", use.Name), true
	}

	// Confirmation gate for mutating tools.
	if !tool.Pure {
		summary := summarizeToolCall(use)
		if !a.cfg.Confirm(ctx, use.Name, summary) {
			return "user denied execution of " + use.Name, true
		}
	}

	// Surface what we're doing for pure tools too — keeps the user
	// informed without prompting.
	if tool.Pure {
		a.cfg.Print(fmt.Sprintf("  [%s] tool: %s\n", a.cfg.ID, summarizeToolCall(use)))
	}

	out, err := tool.Run(ctx, use.Input)
	if err != nil {
		return fmt.Sprintf("error: %v\n%s", err, out), true
	}
	return out, false
}

func (a *Agent) findTool(name string) (Tool, bool) {
	for _, t := range a.cfg.Tools {
		if t.Spec.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// HistoryLen returns the number of messages in the conversation. For tests
// and for diagnostic UIs.
func (a *Agent) HistoryLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conversation)
}

// Reset clears the conversation history. Use sparingly — typically
// only when the project's ground state has changed enough that prior
// reasoning is invalid.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conversation = nil
}

const systemSuffix = `You have access to a set of tools. Pure tools (e.g. read_file, list_files) run automatically. Tools that mutate the project (e.g. exec) require explicit user confirmation — assume any exec call may be denied.

When you have completed your task and the project state is consistent in your domain, return a single final text message describing what you did and what the user should know. Don't pad with apologies or restatements; one or two sentences is enough.

If you cannot make progress because something requires human judgment (e.g. ambiguous code edits, missing credentials), say so plainly in a final text message and stop.`

// extractText concatenates all text blocks in a message.
func extractText(m llm.Message) string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// summarizeToolCall returns a short human-readable form of a tool call,
// e.g. `read_file(path="go.mod")`. Used both for the confirmation prompt
// and for the running-this-tool log line.
func summarizeToolCall(use *llm.ToolUseBlock) string {
	if len(use.Input) == 0 {
		return use.Name + "()"
	}
	parts := make([]string, 0, len(use.Input))
	for k, v := range use.Input {
		switch vv := v.(type) {
		case string:
			parts = append(parts, fmt.Sprintf("%s=%q", k, truncate(vv, 80)))
		default:
			b, _ := json.Marshal(vv)
			parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(string(b), 80)))
		}
	}
	return use.Name + "(" + strings.Join(parts, ", ") + ")"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// timeNow exists so tests can mock time if they ever need to. Currently
// unused but kept for future use; safe to delete if it stays unused.
var timeNow = func() time.Time { return time.Now() }
