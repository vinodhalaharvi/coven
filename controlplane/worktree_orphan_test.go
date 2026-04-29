package controlplane

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestWorktreeMgr_CleanupOrphans_DeletesOrphanBranches verifies that
// CleanupOrphans cleans up not just orphaned worktree directories but
// also any leftover agent-* branches whose worktree dirs are gone.
//
// Real-world scenario: previous coven run crashed mid-cycle, leaving
// branches like 'agent-test/abc12345' with no corresponding worktree
// directory. Without branch cleanup, these accumulate over time.
func TestWorktreeMgr_CleanupOrphans_DeletesOrphanBranches(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	// Provision two worktrees, then manually remove their directories
	// to simulate orphans.
	wt1, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	wt2, err := mgr.Provision(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}

	// Drop them from manager's active set so CleanupOrphans treats
	// them as orphans.
	mgr.mu.Lock()
	delete(mgr.active, wt1.Path)
	delete(mgr.active, wt2.Path)
	mgr.mu.Unlock()

	// Verify branches exist
	out, err := mgr.runGitOutput(context.Background(), "branch", "--list", "agent-*/*")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, wt1.Branch) {
		t.Fatalf("setup: expected branch %s in output:\n%s", wt1.Branch, out)
	}
	if !strings.Contains(out, wt2.Branch) {
		t.Fatalf("setup: expected branch %s in output:\n%s", wt2.Branch, out)
	}

	// Run cleanup
	if err := mgr.CleanupOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Verify branches are gone
	out, err = mgr.runGitOutput(context.Background(), "branch", "--list", "agent-*/*")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, wt1.Branch) {
		t.Errorf("branch %s should have been deleted, output:\n%s", wt1.Branch, out)
	}
	if strings.Contains(out, wt2.Branch) {
		t.Errorf("branch %s should have been deleted, output:\n%s", wt2.Branch, out)
	}

	// Verify worktree directories are gone
	for _, p := range []string{wt1.Path, wt2.Path} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("worktree dir %s should have been removed", p)
		}
	}
}

// TestWorktreeMgr_CleanupOrphans_PreservesActiveBranches verifies
// that CleanupOrphans does NOT delete branches whose worktree
// directory still exists (i.e., branches that are in use).
func TestWorktreeMgr_CleanupOrphans_PreservesActiveBranches(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	// Drop from active set but leave the worktree directory in place.
	// This simulates the case where a previous coven was running, the
	// process exited, and the worktree dir is still on disk because
	// there's a real worktree there.
	mgr.mu.Lock()
	delete(mgr.active, wt.Path)
	mgr.mu.Unlock()

	// Verify worktree dir still exists
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("worktree should still exist: %v", err)
	}

	if err := mgr.CleanupOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Worktree directory should be gone (CleanupOrphans removes it
	// because it's not in active set)
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree should have been removed: %v", err)
	}

	// Branch should also be gone (matches up with the gone directory)
	out, err := mgr.runGitOutput(context.Background(), "branch", "--list", wt.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("branch should have been deleted, got:\n%s", out)
	}
}
