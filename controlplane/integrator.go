// Integrator is the serial gatekeeper between agent branches and main.
//
// In v2, agents work in parallel in their own worktrees, committing to
// their own branches. The integrator pulls branches off a FIFO queue,
// one at a time, and decides whether each one is fit to merge. That
// decision involves:
//
//   1. A clean merge (no textual conflicts)
//   2. All applicable validators passing
//   3. User approval (y/n)
//
// Anything else aborts the merge and (in 6a) requeues the branch for
// the originating agent to handle. Session 6b adds LLM-mediated
// repair as an intermediate step before requeue.
//
// Why serial?
//
// We considered parallelizing non-overlapping merges (e.g., proto-agent
// and docs-agent touching disjoint files), but the complexity isn't
// justified: most cascades are short (3-5 branches), each merge is
// fast (build+test in seconds for most projects), and the serial
// guarantee makes failure modes much easier to reason about. If queue
// depth becomes a real problem we can revisit.
//
// Why a temp worktree for the merge attempt?
//
// We never modify main directly until we know the merge is good. The
// integrator provisions a separate temp worktree at HEAD, merges the
// candidate branch into it, runs validators there, and only fast-
// forwards main if everything passes. If anything fails, the temp
// worktree is discarded — main was never touched.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
)

// IntegrationRequest is what an agent submits when it finishes a task.
// Carries enough context that the integrator can merge the branch and
// (on failure in session 6b) requeue with a useful payload.
type IntegrationRequest struct {
	// AgentName is the agent that produced this branch. Used to
	// identify the requeue target on failure.
	AgentName string

	// Branch is the branch to merge into main. Created by the
	// WorktreeMgr at task provisioning time.
	Branch string

	// Worktree is the agent's worktree path. The integrator cleans
	// this up after a successful merge (or after a final give-up).
	Worktree string

	// OriginalDiff is the diff that triggered routing the agent to
	// this task. Useful in 6b for the requeue payload.
	OriginalDiff string

	// Why is the router's reasoning for picking this agent. Same.
	Why string
}

// IntegrationResult describes the outcome of one merge attempt.
type IntegrationResult struct {
	// Request is the request this result is for. Useful for logging.
	Request IntegrationRequest

	// Outcome describes what happened. See the constants below.
	Outcome IntegrationOutcome

	// MergeError is non-nil if the textual merge itself failed
	// (conflicts).
	MergeError error

	// ValidatorResults is the results of running validators on the
	// merged tree. Populated on OutcomeValidatorFailed.
	ValidatorResults []ValidatorResult

	// MergeCommit is the SHA of the resulting merge commit on main,
	// populated on OutcomeMerged.
	MergeCommit string

	// RepairAttempts captures any LLM repair rounds that ran during
	// this integration. Empty for clean merges; populated when
	// repair was invoked (whether successful or not). Useful for
	// observability and the requeue payload.
	RepairAttempts []RepairResult
}

// IntegrationOutcome enumerates the possible end-states of a merge
// attempt.
type IntegrationOutcome int

const (
	// OutcomeMerged: clean merge, validators passed, user approved,
	// fast-forwarded onto main.
	OutcomeMerged IntegrationOutcome = iota

	// OutcomeMergeConflict: textual merge produced conflicts. Session
	// 6a treats this as a requeue trigger; 6b will attempt LLM
	// resolution first.
	OutcomeMergeConflict

	// OutcomeValidatorFailed: clean merge, but go build / go vet /
	// go test failed on the merged tree. Session 6a treats this as
	// requeue; 6b adds LLM repair.
	OutcomeValidatorFailed

	// OutcomeUserDeclined: clean merge + validators passed, but the
	// user declined the y/n prompt. Worktree is discarded.
	OutcomeUserDeclined

	// OutcomeError: an unexpected error occurred (git failures, IO
	// problems, ctx cancellation). The branch is left in place for
	// later inspection; manual cleanup may be needed.
	OutcomeError
)

// String renders the outcome as a short label for logs.
func (o IntegrationOutcome) String() string {
	switch o {
	case OutcomeMerged:
		return "merged"
	case OutcomeMergeConflict:
		return "merge-conflict"
	case OutcomeValidatorFailed:
		return "validator-failed"
	case OutcomeUserDeclined:
		return "user-declined"
	case OutcomeError:
		return "error"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// IntegrationQueue is a FIFO of pending integration requests. Backed
// by a buffered channel so the queue both blocks consumers when empty
// and drops nothing when bursts arrive.
type IntegrationQueue struct {
	ch chan IntegrationRequest
}

// NewIntegrationQueue constructs a queue with the given buffer size.
// A buffer of 32 is plenty for typical agent fan-out (a routing
// decision rarely picks more than a handful of agents).
func NewIntegrationQueue(buffer int) *IntegrationQueue {
	if buffer <= 0 {
		buffer = 32
	}
	return &IntegrationQueue{
		ch: make(chan IntegrationRequest, buffer),
	}
}

// Submit enqueues a request. Blocks if the queue is at capacity (rare).
// Returns the context's error if it's cancelled while we're waiting.
func (q *IntegrationQueue) Submit(ctx context.Context, req IntegrationRequest) error {
	select {
	case q.ch <- req:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Next dequeues the oldest request, blocking until one is available.
// Returns ctx.Err() if the context is cancelled before a request arrives.
func (q *IntegrationQueue) Next(ctx context.Context) (IntegrationRequest, error) {
	select {
	case req := <-q.ch:
		return req, nil
	case <-ctx.Done():
		return IntegrationRequest{}, ctx.Err()
	}
}

// Len returns the number of pending requests. Useful for tests and
// for debugging stuck cascades.
func (q *IntegrationQueue) Len() int {
	return len(q.ch)
}

// IntegratorConfig configures an Integrator.
type IntegratorConfig struct {
	// ProjectRoot is the absolute path to the main git repo. Merges
	// land here; temp worktrees are siblings of this path's worktree
	// directory.
	ProjectRoot string

	// WorktreeMgr is used to provision the temp worktree the merge
	// happens in. Reusing the same manager keeps cleanup logic in
	// one place.
	WorktreeMgr *WorktreeMgr

	// Validators is the registry of post-merge checks. Required.
	Validators *ValidatorRegistry

	// Confirm is the y/n prompt for the final "merge to main?"
	// decision. Required. Returns true to merge.
	Confirm agent.ConfirmFunc

	// Print is user-facing output. nil means fmt.Print.
	Print agent.PrintFunc

	// Repair (optional) enables LLM-mediated repair of merge conflicts
	// and validator failures. nil = no repair (6a behavior). When
	// set, the integrator will attempt repair before returning a
	// failure outcome.
	Repair *RepairConfig
}

// Integrator drains an IntegrationQueue serially, attempting merges
// and reporting results.
type Integrator struct {
	cfg IntegratorConfig
	mu  sync.Mutex // guards merge operations
}

// NewIntegrator constructs an Integrator. Returns an error if the
// config is missing required fields.
func NewIntegrator(cfg IntegratorConfig) (*Integrator, error) {
	if cfg.ProjectRoot == "" {
		return nil, errors.New("IntegratorConfig.ProjectRoot is required")
	}
	if cfg.WorktreeMgr == nil {
		return nil, errors.New("IntegratorConfig.WorktreeMgr is required")
	}
	if cfg.Validators == nil {
		return nil, errors.New("IntegratorConfig.Validators is required")
	}
	if cfg.Confirm == nil {
		return nil, errors.New("IntegratorConfig.Confirm is required")
	}
	if cfg.Print == nil {
		cfg.Print = func(string) {}
	}
	return &Integrator{cfg: cfg}, nil
}

// Run drains the queue until ctx is cancelled. Each request is
// processed serially via Process. Errors from individual requests
// are logged but don't terminate Run — the loop continues.
func (in *Integrator) Run(ctx context.Context, q *IntegrationQueue) error {
	for {
		req, err := q.Next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		result := in.Process(ctx, req)
		in.logOutcome(req, result)
	}
}

// logOutcome emits a verbose log line describing what happened to a
// merge request. Called by Run after every Process. Verbose because
// the integrator is the trickiest part of v2 — when something goes
// wrong, we want enough information in the log to diagnose it
// without running again with extra logging enabled.
func (in *Integrator) logOutcome(req IntegrationRequest, result IntegrationResult) {
	short := shortBranch(req.Branch)
	switch result.Outcome {
	case OutcomeMerged:
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: MERGED to main as %s\n",
			req.AgentName, short, shortSHA(result.MergeCommit)))
		// Check if the user's working tree diverges from the new main.
		// We use update-ref to advance main without touching the working
		// tree (so we don't clobber uncommitted edits) — but that
		// trade-off can leave the working tree visibly diverged from
		// main, which is confusing if the user doesn't expect it.
		// Print an advisory in that case.
		if dirty, summary := in.workingTreeDivergence(); dirty {
			in.cfg.Print("  ⚠ Your working tree differs from the new main:\n")
			in.cfg.Print("    " + strings.ReplaceAll(strings.TrimSpace(summary), "\n", "\n    ") + "\n")
			in.cfg.Print("    To sync your working tree to main: git checkout -- .\n")
			in.cfg.Print("    To preserve your edits and rebase: git stash && git pull --rebase\n")
		}
	case OutcomeMergeConflict:
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: MERGE-CONFLICT — %v\n",
			req.AgentName, short, result.MergeError))
		if len(result.RepairAttempts) > 0 {
			in.cfg.Print(fmt.Sprintf("  (repair attempts: %d)\n", len(result.RepairAttempts)))
			for i, ra := range result.RepairAttempts {
				if ra.Err != nil {
					in.cfg.Print(fmt.Sprintf("  attempt %d: %v\n", i+1, ra.Err))
				}
			}
		}
	case OutcomeValidatorFailed:
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: VALIDATOR-FAILED\n",
			req.AgentName, short))
		for _, vr := range result.ValidatorResults {
			if !vr.Passed() {
				in.cfg.Print(fmt.Sprintf("  [%s] failed: %v\n", vr.Validator.Name, vr.Err))
				if vr.Output != "" {
					trimmed := strings.TrimSpace(vr.Output)
					if len(trimmed) > 500 {
						trimmed = trimmed[:500] + "...(truncated)"
					}
					in.cfg.Print(fmt.Sprintf("    %s\n", strings.ReplaceAll(trimmed, "\n", "\n    ")))
				}
			}
		}
	case OutcomeUserDeclined:
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: USER-DECLINED\n",
			req.AgentName, short))
	case OutcomeError:
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: ERROR — %v\n",
			req.AgentName, short, result.MergeError))
	default:
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: %s\n",
			req.AgentName, short, result.Outcome))
	}
}

// shortSHA returns the first 8 chars of a git SHA for log readability.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// Process handles one integration request end-to-end. Public so tests
// can invoke it directly without setting up a queue + Run loop.
//
// Process is safe to call from a single goroutine. The integrator's
// design assumes serial processing — calling Process concurrently
// would race on main's HEAD.
func (in *Integrator) Process(ctx context.Context, req IntegrationRequest) IntegrationResult {
	in.mu.Lock()
	defer in.mu.Unlock()

	result := IntegrationResult{Request: req}

	// 1. Provision temp worktree at current main.
	tempWt, err := in.cfg.WorktreeMgr.Provision(ctx, "integrator-temp")
	if err != nil {
		result.Outcome = OutcomeError
		result.MergeError = fmt.Errorf("provision temp worktree: %w", err)
		return result
	}
	defer in.cfg.WorktreeMgr.Cleanup(ctx, tempWt)

	// 2. Attempt merge of req.Branch into the temp worktree.
	mergeOut, mergeErr := in.runGitInWorktree(ctx, tempWt.Path, "merge", "--no-edit", req.Branch)
	if mergeErr != nil {
		// Could be a conflict, or an outright failure. Distinguish via
		// "git status" — if there are unmerged paths, it's a conflict;
		// otherwise it's a different kind of error.
		statusOut, _ := in.runGitInWorktree(ctx, tempWt.Path, "status", "--porcelain")
		if hasUnmergedPaths(statusOut) {
			// Try LLM repair if configured.
			if in.cfg.Repair != nil {
				repaired, repairErr := in.tryConflictRepair(ctx, tempWt.Path, &result)
				if repairErr == nil && repaired {
					// Conflict resolved + committed. Fall through to
					// the validators step below — same as a clean
					// merge would.
				} else {
					result.Outcome = OutcomeMergeConflict
					result.MergeError = fmt.Errorf("merge conflict (repair %s): %s",
						conflictRepairStatus(repairErr, repaired),
						strings.TrimSpace(mergeOut))
					_, _ = in.runGitInWorktree(ctx, tempWt.Path, "merge", "--abort")
					return result
				}
			} else {
				result.Outcome = OutcomeMergeConflict
				result.MergeError = fmt.Errorf("merge conflict: %s", strings.TrimSpace(mergeOut))
				_, _ = in.runGitInWorktree(ctx, tempWt.Path, "merge", "--abort")
				return result
			}
		} else {
			result.Outcome = OutcomeError
			result.MergeError = fmt.Errorf("merge failed: %w: %s", mergeErr, strings.TrimSpace(mergeOut))
			return result
		}
	}

	// 3. Run validators against the merged tree.
	changed, _ := in.changedFilesInBranch(ctx, req.Branch)
	results := in.cfg.Validators.RunAll(ctx, tempWt.Path, changed)
	result.ValidatorResults = results
	if AnyFailed(results) {
		// Try LLM repair if configured.
		if in.cfg.Repair != nil {
			repaired := in.tryValidatorRepair(ctx, tempWt.Path, results, changed, &result)
			if !repaired {
				result.Outcome = OutcomeValidatorFailed
				return result
			}
			// Repair succeeded; results were updated in-place.
			result.ValidatorResults = results // capture the post-repair results too
		} else {
			result.Outcome = OutcomeValidatorFailed
			return result
		}
	}

	// 4. Ask user for approval.
	summary := fmt.Sprintf("merge agent %q branch %q to main (validators passed)", req.AgentName, shortBranch(req.Branch))
	if !in.cfg.Confirm(ctx, "integrate", summary) {
		result.Outcome = OutcomeUserDeclined
		return result
	}

	// 5. Fast-forward main to the merge commit. We do this by merging
	// in the project root (not the temp worktree, which is on a
	// detached HEAD-ish branch). The merge is fast-forward because
	// the temp worktree's branch is just main + the agent's branch.
	mergeCommitOut, err := in.runGitInWorktree(ctx, tempWt.Path, "rev-parse", "HEAD")
	if err != nil {
		result.Outcome = OutcomeError
		result.MergeError = fmt.Errorf("rev-parse HEAD: %w", err)
		return result
	}
	mergeCommit := strings.TrimSpace(mergeCommitOut)
	result.MergeCommit = mergeCommit

	// Move main's branch pointer to the merge commit, then sync the
	// working tree to the new main while preserving any user edits.
	//
	// IMPORTANT order of operations:
	//   1. Stash any user uncommitted changes RELATIVE TO OLD MAIN.
	//      This must happen BEFORE update-ref. Otherwise the working
	//      tree's "deletion" of files that are in the new main but
	//      not yet in the working tree gets captured by the stash —
	//      and then re-applied during pop, undoing the merge in the
	//      working tree.
	//   2. update-ref to advance main's pointer.
	//   3. git reset --hard HEAD: aligns working tree + index to the
	//      new HEAD. Working tree files are recreated from HEAD's
	//      tree, files no longer in HEAD's tree are removed.
	//   4. Pop the stash if we created one. May produce conflicts if
	//      user's edits collide with the merge — surface those for
	//      manual resolution.
	stashed, err := in.stashUserEditsIfDirty(ctx)
	if err != nil {
		result.Outcome = OutcomeError
		result.MergeError = fmt.Errorf("pre-merge stash: %w", err)
		return result
	}

	if _, err := in.runGitInProject(ctx, "update-ref", "refs/heads/main", mergeCommit); err != nil {
		// Try to restore stash on error.
		if stashed {
			_, _ = in.runGitInProject(ctx, "stash", "pop")
		}
		result.Outcome = OutcomeError
		result.MergeError = fmt.Errorf("update-ref refs/heads/main: %w", err)
		return result
	}

	if _, err := in.runGitInProject(ctx, "reset", "--hard", "HEAD"); err != nil {
		// Try to pop stash before giving up on sync; main is still
		// updated correctly so this is a non-fatal warning.
		if stashed {
			_, _ = in.runGitInProject(ctx, "stash", "pop")
		}
		in.cfg.Print(fmt.Sprintf("  ⚠ working tree sync failed: %v\n", err))
		in.cfg.Print("    main has the new content. Run 'git status' to see your tree state.\n")
	} else if stashed {
		popOut, err := in.runGitInProject(ctx, "stash", "pop")
		if err != nil {
			in.cfg.Print("  ⚠ Your uncommitted edits conflict with the new main and need manual resolution.\n")
			in.cfg.Print("    Run 'git status' to see conflicts, fix them, then 'git stash drop' when done.\n")
			in.cfg.Print(fmt.Sprintf("    git stash pop output: %s\n", strings.TrimSpace(popOut)))
		}
	}

	// 6. Cleanup the agent's worktree on success.
	if req.Worktree != "" {
		_ = in.cfg.WorktreeMgr.Cleanup(ctx, &Worktree{
			Path:   req.Worktree,
			Branch: req.Branch,
			Agent:  req.AgentName,
		})
	}

	result.Outcome = OutcomeMerged
	return result
}

// runGitInWorktree runs a git subcommand inside the given worktree
// directory. Returns combined output + error.
func (in *Integrator) runGitInWorktree(ctx context.Context, worktree string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGitInProject runs a git subcommand at the project root.
func (in *Integrator) runGitInProject(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = in.cfg.ProjectRoot
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stashUserEditsIfDirty stashes the user's uncommitted edits if the
// working tree has any. Returns whether a stash was created.
//
// Called BEFORE update-ref so the stash captures the user's actual
// changes (relative to current main), not the divergence that
// update-ref would introduce.
//
// Returns false (and nil error) if the working tree is clean.
func (in *Integrator) stashUserEditsIfDirty(ctx context.Context) (bool, error) {
	statusOut, err := in.runGitInProject(ctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(statusOut) == "" {
		return false, nil
	}

	stashOut, err := in.runGitInProject(ctx, "stash", "push", "-u", "-m", "coven: pre-merge stash")
	if err != nil {
		return false, fmt.Errorf("git stash: %w: %s", err, strings.TrimSpace(stashOut))
	}
	// "No local changes to save" means stash didn't actually create
	// a stash entry (e.g., everything was ignored).
	stashed := !strings.Contains(stashOut, "No local changes to save")
	return stashed, nil
}

// workingTreeDivergence returns true and a short summary if the
// project's working tree differs from HEAD (i.e., the user has
// uncommitted modifications, staged changes, or untracked files
// the integrator's update-ref step didn't sync).
//
// We use 'git status --porcelain' which is stable across git versions
// and returns one line per changed file. The summary truncates to
// the first ~5 lines for log readability.
func (in *Integrator) workingTreeDivergence() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := in.runGitInProject(ctx, "status", "--porcelain")
	if err != nil {
		return false, ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return false, ""
	}

	const maxLines = 5
	if len(lines) > maxLines {
		summary := strings.Join(lines[:maxLines], "\n")
		summary += fmt.Sprintf("\n... (%d more)", len(lines)-maxLines)
		return true, summary
	}
	return true, strings.Join(lines, "\n")
}

// changedFilesInBranch lists files that differ between the branch and
// main. Used to decide which extension-scoped validators apply.
//
// Returns paths relative to the worktree root.
func (in *Integrator) changedFilesInBranch(ctx context.Context, branch string) ([]string, error) {
	out, err := in.runGitInProject(ctx, "diff", "--name-only", "main", branch)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// hasUnmergedPaths checks `git status --porcelain` output for the
// "UU" / "AA" / "DD" markers that indicate merge conflicts.
//
// Porcelain format: each line starts with two status chars; double
// chars (UU = both modified, AA = both added, DD = both deleted)
// indicate conflict.
func hasUnmergedPaths(porcelain string) bool {
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 2 {
			continue
		}
		// Conflict markers in porcelain output
		switch line[:2] {
		case "UU", "AA", "DD", "AU", "UA", "DU", "UD":
			return true
		}
	}
	return false
}

// shortBranch trims the agent branch to the UUID suffix for log
// readability: "agent-proto/a1b2c3d4" → "a1b2c3d4".
func shortBranch(branch string) string {
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		return branch[i+1:]
	}
	return branch
}

// tryConflictRepair attempts up to MaxConflictRounds rounds of LLM-
// mediated conflict resolution. Returns (repaired, err) where
// repaired==true means the merge was successfully completed; err!=nil
// means an unrecoverable error.
//
// Mutates result.RepairAttempts to record what was tried.
func (in *Integrator) tryConflictRepair(ctx context.Context, worktree string, result *IntegrationResult) (bool, error) {
	rc := in.cfg.Repair.withDefaults()
	totalRoundsUsed := 0

	for round := 0; round < rc.MaxConflictRounds; round++ {
		if totalRoundsUsed >= rc.MaxRoundsPerMR {
			return false, nil
		}
		conflicted, err := listConflictedFiles(ctx, worktree)
		if err != nil {
			return false, err
		}
		if len(conflicted) == 0 {
			// Already clean — nothing to do.
			return true, nil
		}

		repairResult, _ := resolveConflicts(ctx, rc.Sender, worktree, conflicted)
		result.RepairAttempts = append(result.RepairAttempts, repairResult)
		totalRoundsUsed += repairResult.RoundsUsed

		if repairResult.Succeeded {
			// Verify no markers remain anywhere — defense against the
			// repair "succeeding" while leaving partial conflicts.
			stillConflicted, _ := listConflictedFiles(ctx, worktree)
			if len(stillConflicted) == 0 {
				in.cfg.Print(fmt.Sprintf("  [integrator] conflict resolved in %d round(s)\n", round+1))
				return true, nil
			}
			// More conflicts remain — try again.
		}
		// Loop back for another attempt unless budget exhausted.
	}
	return false, nil
}

// tryValidatorRepair attempts up to MaxValidatorRounds rounds of LLM-
// mediated repair for validator failures. Returns true if validators
// pass after repair.
//
// On success, mutates results in place to reflect the post-repair
// validator pass.
func (in *Integrator) tryValidatorRepair(ctx context.Context, worktree string, results []ValidatorResult, changed []string, intResult *IntegrationResult) bool {
	rc := in.cfg.Repair.withDefaults()

	// Track total budget across both repair types.
	totalRoundsUsed := 0
	for _, r := range intResult.RepairAttempts {
		totalRoundsUsed += r.RoundsUsed
	}

	currentResults := results
	for round := 0; round < rc.MaxValidatorRounds; round++ {
		if totalRoundsUsed >= rc.MaxRoundsPerMR {
			return false
		}

		repairResult, _ := repairValidatorFailure(ctx, rc.Sender, worktree, currentResults)
		intResult.RepairAttempts = append(intResult.RepairAttempts, repairResult)
		totalRoundsUsed += repairResult.RoundsUsed

		if !repairResult.Succeeded {
			return false
		}

		// Re-run validators on the repaired tree.
		newResults := in.cfg.Validators.RunAll(ctx, worktree, changed)
		if !AnyFailed(newResults) {
			// Mutate the original results slice in place — caller will
			// see clean validators.
			for i := range results {
				if i < len(newResults) {
					results[i] = newResults[i]
				}
			}
			in.cfg.Print(fmt.Sprintf("  [integrator] validator failure repaired in %d round(s)\n", round+1))
			return true
		}
		currentResults = newResults
	}
	return false
}

// conflictRepairStatus produces a short label for the merge-error
// message, summarizing why repair didn't succeed.
func conflictRepairStatus(err error, repaired bool) string {
	if repaired {
		return "succeeded"
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return "exhausted budget"
}

// Compile-time guard: ensures we don't break the integration with
// the os/filepath package (used implicitly through WorktreeMgr).
var _ = os.PathSeparator
var _ = filepath.Separator
