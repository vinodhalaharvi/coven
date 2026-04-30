// Router decides which agents should handle a given changeset, based on
// the git diff and the agents' registered metadata.
//
// The router is the heart of v2's "no static IsRelevant predicates"
// design. Instead of each agent's IsRelevant function deciding "is this
// path in my domain?", the router asks Claude once per changeset:
// "given this diff and these agents, who should handle this?"
//
// Why one LLM call instead of N IsRelevant predicates:
//
//   - The diff has semantic content that path-glob matching can't see.
//     Whitespace-only changes route to nobody. Comment-only changes
//     might route to docs but not to test-agent. The LLM reads what
//     actually changed.
//
//   - New file types and new agents Just Work — no glob updates, no
//     predicate code to maintain. The agent's registered Description
//     and TypicalTriggers tell Claude when it's relevant.
//
//   - Cross-file routing decisions emerge naturally. If a .proto change
//     plus its regenerated .pb.go arrives in one changeset, the LLM
//     recognizes them as a single logical change and routes once.
//
// The router is the highest-risk component in v2: bad routing means
// agents do unnecessary work or miss work they should do. The
// AgentSpec.TypicalTriggers / DomainFiles / AvoidsWhen / ExampleScenarios
// fields exist specifically to make the router's job tractable.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

// Routing is the result of a router call. Agents lists the agent names
// the router selected; Reasoning is the LLM's explanation (kept for
// observability/debugging — the rest of the system shouldn't depend on
// its content).
type Routing struct {
	Agents    []string `json:"agents"`
	Reasoning string   `json:"reasoning"`
}

// Router routes git diffs to agents.
type Router struct {
	sender llm.Sender
}

// NewRouter constructs a Router using the given LLM sender.
func NewRouter(sender llm.Sender) *Router {
	return &Router{sender: sender}
}

// Route asks the LLM which agents should handle the given diff. Returns
// a list of agent names that exist in the registry — hallucinated names
// are filtered out before returning. An empty list is a valid result
// and means "no agents need to run."
//
// Errors are returned only for unrecoverable problems (LLM API failure,
// malformed JSON the model couldn't be coaxed into producing). For
// soft problems (LLM picked a non-existent agent), the router silently
// drops the bad name and returns the rest.
func (r *Router) Route(ctx context.Context, diff string) (*Routing, error) {
	return r.RouteWithIntent(ctx, diff, "")
}

// RouteWithIntent is like Route but also takes the user's stated
// intent for the change. Intent (when non-empty) is added to the
// router's prompt so Claude can disambiguate diffs whose intent
// isn't obvious from the patch alone.
//
// Empty intent gives identical behavior to Route — useful for
// tests and code paths that don't have intent available.
func (r *Router) RouteWithIntent(ctx context.Context, diff, intent string) (*Routing, error) {
	if strings.TrimSpace(diff) == "" {
		// No changes — nothing to route. Return early without an LLM call.
		return &Routing{Agents: nil, Reasoning: "diff is empty"}, nil
	}

	specs := registry.All()
	if len(specs) == 0 {
		return &Routing{Agents: nil, Reasoning: "no agents registered"}, nil
	}

	system := buildRouterSystemPrompt(specs)
	user := buildRouterUserPrompt(diff, intent)

	msg, _, err := r.sender(ctx, system, []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Text: user}}},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("router LLM call: %w", err)
	}

	// Extract text response. The router doesn't use tools, so we expect
	// a single text block.
	var text string
	for _, b := range msg.Blocks {
		if b.Text != "" {
			text += b.Text
		}
	}
	if text == "" {
		return nil, fmt.Errorf("router: empty LLM response")
	}

	routing, err := parseRoutingResponse(text)
	if err != nil {
		return nil, fmt.Errorf("router: parsing response: %w (raw: %q)", err, truncateForError(text, 200))
	}

	// Filter hallucinated agent names. A registered name is one that
	// appears in registry.All().
	known := make(map[string]bool, len(specs))
	for _, s := range specs {
		known[s.Name] = true
	}
	filtered := make([]string, 0, len(routing.Agents))
	for _, name := range routing.Agents {
		if known[name] {
			filtered = append(filtered, name)
		}
		// Silent drop on unknown — could log this if a debug path exists.
	}
	routing.Agents = filtered

	return routing, nil
}

// parseRoutingResponse extracts the JSON object from the LLM's text
// response. We try to be forgiving: if the LLM wraps the JSON in
// markdown fences or prepends prose, we still find the JSON object.
func parseRoutingResponse(text string) (*Routing, error) {
	// Strip common markdown fences.
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	// If there's leading prose before the JSON, find the first '{'
	// and use from there to the last '}'.
	if !strings.HasPrefix(text, "{") {
		if i := strings.Index(text, "{"); i >= 0 {
			text = text[i:]
		}
	}
	if !strings.HasSuffix(text, "}") {
		if i := strings.LastIndex(text, "}"); i >= 0 {
			text = text[:i+1]
		}
	}

	var routing Routing
	if err := json.Unmarshal([]byte(text), &routing); err != nil {
		return nil, err
	}
	return &routing, nil
}

// buildRouterSystemPrompt assembles the system prompt: explanation of
// the router's job + the agent registry as structured context.
func buildRouterSystemPrompt(specs []registry.AgentSpec) string {
	var b strings.Builder
	b.WriteString(`You are the routing layer for a multi-agent code-generation system. Your job is to decide which agents should handle a given git diff.

You will receive:
  1. A git diff representing changes to a project.
  2. (Below) a list of agents, each with a name, description, and contextual fields describing when they should be invoked.

Your output is JSON of the form:
  {"agents": ["agent-name-1", "agent-name-2"], "reasoning": "brief explanation"}

Rules:
  - Return ONLY agent names that appear in the list below. Do not invent agent names.
  - Empty list ({"agents": [], "reasoning": "..."}) is a valid response when no agents should run (e.g., whitespace-only changes, doc-only edits with no docs agent).
  - Be precise: include an agent only if the diff actually contains changes that match its TypicalTriggers and aren't ruled out by AvoidsWhen.
  - The reasoning field should briefly explain why you picked these agents (or none).
  - Output the JSON object directly. No markdown fences, no prose before or after.

Available agents:

`)
	for _, s := range specs {
		fmt.Fprintf(&b, "- name: %s\n", s.Name)
		fmt.Fprintf(&b, "  description: %s\n", s.Description)
		if s.TypicalTriggers != "" {
			fmt.Fprintf(&b, "  typical_triggers: %s\n", s.TypicalTriggers)
		}
		if s.DomainFiles != "" {
			fmt.Fprintf(&b, "  domain_files: %s\n", s.DomainFiles)
		}
		if s.AvoidsWhen != "" {
			fmt.Fprintf(&b, "  avoids_when: %s\n", s.AvoidsWhen)
		}
		if s.ExampleScenarios != "" {
			fmt.Fprintf(&b, "  example: %s\n", s.ExampleScenarios)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildRouterUserPrompt wraps the diff (and optional user intent)
// in instructions for the LLM.
func buildRouterUserPrompt(diff, intent string) string {
	if strings.TrimSpace(intent) != "" {
		return fmt.Sprintf(`Here is the diff to route, along with the user's stated intent for this change.

USER'S INTENT:
%s

The intent above is what the user explicitly told us they want from this commit. Use it to disambiguate the diff: if the diff alone could be interpreted multiple ways, the intent tells you which interpretation is correct. The intent does NOT change which agents exist or what they do — it just helps you pick the right ones for what the user actually wants.

Diff:
---
%s
---

Respond with JSON only.`, strings.TrimSpace(intent), diff)
	}
	return fmt.Sprintf(`Here is the diff to route. Decide which agents should handle it.

Diff:
---
%s
---

Respond with JSON only.`, diff)
}

// truncateForError shortens long strings for use in error messages, so
// we don't dump kilobytes of raw LLM output into logs.
func truncateForError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
