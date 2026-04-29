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
		in.cfg.Print(fmt.Sprintf("[integrator] %s/%s: %s\n",
			req.AgentName, shortBranch(req.Branch), result.Outcome))
	}
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
			result.Outcome = OutcomeMergeConflict
			result.MergeError = fmt.Errorf("merge conflict: %s", strings.TrimSpace(mergeOut))
			// Reset the temp worktree for clean shutdown.
			_, _ = in.runGitInWorktree(ctx, tempWt.Path, "merge", "--abort")
		} else {
			result.Outcome = OutcomeError
			result.MergeError = fmt.Errorf("merge failed: %w: %s", mergeErr, strings.TrimSpace(mergeOut))
		}
		return result
	}

	// 3. Run validators against the merged tree.
	changed, _ := in.changedFilesInBranch(ctx, req.Branch)
	results := in.cfg.Validators.RunAll(ctx, tempWt.Path, changed)
	result.ValidatorResults = results
	if AnyFailed(results) {
		result.Outcome = OutcomeValidatorFailed
		return result
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

	// Fast-forward main in the project root to the merge commit.
	// This works because the temp worktree is on a branch that's
	// main + the agent's changes — main is an ancestor of HEAD.
	if _, err := in.runGitInProject(ctx, "merge", "--ff-only", mergeCommit); err != nil {
		result.Outcome = OutcomeError
		result.MergeError = fmt.Errorf("fast-forward main: %w", err)
		return result
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

// Compile-time guard: ensures we don't break the integration with
// the os/filepath package (used implicitly through WorktreeMgr).
var _ = os.PathSeparator
var _ = filepath.Separator
