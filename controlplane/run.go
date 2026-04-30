// run.go is the orchestration layer that ties everything together.
//
// MODEL: commit-based triggering.
//
// coven polls the project's main branch SHA. When it advances, coven
// processes the new commit(s): builds a diff against the previously-
// processed SHA, asks the router which agents should react, fans out
// agent tasks in worktrees, and lets the integrator queue serialize
// the merges back to main.
//
// One commit = one round of agent work. After each round, lastProcessed
// advances to wherever main is now (post-merge), and coven goes back
// to polling. There is NO automatic cascade: if proto-agent's output
// should wake connect-agent, the user must commit something to trigger
// the next round. This is a deliberate design choice — auto-cascade
// previously caused noisy verify passes and made stop conditions
// circular. Now the user drives the cycle with commits.
//
// State persistence:
//
//   .coven/state.json holds {LastProcessedSHA}. On startup, coven
//   reads this; if there are unprocessed commits since, they all get
//   collapsed into one routing decision (we diff lastProcessed..main).
//   This survives restarts cleanly.
//
// Why polling and not git hooks:
//
//   Git hooks require installation in .git/hooks/, which means the
//   user has to set them up. They also don't fire for commits made
//   from outside the repo (e.g., 'git commit -a' from another tool,
//   GUI clients, etc.). Polling is simpler, universal, and the cost
//   is trivial — once-per-second 'git rev-parse' is essentially free.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// controlPlane is the real ControlPlane implementation.
type controlPlane struct {
	cfg Config

	router     *Router
	wtMgr      *WorktreeMgr
	taskRunner *TaskRunner
	integrator *Integrator
	queue      *IntegrationQueue
	allowList  *AllowList

	// stateFile is the persistence path: <ProjectRoot>/.coven/state.json
	stateFile string
}

// state is the persisted state of the polling loop.
type state struct {
	// LastProcessedSHA is the most recent main SHA coven has fully
	// processed (routed and merged any agent work for). On the next
	// poll, if main has advanced, the diff sent to the router is
	// LastProcessedSHA..currentMain.
	LastProcessedSHA string `json:"last_processed_sha"`
}

// New constructs a real ControlPlane from a Config. Returns the stub
// if Config doesn't have the minimum required fields (preserves
// session-1 behavior for callers that pass empty configs).
func New(cfg Config) ControlPlane {
	cfg = cfg.withDefaults()

	if cfg.ProjectRoot == "" {
		return &stub{}
	}

	cp := &controlPlane{
		cfg:       cfg,
		stateFile: filepath.Join(cfg.ProjectRoot, ".coven", "state.json"),
	}

	cp.allowList = NewAllowList()
	cp.wtMgr = NewWorktreeMgr(cfg.ProjectRoot)
	cp.queue = NewIntegrationQueue(32)

	if cfg.Sender != nil {
		cp.router = NewRouter(cfg.Sender)
		cp.taskRunner = NewTaskRunner(cfg.Sender, cp.allowList, cfg.Confirm, cfg.Print)
	}

	validators := cfg.Validators
	if validators == nil {
		validators = NewValidatorRegistry()
	}

	intCfg := IntegratorConfig{
		ProjectRoot: cfg.ProjectRoot,
		WorktreeMgr: cp.wtMgr,
		Validators:  validators,
		Confirm:     cfg.Confirm,
		Print:       cfg.Print,
	}
	if cfg.Sender != nil && cfg.EnableRepair {
		intCfg.Repair = &RepairConfig{Sender: cfg.Sender}
	}
	in, err := NewIntegrator(intCfg)
	if err != nil {
		cp.integrator = nil
	} else {
		cp.integrator = in
	}

	return cp
}

// Run is the main loop. Blocks until ctx is cancelled.
func (cp *controlPlane) Run(ctx context.Context) error {
	if cp.integrator == nil {
		return errors.New("controlplane: integrator not configured (check Config)")
	}
	if cp.router == nil {
		return errors.New("controlplane: router not configured (Sender required)")
	}

	cp.cfg.Print(fmt.Sprintf("[coven v2] watching %s\n", cp.cfg.ProjectRoot))

	// Recover from any leftover worktrees and orphan branches from a
	// prior crashed run.
	if err := cp.wtMgr.CleanupOrphans(ctx); err != nil {
		cp.cfg.Print(fmt.Sprintf("[coven v2] cleanup orphans: %v\n", err))
	}

	// Start integrator goroutine.
	intErr := make(chan error, 1)
	go func() {
		intErr <- cp.integrator.Run(ctx, cp.queue)
	}()

	// Load persisted state (or initialize).
	st, err := cp.loadState()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// If state has no LastProcessedSHA, initialize it to current main.
	// This means the first run on a fresh project will NOT process any
	// pre-existing history — coven only acts on commits made AFTER it
	// starts watching. If the user wants a specific older SHA processed,
	// they can manually edit state.json or make a commit.
	if st.LastProcessedSHA == "" {
		currentSHA, err := cp.headSHA(ctx)
		if err != nil {
			return fmt.Errorf("read current main SHA: %w", err)
		}
		st.LastProcessedSHA = currentSHA
		if err := cp.saveState(st); err != nil {
			cp.cfg.Print(fmt.Sprintf("[coven v2] save state: %v\n", err))
		}
		cp.cfg.Print(fmt.Sprintf("[coven v2] starting from main at %s\n", shortSHAStr(currentSHA)))
	} else {
		cp.cfg.Print(fmt.Sprintf("[coven v2] resuming from last processed %s\n", shortSHAStr(st.LastProcessedSHA)))
	}

	// Run polling loop.
	if err := cp.pollLoop(ctx, st); err != nil {
		// Drain integrator before returning.
		select {
		case <-intErr:
		case <-time.After(2 * time.Second):
		}
		return err
	}

	// ctx cancelled — drain integrator.
	select {
	case err := <-intErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("integrator: %w", err)
		}
	case <-time.After(2 * time.Second):
	}
	return nil
}

// pollLoop watches main's SHA. When it advances, processes the new
// commits.
//
// Loop invariant: at the top of each iteration, st.LastProcessedSHA
// reflects what we've fully handled. After a successful round, we
// advance it to the new main SHA (post-merge if the agent work
// landed).
func (cp *controlPlane) pollLoop(ctx context.Context, st *state) error {
	ticker := time.NewTicker(cp.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		currentSHA, err := cp.headSHA(ctx)
		if err != nil {
			cp.cfg.Print(fmt.Sprintf("[coven v2] poll error: %v\n", err))
			continue
		}
		if currentSHA == st.LastProcessedSHA {
			continue // no new commits
		}

		// Check whether the new commits are all coven's own (from a
		// previous merge). If yes, advance state and skip — no need
		// to route on our own work.
		allTagged, err := allCommitsAreCovenTagged(ctx, cp.cfg.ProjectRoot, st.LastProcessedSHA, currentSHA)
		if err == nil && allTagged {
			cp.cfg.Print(fmt.Sprintf("[coven v2] skipping coven-tagged commits: %s → %s\n",
				shortSHAStr(st.LastProcessedSHA), shortSHAStr(currentSHA)))
			st.LastProcessedSHA = currentSHA
			if err := cp.saveState(st); err != nil {
				cp.cfg.Print(fmt.Sprintf("[coven v2] save state: %v\n", err))
			}
			continue
		}
		// If err != nil, fall through to processing — being safe is
		// better than silently skipping due to a transient git error.

		// New user commits detected. Process them as one batch.
		cp.cfg.Print(fmt.Sprintf("[coven v2] new commit on main: %s → %s\n",
			shortSHAStr(st.LastProcessedSHA), shortSHAStr(currentSHA)))

		if err := cp.processChangeset(ctx, st.LastProcessedSHA, currentSHA); err != nil {
			cp.cfg.Print(fmt.Sprintf("[coven v2] process error: %v\n", err))
			// Even on error, advance lastProcessed so we don't keep retrying
			// the same broken commit. Better to skip than to loop forever.
		}

		// Advance lastProcessed to whatever main is NOW (might include
		// agent-merged commits made during processChangeset).
		newSHA, err := cp.headSHA(ctx)
		if err == nil {
			st.LastProcessedSHA = newSHA
		} else {
			st.LastProcessedSHA = currentSHA
		}
		if err := cp.saveState(st); err != nil {
			cp.cfg.Print(fmt.Sprintf("[coven v2] save state: %v\n", err))
		}
	}
}

// processChangeset handles one changeset: routes it to agents, fans out
// tasks, waits for them all to complete (whether merged, failed, or
// skipped). Returns when all agent tasks are done.
//
// Note: this is synchronous from the polling loop's perspective. Agent
// tasks run in parallel internally (each in a goroutine), but the
// integrator processes their integration requests serially via the
// queue.
func (cp *controlPlane) processChangeset(ctx context.Context, fromSHA, toSHA string) error {
	diff, err := cp.gitDiff(ctx, fromSHA, toSHA)
	if err != nil {
		return fmt.Errorf("git diff %s..%s: %w", fromSHA[:8], toSHA[:8], err)
	}
	if strings.TrimSpace(diff) == "" {
		// Could happen if the only commits in this range are empty
		// (--allow-empty) or if SHAs reference the same tree. Skip.
		return nil
	}

	cp.cfg.Print(fmt.Sprintf("[coven v2] routing changeset (%d bytes)...\n", len(diff)))

	routing, err := cp.router.Route(ctx, diff)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	if len(routing.Agents) == 0 {
		cp.cfg.Print(fmt.Sprintf("[coven v2] router: no agents needed (%s)\n", routing.Reasoning))
		return nil
	}

	cp.cfg.Print(fmt.Sprintf("[coven v2] router picked: %v — %s\n",
		routing.Agents, routing.Reasoning))

	// Fan out: provision worktree per agent, run task in goroutine,
	// submit to integrator queue when done.
	var wg sync.WaitGroup
	for _, agentName := range routing.Agents {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			cp.runOneTask(ctx, name, diff, routing.Reasoning, toSHA)
		}(agentName)
	}
	wg.Wait()
	return nil
}

// runOneTask provisions a worktree (branched from toSHA, the just-
// processed main commit), invokes the agent, and submits the result
// to the integration queue. Errors are logged but don't propagate —
// one agent's failure shouldn't block others.
func (cp *controlPlane) runOneTask(ctx context.Context, agentName, diff, why, baseSHA string) {
	wt, err := cp.wtMgr.Provision(ctx, agentName)
	if err != nil {
		cp.cfg.Print(fmt.Sprintf("[%s] provision worktree: %v\n", agentName, err))
		return
	}

	task := Task{
		AgentName:  agentName,
		Worktree:   wt.Path,
		Branch:     wt.Branch,
		Diff:       diff,
		Why:        why,
		BaseCommit: baseSHA,
	}

	cp.cfg.Print(fmt.Sprintf("[%s] task starting in %s\n", agentName, shortBranch(wt.Branch)))

	final, err := cp.taskRunner.Run(ctx, task)
	if err != nil {
		cp.cfg.Print(fmt.Sprintf("[%s] task error: %v\n", agentName, err))
		_ = cp.wtMgr.Cleanup(ctx, wt)
		return
	}

	hasCommits, _ := branchHasCommitsBeyondBase(ctx, cp.cfg.ProjectRoot, wt.Branch, "main")
	if !hasCommits {
		cp.cfg.Print(fmt.Sprintf("[%s] no changes committed; cleaning up\n", agentName))
		_ = cp.wtMgr.Cleanup(ctx, wt)
		return
	}

	// Tag the agent's commits on this branch so the polling loop can
	// distinguish them from user commits when they land on main.
	// Without tagging, the integrator's merge to main would look like
	// a fresh user commit and trigger another routing round.
	if err := tagAgentCommits(ctx, wt.Path, "main"); err != nil {
		cp.cfg.Print(fmt.Sprintf("[%s] warning: failed to tag commits: %v\n", agentName, err))
		// Continue anyway — worst case we get a cascade pass that
		// terminates harmlessly.
	}

	cp.cfg.Print(fmt.Sprintf("[%s] submitting to integrator (%s)\n", agentName, truncateLog(final, 80)))
	if err := cp.queue.Submit(ctx, IntegrationRequest{
		AgentName:    agentName,
		Branch:       wt.Branch,
		Worktree:     wt.Path,
		OriginalDiff: diff,
		Why:          why,
	}); err != nil {
		cp.cfg.Print(fmt.Sprintf("[%s] queue submit: %v\n", agentName, err))
		_ = cp.wtMgr.Cleanup(ctx, wt)
	}
}

// gitDiff returns the diff between two commits.
func (cp *controlPlane) gitDiff(ctx context.Context, fromSHA, toSHA string) (string, error) {
	out, err := runGitAtRootCapture(ctx, cp.cfg.ProjectRoot, "diff", fromSHA, toSHA)
	if err != nil {
		return "", err
	}
	return out, nil
}

// headSHA returns the current SHA of refs/heads/main.
func (cp *controlPlane) headSHA(ctx context.Context) (string, error) {
	out, err := runGitAtRootCapture(ctx, cp.cfg.ProjectRoot, "rev-parse", "main")
	if err != nil {
		return "", fmt.Errorf("rev-parse main: %w: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// loadState reads .coven/state.json. Returns a fresh state if the file
// doesn't exist or is unreadable.
func (cp *controlPlane) loadState() (*state, error) {
	data, err := os.ReadFile(cp.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &state{}, nil
		}
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		// Treat malformed state file as if it didn't exist; warn and
		// continue rather than crashing on a corrupted JSON file.
		cp.cfg.Print(fmt.Sprintf("[coven v2] state file corrupt, reinitializing: %v\n", err))
		return &state{}, nil
	}
	return &st, nil
}

// saveState writes .coven/state.json atomically.
func (cp *controlPlane) saveState(st *state) error {
	if err := os.MkdirAll(filepath.Dir(cp.stateFile), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := cp.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cp.stateFile)
}

// covenCommitPrefix marks commits that came from coven's agents (not
// from the user). The polling loop uses this to skip its own merges
// and avoid triggering cascade passes on agent-produced state.
//
// Format: every line of the commit message gets nothing special, but
// the SUBJECT (first line) starts with this prefix. Multi-line bodies
// are unaffected.
const covenCommitPrefix = "[coven] "

// tagAgentCommits rewrites every commit on `worktree`'s current branch
// that's beyond the merge-base with `base` (i.e., every commit the
// agent added) so that its subject line starts with covenCommitPrefix.
//
// We do this BEFORE submitting to the integrator because:
//   1. The integrator merges via update-ref which inherits the agent's
//      commit verbatim onto main. There's no separate merge commit to
//      tag.
//   2. Tagging at the source (the agent's branch, before any merge)
//      means the tag is applied exactly once.
//
// The implementation uses an interactive rebase with --exec to amend
// each commit's message. For a simple agent that produced one commit
// (the common case) this is just one rewrite. We use git filter-branch
// would also work but is deprecated and slow. Interactive rebase is
// the modern equivalent.
//
// If a commit subject is already prefixed (idempotency), we leave it
// alone.
func tagAgentCommits(ctx context.Context, worktree, base string) error {
	// Get the list of commits the agent added.
	out, err := runGitCmdOutput(ctx, worktree, "rev-list", "--reverse", base+"..HEAD")
	if err != nil {
		return fmt.Errorf("rev-list: %w", err)
	}
	shas := []string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			shas = append(shas, line)
		}
	}
	if len(shas) == 0 {
		return nil // nothing to tag
	}

	// Common case: one commit. Just amend it.
	if len(shas) == 1 {
		return amendCommitWithCovenPrefix(ctx, worktree)
	}

	// Multi-commit case: reset working tree to base, then cherry-pick
	// each original commit with an amended message.
	//
	// We use --hard reset (not --mixed) so the working tree matches
	// base cleanly. Without --hard, reset --mixed leaves the agent's
	// uncommitted file additions as untracked, and cherry-pick refuses
	// to overwrite untracked files. The --hard reset throws away those
	// untracked-but-actually-just-added files; cherry-pick re-applies
	// them with the original commit's tree state.
	if err := runGitInWorktreeNoOutput(ctx, worktree, "reset", "--hard", base); err != nil {
		return fmt.Errorf("reset --hard to %s: %w", base, err)
	}
	for _, sha := range shas {
		if err := runGitInWorktreeNoOutput(ctx, worktree, "cherry-pick", sha); err != nil {
			return fmt.Errorf("cherry-pick %s: %w", sha[:8], err)
		}
		if err := amendCommitWithCovenPrefix(ctx, worktree); err != nil {
			return fmt.Errorf("amend after cherry-pick %s: %w", sha[:8], err)
		}
	}
	return nil
}

// amendCommitWithCovenPrefix amends HEAD's commit message to prepend
// covenCommitPrefix (idempotent — won't re-prefix if already there).
func amendCommitWithCovenPrefix(ctx context.Context, worktree string) error {
	// Get current message.
	out, err := runGitCmdOutput(ctx, worktree, "log", "-1", "--format=%B")
	if err != nil {
		return fmt.Errorf("read commit message: %w", err)
	}
	msg := strings.TrimRight(out, "\n")
	if strings.HasPrefix(msg, covenCommitPrefix) {
		return nil // already tagged
	}
	newMsg := covenCommitPrefix + msg
	if err := runGitInWorktreeNoOutput(ctx, worktree, "commit", "--amend", "-m", newMsg); err != nil {
		return fmt.Errorf("amend: %w", err)
	}
	return nil
}

// runGitInWorktreeNoOutput runs git in `worktree`, discarding output,
// returning a wrapped error on failure.
func runGitInWorktreeNoOutput(ctx context.Context, worktree string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// allCommitsAreCovenTagged returns true if every commit in the range
// fromSHA..toSHA has a subject line starting with covenCommitPrefix.
// Used by pollLoop to detect "this is coven's own merge, don't route."
//
// If the range is empty or git rev-list fails, returns false (safer to
// route than to skip).
func allCommitsAreCovenTagged(ctx context.Context, projectRoot, fromSHA, toSHA string) (bool, error) {
	out, err := runGitAtRootCapture(ctx, projectRoot, "log", fromSHA+".."+toSHA, "--format=%s")
	if err != nil {
		return false, err
	}
	subjects := strings.Split(strings.TrimSpace(out), "\n")
	if len(subjects) == 0 || (len(subjects) == 1 && subjects[0] == "") {
		return false, nil
	}
	for _, s := range subjects {
		if !strings.HasPrefix(s, covenCommitPrefix) {
			return false, nil
		}
	}
	return true, nil
}


// that aren't on `base`. Used to decide whether to enqueue an
// integration request: if the agent didn't actually commit anything,
// there's nothing to merge.
// branchHasCommitsBeyondBase reports whether `branch` has any commits
// that aren't on `base`. Used to decide whether to enqueue an
// integration request: if the agent didn't actually commit anything,
// there's nothing to merge.
func branchHasCommitsBeyondBase(ctx context.Context, projectRoot, branch, base string) (bool, error) {
	out, err := runGitAtRootCapture(ctx, projectRoot, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return false, err
	}
	count := strings.TrimSpace(out)
	return count != "0" && count != "", nil
}

// runGitAtRoot runs git in the project root, discarding output.
func runGitAtRoot(ctx context.Context, root string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runGitAtRootCapture runs git in the project root and returns its
// combined output.
func runGitAtRootCapture(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// truncateLog clips a long string for log lines.
func truncateLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// shortSHAStr returns the first 8 chars of a SHA for log readability.
// Named -Str to avoid conflict with integrator's shortSHA.
func shortSHAStr(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// withDefaults populates Config zero values with sensible defaults.
func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 1 * time.Second
	}
	if c.Print == nil {
		c.Print = func(string) {}
	}
	if c.Confirm == nil {
		c.Confirm = func(context.Context, string, string) bool { return false }
	}
	return c
}
