// WorktreeMgr provisions and cleans up git worktrees for parallel agent
// execution.
//
// In v2, each agent runs in its own git worktree on its own branch.
// This gives true filesystem isolation — concurrent agents can run go
// build, write files, and edit anywhere in their checkout without
// stepping on each other. When the agent finishes, its branch goes to
// the integration queue; once merged (or discarded), its worktree is
// cleaned up.
//
// Worktrees live under <project>/.coven/worktrees/<agent>-<uuid>/, NOT
// /tmp. Reasons:
//   - Survives reboots and /tmp cleanup policies
//   - Visible to the user — `ls .coven/worktrees/` shows what's active,
//     which is useful for debugging stuck cascades
//   - Same filesystem as the repo, avoiding cross-volume copy overhead
//     when git materializes the working directory
//   - Programmatic cleanup is straightforward (we know exactly where to
//     look)
//
// Naming: branches are `agent-<name>/<uuid>`. The slash is intentional
// — git accepts it and it groups branches in tools like `git branch`
// for easy inspection.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Worktree represents one provisioned worktree. A WorktreeMgr.Provision
// call returns one of these; the caller passes it to Cleanup when done.
type Worktree struct {
	// Path is the absolute path to the worktree's working directory.
	// Agents do their work here.
	Path string

	// Branch is the git branch checked out in this worktree, e.g.
	// "agent-proto/a1b2c3d4". Agents commit to this branch.
	Branch string

	// Agent is the name of the agent that owns this worktree (for
	// logging and identification).
	Agent string
}

// WorktreeMgr provisions worktrees rooted in a single project.
type WorktreeMgr struct {
	// projectRoot is the top-level git repo. All worktrees are
	// provisioned as siblings (in .coven/worktrees/) of this root.
	projectRoot string

	// worktreesDir is projectRoot + "/.coven/worktrees/". Cached for
	// convenience.
	worktreesDir string

	mu     sync.Mutex
	active map[string]*Worktree // path -> Worktree, for cleanup tracking
}

// NewWorktreeMgr constructs a manager for the given project root. The
// root must be inside a git repository; we don't verify here, but
// Provision will fail if it isn't.
func NewWorktreeMgr(projectRoot string) *WorktreeMgr {
	abs, _ := filepath.Abs(projectRoot)
	return &WorktreeMgr{
		projectRoot:  abs,
		worktreesDir: filepath.Join(abs, ".coven", "worktrees"),
		active:       make(map[string]*Worktree),
	}
}

// Provision creates a new worktree for the named agent, branched from
// HEAD. Returns the Worktree on success, with all paths populated.
//
// If the worktree directory or branch already exists (e.g. from a
// previous crash with the same UUID — extremely unlikely), Provision
// cleans up the leftover before retrying.
//
// The caller is responsible for calling Cleanup on the returned
// Worktree when done.
func (m *WorktreeMgr) Provision(ctx context.Context, agentName string) (*Worktree, error) {
	if err := m.ensureWorktreesDir(); err != nil {
		return nil, fmt.Errorf("ensure worktrees dir: %w", err)
	}
	if err := m.ensureGitignore(); err != nil {
		// Non-fatal — log the issue but proceed. The .coven/ entry not
		// being in .gitignore won't break anything; it's just messy.
		_ = err
	}

	uuid := newShortUUID()
	branch := fmt.Sprintf("agent-%s/%s", agentName, uuid)
	wtName := fmt.Sprintf("%s-%s", agentName, uuid)
	wtPath := filepath.Join(m.worktreesDir, wtName)

	// Defensive cleanup: in the vanishingly unlikely case the path or
	// branch exists, remove them first.
	if _, err := os.Stat(wtPath); err == nil {
		_ = m.forceRemoveWorktree(ctx, wtPath)
	}

	// Create the worktree.
	if err := m.runGit(ctx, "worktree", "add", "-b", branch, wtPath, "HEAD"); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}

	wt := &Worktree{
		Path:   wtPath,
		Branch: branch,
		Agent:  agentName,
	}

	m.mu.Lock()
	m.active[wtPath] = wt
	m.mu.Unlock()

	return wt, nil
}

// Cleanup removes the worktree directory and deletes its branch. Safe
// to call even if the worktree is already gone (idempotent). Errors
// from individual cleanup steps are logged-and-continued — the goal is
// to remove as much state as possible, not to fail-fast on partial
// cleanup.
func (m *WorktreeMgr) Cleanup(ctx context.Context, wt *Worktree) error {
	if wt == nil {
		return nil
	}

	m.mu.Lock()
	delete(m.active, wt.Path)
	m.mu.Unlock()

	// Remove the worktree (uses --force to handle uncommitted changes).
	// This also removes the working directory.
	if err := m.runGit(ctx, "worktree", "remove", "--force", wt.Path); err != nil {
		// If the worktree is already gone, fall through to branch
		// cleanup. Otherwise, try the manual removal path.
		if _, statErr := os.Stat(wt.Path); statErr == nil {
			// Directory still exists — try to remove it directly.
			_ = os.RemoveAll(wt.Path)
		}
	}

	// Delete the branch. -D forces deletion even if not merged.
	if err := m.runGit(ctx, "branch", "-D", wt.Branch); err != nil {
		// Branch may already be gone (e.g., merged and cleaned up by
		// integrator). Not fatal.
		_ = err
	}

	return nil
}

// CleanupOrphans removes any worktrees in .coven/worktrees/ that aren't
// in the manager's active set. Useful at startup to recover from a
// previous crash that left stale worktrees behind.
//
// This is safe to call when the manager is otherwise idle. Concurrent
// Provision calls are NOT made during CleanupOrphans — caller is
// expected to invoke this before starting agent work.
func (m *WorktreeMgr) CleanupOrphans(ctx context.Context) error {
	if _, err := os.Stat(m.worktreesDir); os.IsNotExist(err) {
		return nil // nothing to clean
	}

	entries, err := os.ReadDir(m.worktreesDir)
	if err != nil {
		return fmt.Errorf("read worktrees dir: %w", err)
	}

	m.mu.Lock()
	activeSnapshot := make(map[string]bool, len(m.active))
	for path := range m.active {
		activeSnapshot[path] = true
	}
	m.mu.Unlock()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(m.worktreesDir, e.Name())
		if activeSnapshot[path] {
			continue // belongs to an active worktree, leave it alone
		}
		if err := m.forceRemoveWorktree(ctx, path); err != nil {
			// Log-and-continue: don't let one stuck orphan prevent
			// cleanup of others.
			_ = err
		}
	}

	// Also prune git's worktree metadata for any paths that no longer
	// exist on disk.
	_ = m.runGit(ctx, "worktree", "prune")

	return nil
}

// Active returns a snapshot of currently-provisioned worktrees.
// Useful for debugging and shutdown cleanup.
func (m *WorktreeMgr) Active() []*Worktree {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Worktree, 0, len(m.active))
	for _, wt := range m.active {
		out = append(out, wt)
	}
	return out
}

// ensureWorktreesDir creates .coven/worktrees/ if it doesn't exist.
func (m *WorktreeMgr) ensureWorktreesDir() error {
	return os.MkdirAll(m.worktreesDir, 0o755)
}

// ensureGitignore appends .coven/ to the project's .gitignore if it
// isn't already there. We don't want users committing worktrees by
// accident.
func (m *WorktreeMgr) ensureGitignore() error {
	path := filepath.Join(m.projectRoot, ".gitignore")
	const entry = ".coven/"

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), entry) {
		return nil // already present
	}

	// Append (or create).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + entry + "\n")
	return err
}

// forceRemoveWorktree tries hard to remove a worktree by any means. Used
// when a leftover orphan needs cleaning up.
func (m *WorktreeMgr) forceRemoveWorktree(ctx context.Context, path string) error {
	// Try git's removal first — it handles refs cleanup correctly.
	_ = m.runGit(ctx, "worktree", "remove", "--force", path)

	// Always follow with a filesystem-level remove in case git missed
	// anything (e.g. the worktree was never registered with git).
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return nil
}

// runGit runs a git subcommand in the project root. Returns the error
// from exec.Command, with stderr included in the error message for
// debuggability.
func (m *WorktreeMgr) runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = m.projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: git %s: %s", err, strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// newShortUUID returns 8 hex characters of randomness. Long enough to
// avoid collisions in practice; short enough to keep paths readable.
func newShortUUID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
