# Coven v2 Architecture

This document describes the v2 control plane that replaces v1's per-agent
fsmonitor subscriptions with an LLM-routed branch-and-merge model.

## TL;DR

v1 was the prototype that taught us what to build. v2 takes the same
narrow-domain agents and reorchestrates them around three principles:

1. **One LLM call decides which agents handle a change.** No more static
   IsRelevant predicates and settle-window guessing.
2. **Each agent works in its own git worktree.** True isolation, parallel
   execution, no concurrent-write races.
3. **A serial integrator merges with a repair loop.** Conflicts and build
   failures get one or two LLM repair rounds before requeuing to the
   originating agent with full context.

The agents themselves (proto, sqlc, wire, gin, connect, etc.) carry over
from v1 with thin task adapters. Their role strings, tool usage, and
domain knowledge stay the same. What changes is the orchestration around
them.

## Architecture diagram

See [v2-architecture.svg](v2-architecture.svg) for the full sequence
diagram. Open in a browser or SVG viewer for zoomable detail.

## Five phases

### Phase 1 — Change detection & diff construction

- fsnotify watches the project root.
- File events get debounced with a 2-3 second settle window — same idea
  as v1's settle, but a single global window instead of per-agent.
- Once events settle, we run `git add -N` to mark untracked files as
  intent-to-add (so they appear in diff), then `git diff HEAD` to capture
  the full changeset including new files.

The user mostly won't `git commit` directly — most commits will come from
agent integration. This means `git diff HEAD` reflects the user's
in-progress work, not yet-committed agent merges.

### Phase 2 — LLM routing

- Send the diff plus the agent registry (each agent's name, description,
  typical triggers, domain files, exclusion criteria, example scenarios)
  to Claude.
- Claude returns structured JSON: `{agents: [...], reasoning: "..."}`.
- Validate the response: drop hallucinated agent names; an empty list is
  legal and means "no work needed."

The router prompt and registry context format are the highest-risk
component. Spend iteration time here.

### Phase 3 — Parallel fan-out

For each agent in the routing response:

1. Provision a git worktree at `/tmp/coven-worktrees/<agent>-<uuid>/`.
2. Create a branch `agent-<name>/<uuid>` from current main.
3. Build a Task with `{worktree, branch, diff, why, base}`.
4. Invoke the agent with the Task.

Agents run concurrently in their own worktrees — no shared filesystem
state, no concurrent-write races possible.

**Allow-list confirmations.** Inside a worktree, the agent's exec calls
follow this rule:

- **Auto-approved (no y/n prompt):** file writes, codegen tools (buf
  generate, sqlc generate, wire), `go build`, `go test`, `go vet`,
  `go install`, git operations.
- **Requires explicit y/n:** anything else (curl, custom scripts,
  arbitrary shell commands).

This preserves user oversight where it matters (unexpected commands)
without requiring approval for every routine operation.

When an agent commits to its branch and returns successfully, the system
enqueues the branch for integration.

### Phase 4 — Serial integration

A single integrator goroutine drains the queue FIFO, processing one MR
at a time:

1. Merge the agent's branch into a temp worktree (not directly into main
   — keeps main untouched until validation passes).
2. Run validators on changed files: `go build ./...`, `go vet ./...`,
   `go test ./...`. Other validators get added by extension as needed.
3. Decide based on outcome:

   - **All validators pass.** Ask user: "approve merge to main? (y/n)".
     On approval, fast-forward main, clean up worktree.
   - **Validator failed.** LLM repair loop, capped: 1 round for build
     failures, 2 rounds for textual conflicts. If repair succeeds,
     revalidate. If still failing, requeue to originating agent with
     full context (task, prev output, failure, current main).
   - **Textual merge conflict.** LLM resolves via the JSON-formatted
     conflict approach, max 2 rounds.

### Phase 5 — Cascade

When a merge lands on main, fsnotify sees the file changes from the
merge. The cycle repeats from Phase 1 — debounce, diff, route. If the
router determines additional agents are needed (e.g., proto-agent merged
new `.pb.go` files, now connect-agent should run), those agents activate
naturally through the same pipeline.

This is how we replace v1's implicit cascade (each agent's IsRelevant
matching new files) with v2's explicit routing (router decides on each
change).

## What gets reused from v1

These packages move to v2 unchanged or with minor adjustments:

- `agent/` — Claude conversation runtime + standard tools
- `agent/standard tools` — read_file, list_files, search_text, exec, pureast
- `algebra/blackboard/` — fact storage if v2 wants it
- `fsmonitor/` — file change detection
- `llm/` — Sender interface, scripted variant for tests
- `registry/` — agent specs (gets new context fields for router prompt)
- `sources/` — source composition utilities
- All agent packages: `protoagent/`, `sqlcagent/`, `wireagent/`,
  `connectagent/`, `ginagent/`, `dockeragent/`, `mainbuilder/`,
  `makefileagent/`, `gogenerateagent/`, `testagent/`, `buildhealth/`

## What v2 introduces

New packages and files:

- `controlplane/` — top-level orchestrator (this package)
  - `controlplane.go` — `ControlPlane` interface, `Config`, `New`
  - `router.go` — LLM routing call
  - `worktree.go` — worktree provisioning, cleanup
  - `integrator.go` — merge queue, repair loop
  - `validators.go` — extension → validator command registry
  - `allowlist.go` — exec allow-list policy
- `cmd/coven/main.go` — new entry point (replaces `cmd/demo`)
- `docs/` — this directory; architecture doc + diagram

## What gets deleted from v1

Once v2 is fully wired and tested, these get removed in the final commit:

- `cmd/demo/` — replaced by `cmd/coven`
- Per-agent goroutine `Run` loops that subscribe to fsmonitor (gin,
  connect, proto, sqlc, wire, etc.). Each agent's package keeps its
  role string, tools, and inner agent runtime, but loses the
  long-lived watcher.
- `runAgent` retry-on-failure wrapper in cmd/demo
- `agent.ConfirmFunc` per-tool-call gating semantics (replaced by
  allow-list + integration y/n)
- The settle window per agent (replaced by single debouncer in
  controlplane)
- The wake-once vs steady-state distinction in cmd/demo

These deletions don't happen until v2 is functional. v1 stays runnable
on the v1 branch.

## Build order

This is incremental. Each step ships an independently buildable,
testable piece.

1. **Foundation (this commit).** Branch v1 locally, create
   `controlplane` package skeleton with stub `New/Run`, create
   `cmd/coven` no-op entry, add docs. Everything still builds; v1 still
   works.

2. **Router.** Build the LLM routing call against hand-crafted diffs.
   Verify it returns sensible agent lists. Handle empty list,
   hallucinations.

3. **Registry context expansion.** Add TypicalTriggers, DomainFiles,
   AvoidsWhen, ExampleScenarios fields to AgentSpec. Each agent fills
   them in.

4. **Worktree manager.** Provision/cleanup git worktrees with proper
   isolation.

5. **Allow-list policy.** The shim around `exec` that decides
   auto-approve vs y/n.

6. **Task interface for agents.** Thin wrapper that adapts each agent's
   internal runtime to receive a Task instead of a Wake observation.

7. **Integrator.** Merge queue, validators, repair loop, requeue. The
   hardest piece — built last because it depends on everything else.

8. **End-to-end wiring.** Connect controlplane components together,
   point cmd/coven at the real ControlPlane.

9. **Test on coven-demo.** Real cascade.

10. **Delete v1 orchestration.** Once v2 proves out, remove cmd/demo,
    per-agent fsmonitor loops, etc.

## Trade-offs we accept

**Higher per-cascade latency than v1.** v2 adds an LLM routing call
before agents start, plus worktree provisioning overhead. A deep cascade
(proto → connect → build) might take 60-90s where v1 took 30s. In
exchange, agents that can run in parallel actually do, and the routing
is content-aware (whitespace changes don't wake anyone).

**Worktrees use disk and disk pressure.** Each parallel agent needs its
own checkout. For a 100MB Go module with module cache, this is real
disk pressure. Cleanup-after-merge is critical.

**The router can be wrong.** It might miss an agent that should run, or
include one that shouldn't. We mitigate with: (a) good registry context
descriptions; (b) the integrator's validator pass catches "we missed an
agent" cases (the build will fail, surfacing the gap); (c) reasoning
field in the response makes routing decisions debuggable.

**The integrator is the new bottleneck.** Serial by design. If five
agents finish at once, four wait. Acceptable for now; if it hurts,
parallel-merge of non-overlapping branches is a future optimization.

## What this isn't

- Not a centralized planner DAG. The router decides per-change which
  agents run, but doesn't sequence them. Sequencing emerges from the
  integration order (whichever MR arrives first wins for shared files).
- Not contract-first / interface-first. We rejected this earlier — v2
  handles semantic conflicts through the integrator's repair loop, not
  through upfront API design phases.
- Not multi-coordinator. Single ControlPlane per repo. Single integrator
  per ControlPlane. If we ever want multi-repo orchestration, that's a
  layer above this.

## References

- [v2-architecture.svg](v2-architecture.svg) — sequence diagram
- See git log for `feat(controlplane):` and `feat(v2):` commits as
  components land.
