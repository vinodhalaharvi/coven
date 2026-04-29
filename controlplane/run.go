// run.go is the orchestration layer that ties everything together.
//
// The control plane subscribes to fsnotify events via fsmonitor,
// debounces them into a settle window, takes the resulting changeset
// as a git diff, asks the router which agents should handle it,
// fans the work out into worktrees with task adapters, and lets the
// integrator queue serialize the merges.
//
// In short: events → debounce → diff → route → fan-out → integrate.
//
// What's deliberately NOT here:
//   - Cycle suppression. When the integrator merges to main, fsnotify
//     fires and we route the merge content through the same pipeline.
//     This is INTENTIONAL — that's how cascades work (proto-agent's
//     output wakes connect-agent). The router decides what to do with
//     the merge content; if it correctly excludes the agent that just
//     produced the changes, the cascade terminates naturally.
//   - Cross-changeset state. Each routing decision is independent.
//     We don't remember "we just ran this agent" — if the diff says
//     it should run again, it does.
//
// These are deliberate simplifications. If real cascades create
// pathological loops, we add suppression then. Until then, simpler
// is better.
package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/fsmonitor"
)

// Real ControlPlane implementation that replaces the stub from
// session 1. Kept inside controlplane to avoid exporting the
// orchestration internals.
type controlPlane struct {
	cfg Config

	router      *Router
	wtMgr       *WorktreeMgr
	taskRunner  *TaskRunner
	integrator  *Integrator
	queue       *IntegrationQueue
	allowList   *AllowList

	// fsBoard is the blackboard fsmonitor publishes to. Subscribers
	// read from it via blackboard.Subscribe.
	fsBoard *blackboard.Board[fsmonitor.FileChangeFact]
}

// New constructs a real ControlPlane from a Config. Returns the
// stub if Config doesn't have the minimum required fields (so
// existing callers continue to work).
func New(cfg Config) ControlPlane {
	cfg = cfg.withDefaults()

	// Without a project root, we can't do anything meaningful.
	// Fall back to the stub.
	if cfg.ProjectRoot == "" {
		return &stub{}
	}

	cp := &controlPlane{cfg: cfg}

	cp.allowList = NewAllowList()
	cp.wtMgr = NewWorktreeMgr(cfg.ProjectRoot)
	cp.queue = NewIntegrationQueue(32)
	cp.fsBoard = blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{})

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
		// Bad config — fall back to stub. Logged on first Run call.
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

	// Recover from any leftover worktrees from a prior crash.
	if err := cp.wtMgr.CleanupOrphans(ctx); err != nil {
		cp.cfg.Print(fmt.Sprintf("[coven v2] cleanup orphans: %v\n", err))
	}

	// Start fsmonitor.
	fsCtx, fsCancel := context.WithCancel(ctx)
	defer fsCancel()
	fsErr := make(chan error, 1)
	go func() {
		fsErr <- fsmonitor.Run(fsCtx, fsmonitor.Config{
			Root:       cp.cfg.ProjectRoot,
			Debounce:   200 * time.Millisecond,
			IgnoreFile: ignoreInternalPaths(cp.cfg.ProjectRoot),
			Board:      cp.fsBoard,
			Author:     "controlplane",
		})
	}()

	// Start integrator.
	intErr := make(chan error, 1)
	go func() {
		intErr <- cp.integrator.Run(ctx, cp.queue)
	}()

	// Subscribe to fsmonitor facts and feed the orchestration loop.
	subFn := fsmonitor.Subscribe(cp.fsBoard, fsmonitor.AnyFiles)
	events, err := subFn(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to fsmonitor: %w", err)
	}

	// Run the debouncer + dispatch loop.
	if err := cp.dispatchLoop(ctx, events); err != nil {
		return err
	}

	// Drain background goroutines.
	select {
	case err := <-fsErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("fsmonitor: %w", err)
		}
	case <-time.After(2 * time.Second):
	}
	select {
	case err := <-intErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("integrator: %w", err)
		}
	case <-time.After(2 * time.Second):
	}
	return nil
}

// dispatchLoop is the v2 orchestration heart. Receives debounced
// events and turns them into routing + fan-out actions.
//
// Blocks until ctx is cancelled or events channel closes.
func (cp *controlPlane) dispatchLoop(ctx context.Context, events <-chan fsmonitor.FileChangeFact) error {
	settle := cp.cfg.Settle
	timer := time.NewTimer(time.Hour) // initially "never"
	timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			// Reset the settle timer.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(settle)
		case <-timer.C:
			// Settle window expired — process what's there.
			if err := cp.handleChangeset(ctx); err != nil {
				cp.cfg.Print(fmt.Sprintf("[coven v2] dispatch error: %v\n", err))
			}
		}
	}
}

// handleChangeset is called when the settle timer expires. Builds the
// diff, calls the router, fans out tasks, queues them for integration.
func (cp *controlPlane) handleChangeset(ctx context.Context) error {
	diff, err := buildDiff(ctx, cp.cfg.ProjectRoot)
	if err != nil {
		return fmt.Errorf("build diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		// Nothing meaningful changed (whitespace-only or pure
		// metadata). Skip without an LLM call.
		return nil
	}

	cp.cfg.Print(fmt.Sprintf("[coven v2] changeset detected (%d bytes); routing...\n", len(diff)))

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
			cp.runOneTask(ctx, name, diff, routing.Reasoning)
		}(agentName)
	}
	wg.Wait()
	return nil
}

// runOneTask provisions a worktree, invokes the agent against it,
// and submits the result to the integration queue. Errors are logged
// but don't propagate — one agent's failure shouldn't block others.
func (cp *controlPlane) runOneTask(ctx context.Context, agentName, diff, why string) {
	wt, err := cp.wtMgr.Provision(ctx, agentName)
	if err != nil {
		cp.cfg.Print(fmt.Sprintf("[%s] provision worktree: %v\n", agentName, err))
		return
	}

	// Get the base commit SHA so the agent knows what it's working from.
	baseCommit, _ := getHeadSHA(ctx, cp.cfg.ProjectRoot)

	task := Task{
		AgentName:  agentName,
		Worktree:   wt.Path,
		Branch:     wt.Branch,
		Diff:       diff,
		Why:        why,
		BaseCommit: baseCommit,
	}

	cp.cfg.Print(fmt.Sprintf("[%s] task starting in %s\n", agentName, shortBranch(wt.Branch)))

	final, err := cp.taskRunner.Run(ctx, task)
	if err != nil {
		cp.cfg.Print(fmt.Sprintf("[%s] task error: %v\n", agentName, err))
		// Cleanup worktree on error — no integration to run.
		_ = cp.wtMgr.Cleanup(ctx, wt)
		return
	}

	// Did the agent actually commit anything? If the branch has no
	// commits beyond its base, there's nothing to integrate.
	hasCommits, _ := branchHasCommitsBeyondBase(ctx, cp.cfg.ProjectRoot, wt.Branch, "main")
	if !hasCommits {
		cp.cfg.Print(fmt.Sprintf("[%s] no changes committed; cleaning up\n", agentName))
		_ = cp.wtMgr.Cleanup(ctx, wt)
		return
	}

	// Submit to integrator queue.
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

// buildDiff produces the unified diff representing all uncommitted
// changes (working tree vs HEAD), including untracked files.
//
// Untracked files are made visible by `git add -N` first — this is
// "intent to add" mode that doesn't actually stage content but causes
// `git diff HEAD` to show new files. Side-effect-free from the user's
// perspective.
//
// Binary file diffs are stripped from the output. git represents them
// as 4-line blocks ending in "Binary files ... differ" — these are
// useless to the router (Claude can't reason about binary content)
// and add noise to routing decisions. We keep the file-presence
// information by leaving a one-line marker.
func buildDiff(ctx context.Context, projectRoot string) (string, error) {
	// Stage intent-to-add for untracked files.
	if err := runGitAtRoot(ctx, projectRoot, "add", "-N", "."); err != nil {
		return "", fmt.Errorf("git add -N: %w", err)
	}
	// Get the diff.
	out, err := runGitAtRootCapture(ctx, projectRoot, "diff", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git diff HEAD: %w", err)
	}
	return stripBinaryDiffs(out), nil
}

// stripBinaryDiffs removes the file-diff blocks that represent binary
// content from a unified diff. Replaces each one with a one-line
// summary so the router still knows the file changed.
//
// A binary diff block in git output looks like:
//
//	diff --git a/path b/path
//	new file mode 100755
//	index 0000000..0fdd330
//	Binary files /dev/null and b/path differ
//
// The 4th line is the giveaway. We detect blocks ending in
// "Binary files ... differ" and replace them with:
//
//	diff --git a/path b/path
//	[binary file change suppressed]
//
// preserving the file-list signal but dropping content the router
// can't use.
func stripBinaryDiffs(diff string) string {
	if !strings.Contains(diff, "Binary files") {
		return diff
	}

	var out strings.Builder
	var blockStart int = -1
	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			// Flush pending block if it wasn't binary.
			if blockStart >= 0 {
				for _, prev := range lines[blockStart:i] {
					out.WriteString(prev)
					out.WriteString("\n")
				}
			}
			blockStart = i
			continue
		}
		if blockStart >= 0 && strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ") {
			// This block is binary — write the diff header + marker, skip rest.
			out.WriteString(lines[blockStart])
			out.WriteString("\n")
			out.WriteString("[binary file change suppressed]\n")
			// Find the next diff --git or end of input.
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "diff --git ") {
					blockStart = j
					break
				}
				if j == len(lines)-1 {
					blockStart = -1
				}
			}
			// If we reached end without finding another diff, blockStart=-1.
			// Otherwise blockStart points at the next diff header, which we'll
			// process on the next iteration. Skip ahead.
			continue
		}
	}
	// Flush any remaining non-binary block.
	if blockStart >= 0 {
		for _, line := range lines[blockStart:] {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return out.String()
}

// getHeadSHA returns the current HEAD commit SHA at the project root.
func getHeadSHA(ctx context.Context, projectRoot string) (string, error) {
	out, err := runGitAtRootCapture(ctx, projectRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

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
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
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

// ignoreInternalPaths returns an fsmonitor.IgnoreFile predicate that
// suppresses events for paths coven itself manages.
//
// Without this filter, agents writing files inside their worktrees
// (.coven/worktrees/<agent>-<uuid>/...) would fire fsnotify events
// that propagate to the dispatcher, triggering routing on coven's
// own internal activity. That creates infinite-loop hazards in
// cascades.
//
// We ignore:
//   - everything under <project>/.coven/ (worktrees and any future
//     internal state)
//   - .git/ (git's internal files; not normally written during
//     normal operation but defensive)
//
// The predicate receives absolute paths. The check is a string-prefix
// match against the absolute project root + "/.coven" (or "/.git").
func ignoreInternalPaths(projectRoot string) func(string) bool {
	covenInternal := projectRoot + string(os.PathSeparator) + ".coven"
	gitInternal := projectRoot + string(os.PathSeparator) + ".git"
	return func(absPath string) bool {
		if strings.HasPrefix(absPath, covenInternal) {
			return true
		}
		if strings.HasPrefix(absPath, gitInternal) {
			return true
		}
		return false
	}
}

// withDefaults populates Config zero values with sensible defaults.
func (c Config) withDefaults() Config {
	if c.Settle <= 0 {
		c.Settle = 2500 * time.Millisecond
	}
	if c.Print == nil {
		c.Print = func(string) {}
	}
	if c.Confirm == nil {
		c.Confirm = func(context.Context, string, string) bool { return false }
	}
	return c
}
