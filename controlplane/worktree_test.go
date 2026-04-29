package controlplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// initTestRepo creates a minimal git repo in a temp dir with one commit
// on main. Returns the absolute path. t.TempDir handles cleanup.
//
// We need a real git repo because worktree operations talk to git.
// Mocking git would mean re-implementing it, which defeats the purpose
// of the test.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}

	commands := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test"},
		{"git", "config", "user.name", "test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, c := range commands {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup failed: %s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	return dir
}

func TestWorktreeMgr_ProvisionCreatesWorktreeAndBranch(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// Worktree path should exist as a directory
	info, err := os.Stat(wt.Path)
	if err != nil {
		t.Fatalf("worktree path not found: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("worktree path is not a directory")
	}

	// Path should be under .coven/worktrees/
	expectedPrefix := filepath.Join(repo, ".coven", "worktrees")
	if !strings.HasPrefix(wt.Path, expectedPrefix) {
		t.Errorf("worktree at unexpected path: %s (expected under %s)", wt.Path, expectedPrefix)
	}

	// Branch name format: agent-<name>/<uuid>
	if !strings.HasPrefix(wt.Branch, "agent-alpha/") {
		t.Errorf("branch name format wrong: %q", wt.Branch)
	}

	// Agent field populated
	if wt.Agent != "alpha" {
		t.Errorf("agent name = %q, want alpha", wt.Agent)
	}

	// Worktree should be visible to git
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if !strings.Contains(string(out), wt.Path) {
		t.Errorf("worktree not in git's list:\n%s", out)
	}
}

func TestWorktreeMgr_ProvisionAddsToActive(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "beta")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	active := mgr.Active()
	if len(active) != 1 {
		t.Errorf("Active() len = %d, want 1", len(active))
	}
	if active[0].Path != wt.Path {
		t.Errorf("Active()[0] != provisioned worktree")
	}
}

func TestWorktreeMgr_CleanupRemovesWorktreeAndBranch(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if err := mgr.Cleanup(context.Background(), wt); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	// Path should no longer exist
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree path still exists after cleanup")
	}

	// Active set should be empty
	if len(mgr.Active()) != 0 {
		t.Errorf("Active() = %v after cleanup, want empty", mgr.Active())
	}

	// Branch should be gone
	cmd := exec.Command("git", "branch", "--list", wt.Branch)
	cmd.Dir = repo
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), wt.Branch) {
		t.Errorf("branch %q still exists after cleanup:\n%s", wt.Branch, out)
	}
}

func TestWorktreeMgr_CleanupNilIsSafe(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	if err := mgr.Cleanup(context.Background(), nil); err != nil {
		t.Errorf("Cleanup(nil) returned error: %v", err)
	}
}

func TestWorktreeMgr_CleanupTwiceIsIdempotent(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if err := mgr.Cleanup(context.Background(), wt); err != nil {
		t.Fatalf("first Cleanup failed: %v", err)
	}
	// Second cleanup should not error.
	if err := mgr.Cleanup(context.Background(), wt); err != nil {
		t.Errorf("second Cleanup returned error: %v", err)
	}
}

func TestWorktreeMgr_AppendsToGitignore(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	_, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	gi, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if !strings.Contains(string(gi), ".coven/") {
		t.Errorf(".gitignore missing .coven/ entry:\n%s", gi)
	}
}

func TestWorktreeMgr_GitignoreNotDuplicated(t *testing.T) {
	repo := initTestRepo(t)
	// Pre-create .gitignore with the entry already present
	preExisting := "# my project\n.coven/\nbin/\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewWorktreeMgr(repo)
	_, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	count := strings.Count(string(gi), ".coven/")
	if count != 1 {
		t.Errorf("expected .coven/ to appear once, got %d times:\n%s", count, gi)
	}
}

func TestWorktreeMgr_TwoConcurrentProvisionsGetDifferentPaths(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	var wg sync.WaitGroup
	results := make([]*Worktree, 5)
	errs := make([]error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			wt, err := mgr.Provision(context.Background(), "alpha")
			results[idx] = wt
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for i, wt := range results {
		if errs[i] != nil {
			t.Errorf("provision %d failed: %v", i, errs[i])
			continue
		}
		if seen[wt.Path] {
			t.Errorf("duplicate path: %s", wt.Path)
		}
		seen[wt.Path] = true
	}

	// Cleanup all
	for _, wt := range results {
		if wt != nil {
			_ = mgr.Cleanup(context.Background(), wt)
		}
	}
}

func TestWorktreeMgr_CleanupOrphansRemovesStaleDirs(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	// Create one real worktree
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Create a stale orphan directory directly (simulating a crash)
	orphanPath := filepath.Join(mgr.worktreesDir, "stale-deadbeef")
	if err := os.MkdirAll(orphanPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// Put a file in it
	if err := os.WriteFile(filepath.Join(orphanPath, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run cleanup
	if err := mgr.CleanupOrphans(context.Background()); err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}

	// Real worktree should still exist (it's in active set)
	if _, err := os.Stat(wt.Path); err != nil {
		t.Errorf("active worktree was removed: %v", err)
	}

	// Orphan should be gone
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphan still exists after CleanupOrphans")
	}
}

func TestWorktreeMgr_CleanupOrphansOnEmptyDirIsSafe(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	// .coven/worktrees doesn't exist yet
	if err := mgr.CleanupOrphans(context.Background()); err != nil {
		t.Errorf("CleanupOrphans on empty: %v", err)
	}
}

func TestWorktreeMgr_NewShortUUIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		u := newShortUUID()
		if len(u) != 8 {
			t.Fatalf("UUID length = %d, want 8", len(u))
		}
		if seen[u] {
			t.Fatalf("duplicate UUID after %d iterations: %s", i, u)
		}
		seen[u] = true
	}
}

func TestWorktreeMgr_WorktreeAcceptsCommits(t *testing.T) {
	// End-to-end-ish: provision a worktree, add a file, commit. Verify
	// the commit lands on the worktree's branch and not on main.
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Write a file in the worktree, commit
	testFile := filepath.Join(wt.Path, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range [][]string{
		{"git", "config", "user.email", "test@test"},
		{"git", "config", "user.name", "test"},
		{"git", "add", "hello.txt"},
		{"git", "commit", "-m", "test commit"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = wt.Path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}

	// In the worktree, the branch HEAD should now have hello.txt
	cmd := exec.Command("git", "log", "--oneline")
	cmd.Dir = wt.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log in worktree: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "test commit") {
		t.Errorf("commit not in worktree branch:\n%s", out)
	}

	// Main branch should NOT have hello.txt — verify with git show-ref
	cmd = exec.Command("git", "log", "--oneline", "main")
	cmd.Dir = repo
	out, _ = cmd.CombinedOutput()
	if strings.Contains(string(out), "test commit") {
		t.Errorf("test commit leaked to main:\n%s", out)
	}
}
