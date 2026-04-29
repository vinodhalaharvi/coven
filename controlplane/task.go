// Task is the unit of work the v2 control plane dispatches to an agent.
//
// Where v1 had agents subscribe to fsmonitor and decide for themselves
// whether to wake (via per-agent IsRelevant predicates), v2 routes diffs
// to agents centrally and hands each one a Task describing what to do
// and where to do it.
//
// The Task is everything an agent needs:
//   - Worktree: the absolute path of the agent's isolated checkout
//   - Branch: the branch name the agent should commit to
//   - Diff: the diff that triggered routing (so the agent has context
//     for what changed)
//   - Why: the router's reasoning for picking this agent (helps the
//     agent focus on what's relevant)
//   - BaseCommit: the SHA the worktree branched from
//   - AgentName: which registered agent this task targets
//
// The agent code itself (role string, Claude conversation runtime,
// standard tools) is unchanged from v1. The adapter wraps the inner
// agent runtime, swaps the moduleRoot for the worktree path, installs
// the allow-list as a PreConfirm hook, and translates the Task into
// an observation string the agent's conversational loop understands.
package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/vinodhalaharvi/coven/agent"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

// Task carries everything an agent needs to do one unit of work.
type Task struct {
	// AgentName is the registered name of the agent to invoke (e.g.
	// "proto", "connect", "test"). Must match an entry in
	// registry.All().
	AgentName string

	// Worktree is the absolute path to the agent's isolated git
	// checkout. The agent's StandardTools are rooted here, so reads
	// and writes go through this directory.
	Worktree string

	// Branch is the git branch the agent should commit to. Agent
	// commits are expected to land on this branch; the integrator
	// later merges it into main.
	Branch string

	// Diff is the diff that triggered routing for this task. The
	// agent uses this as context — "this is what changed; do what
	// your role says to do about it."
	Diff string

	// Why is the router's reasoning for picking this agent. Helps the
	// agent focus on the specific aspect of the diff that matters to
	// it (vs. blindly re-deriving the whole change from scratch).
	Why string

	// BaseCommit is the SHA the worktree branched from. Useful for
	// the agent to construct git commands like "git diff HEAD" that
	// see only the agent's own changes.
	BaseCommit string
}

// TaskRunner runs an agent against a task, returning the agent's final
// text output (the agent's own conclusion / summary) and an error if
// the conversation failed unrecoverably.
//
// On a successful run, the agent has either committed to the task's
// branch or determined no work was needed. The TaskRunner does not
// itself commit — that's the agent's job, via the standard exec tool
// invoking git.
type TaskRunner struct {
	sender    llm.Sender
	allowList *AllowList
	confirm   agent.ConfirmFunc
	print     agent.PrintFunc
}

// NewTaskRunner constructs a TaskRunner.
//
// sender   — Claude API client (used for the agent's conversation)
// allow    — policy gate; auto-approves standard operations
// confirm  — fallback for non-standard exec calls; nil means deny
// print    — user-facing output; nil means fmt.Print
func NewTaskRunner(sender llm.Sender, allow *AllowList, confirm agent.ConfirmFunc, print agent.PrintFunc) *TaskRunner {
	return &TaskRunner{
		sender:    sender,
		allowList: allow,
		confirm:   confirm,
		print:     print,
	}
}

// Run invokes the named agent against the given task. The agent runs
// in a fresh conversational session — there's no long-lived state
// from previous tasks. Each task is its own complete invocation.
//
// Returns the agent's final text output (or empty string if the agent
// produced none) and an error if the conversation failed.
func (tr *TaskRunner) Run(ctx context.Context, task Task) (string, error) {
	if task.AgentName == "" {
		return "", fmt.Errorf("task missing AgentName")
	}
	if task.Worktree == "" {
		return "", fmt.Errorf("task missing Worktree path")
	}

	spec := registry.ByName(task.AgentName)
	if spec == nil {
		return "", fmt.Errorf("no registered agent named %q", task.AgentName)
	}
	if spec.Role == "" {
		return "", fmt.Errorf("agent %q has no Role string registered", task.AgentName)
	}

	// Build a fresh inner agent rooted at the task's worktree.
	// Tools are rooted at task.Worktree so file reads/writes and exec
	// commands all happen inside the worktree, not the project root.
	inner := agent.New(agent.Config{
		ID:         task.AgentName + "-task",
		Role:       spec.Role,
		Tools:      agent.StandardTools(task.Worktree),
		Sender:     tr.sender,
		Confirm:    tr.confirm,
		PreConfirm: tr.preConfirmFromAllowList(),
		Print:      tr.print,
		MaxTurns:   60,
	})

	// Translate the Task into an observation the agent understands.
	// The observation primes Claude with: what changed, why this agent
	// was picked, and where to commit. The agent's role string already
	// tells Claude HOW to do its job.
	observation := buildTaskObservation(task)

	final, err := inner.Wake(ctx, observation)
	if err != nil {
		return "", fmt.Errorf("agent %s on task: %w", task.AgentName, err)
	}
	return final, nil
}

// preConfirmFromAllowList returns a PreConfirm hook that consults the
// allow-list. Allowed commands run silently; everything else falls
// through to the user's ConfirmFunc.
//
// Returns nil if no allow-list was provided — in that case the agent
// behaves like v1 (every mutating tool prompts the user).
func (tr *TaskRunner) preConfirmFromAllowList() agent.PreConfirmFunc {
	if tr.allowList == nil {
		return nil
	}
	return func(ctx context.Context, use *llm.ToolUseBlock) agent.PreConfirmDecision {
		// Only the exec tool gets policy treatment. File-write tools
		// (write_file, mkdir) are mutating but they're project-scoped
		// inside a worktree by construction, so they're always
		// auto-approved when an allow-list is in use.
		switch use.Name {
		case "exec":
			cmd, _ := use.Input["command"].(string)
			switch tr.allowList.Decide(cmd) {
			case DecisionAllow:
				return agent.PreConfirmAllow
			default:
				return agent.PreConfirmAsk
			}
		case "write_file", "create_file", "mkdir":
			return agent.PreConfirmAllow
		default:
			// Unknown tool — be safe, ask.
			return agent.PreConfirmAsk
		}
	}
}

// buildTaskObservation produces the wake observation string passed to
// the agent's conversational runtime. It tells Claude:
//   - what triggered this work (the diff)
//   - why this agent was picked (router's reasoning)
//   - where to commit (the branch)
//   - that this is a v2 task-driven invocation
//
// The role string tells Claude HOW to do the work; this observation
// tells Claude WHAT the immediate situation is.
func buildTaskObservation(task Task) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You have been invoked as %s-agent against a v2 task.\n\n", task.AgentName)

	if task.Why != "" {
		fmt.Fprintf(&b, "Routing reason: %s\n\n", task.Why)
	}

	fmt.Fprintf(&b, "You are working in an isolated git worktree on branch %q.\n", task.Branch)
	fmt.Fprintf(&b, "Your worktree path is %q.\n", task.Worktree)
	if task.BaseCommit != "" {
		fmt.Fprintf(&b, "The branch was created from commit %s.\n", task.BaseCommit)
	}
	b.WriteString("\nWhen you finish your work, commit your changes to this branch using git. The integrator will merge your branch back into main once validators pass. If you decide no work is needed, you may exit without committing.\n\n")

	if task.Diff != "" {
		b.WriteString("The diff that triggered routing to you:\n---\n")
		b.WriteString(task.Diff)
		b.WriteString("\n---\n")
	}

	return b.String()
}
