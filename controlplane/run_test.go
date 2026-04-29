package controlplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNew_RealConfigReturnsControlPlane(t *testing.T) {
	// With ProjectRoot set, New should return a real (non-stub)
	// ControlPlane. We can't call Run without a sender, but we can
	// at least verify the constructor returns something sane.
	repo := initTestRepo(t)
	cp := New(Config{
		ProjectRoot: repo,
		Sender:      nil, // intentionally nil — will fail Run
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	})
	if cp == nil {
		t.Fatal("New returned nil")
	}
	// Verify it's not the stub. We check by trying Run with a nil
	// sender and seeing the real-implementation error message.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := cp.Run(ctx)
	if err == nil {
		t.Error("expected error from Run with nil sender, got nil")
		return
	}
	if !strings.Contains(err.Error(), "router not configured") {
		t.Errorf("expected 'router not configured' error, got %v", err)
	}
}

func TestNew_NoProjectRootReturnsStub(t *testing.T) {
	// Without ProjectRoot, New returns the stub (preserves session 1
	// behavior so old tests still work).
	cp := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := cp.Run(ctx)
	if err != nil {
		t.Errorf("stub Run returned unexpected error: %v", err)
	}
}

func TestConfig_WithDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Settle != 2500*time.Millisecond {
		t.Errorf("default Settle = %v, want 2.5s", c.Settle)
	}
	if c.Print == nil {
		t.Error("default Print should not be nil")
	}
	if c.Confirm == nil {
		t.Error("default Confirm should not be nil")
	}

	// Custom values are preserved.
	c2 := Config{
		Settle: 500 * time.Millisecond,
	}.withDefaults()
	if c2.Settle != 500*time.Millisecond {
		t.Errorf("custom Settle overridden: got %v", c2.Settle)
	}
}

func TestBuildDiff_EmptyOnCleanRepo(t *testing.T) {
	repo := initTestRepo(t)
	diff, err := buildDiff(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Errorf("expected empty diff on clean repo, got %d bytes", len(diff))
	}
}

func TestBuildDiff_DetectsNewUntrackedFile(t *testing.T) {
	repo := initTestRepo(t)

	// Add an untracked file
	if err := os.WriteFile(filepath.Join(repo, "new_file.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := buildDiff(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "new_file.go") {
		t.Errorf("diff should reference untracked file, got:\n%s", diff)
	}
}

func TestBuildDiff_DetectsModifiedTrackedFile(t *testing.T) {
	repo := initTestRepo(t)

	// Make a tracked file, commit it, then modify it
	tracked := filepath.Join(repo, "tracked.go")
	if err := os.WriteFile(tracked, []byte("package main\n\nfunc fn() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "add", "tracked.go"},
		{"git", "commit", "-m", "add tracked"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	// Modify
	if err := os.WriteFile(tracked, []byte("package main\n\nfunc fn() { x := 1; _ = x }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := buildDiff(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "tracked.go") {
		t.Errorf("diff should mention tracked.go:\n%s", diff)
	}
	if !strings.Contains(diff, "x := 1") {
		t.Errorf("diff should show added line:\n%s", diff)
	}
}

func TestGetHeadSHA_ReturnsValidSHA(t *testing.T) {
	repo := initTestRepo(t)
	sha, err := getHeadSHA(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	// Should be a 40-char hex string
	if len(sha) != 40 {
		t.Errorf("SHA length = %d, want 40: %q", len(sha), sha)
	}
	for _, c := range sha {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex char in SHA: %q", sha)
			break
		}
	}
}

func TestBranchHasCommitsBeyondBase_NoCommits(t *testing.T) {
	repo := initTestRepo(t)

	// Create a branch but don't add any commits to it
	cmd := exec.Command("git", "branch", "feature/empty")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v: %s", err, out)
	}

	has, err := branchHasCommitsBeyondBase(context.Background(), repo, "feature/empty", "main")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("empty branch should not have commits beyond main")
	}
}

func TestBranchHasCommitsBeyondBase_WithCommits(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Add a commit on the branch
	_ = makeFileChange(t, wt.Path, "x.go", "package main\n", "added x")

	has, err := branchHasCommitsBeyondBase(context.Background(), repo, wt.Branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("branch with commit should have commits beyond main")
	}
}

func TestTruncateLog(t *testing.T) {
	cases := map[string]int{
		"short":                              80,
		"":                                   80,
		"  whitespace stripped  ":            80,
		strings.Repeat("a", 200):             80,
	}
	for input, max := range cases {
		got := truncateLog(input, max)
		if len(got) > max+3 { // +3 for "..."
			t.Errorf("truncateLog(%q, %d) too long: %d chars", input[:min(20, len(input))], max, len(got))
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestDispatchLoop_DebounceCoalescesEvents — fires multiple events
// rapidly, verifies the dispatcher only handles the changeset once
// after the settle window expires.
//
// We can't test the full handleChangeset path without a sender, so
// this test substitutes by checking that the timer mechanics work
// correctly when events come in fast.
func TestDispatchLoop_DebounceCoalescesEvents(t *testing.T) {
	// This test is a little artificial — we don't have an easy way to
	// observe that handleChangeset was called exactly once. The
	// integration tests on a real project will validate this end-to-end
	// in session 8. For now, just exercise the timer reset code path
	// without crashing.
	t.Skip("end-to-end debounce coverage deferred to session 8 (real project test)")
}

func TestStripBinaryDiffs(t *testing.T) {
	cases := map[string]struct {
		input string
		check func(t *testing.T, output string)
	}{
		"no binaries — output unchanged": {
			input: `diff --git a/main.go b/main.go
index abc..def 100644
--- a/main.go
+++ b/main.go
@@ -1 +1 @@
-old
+new
`,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "+new") {
					t.Error("text content was lost")
				}
				if strings.Contains(output, "[binary file change suppressed]") {
					t.Error("non-binary diff was incorrectly marked as suppressed")
				}
			},
		},
		"single binary file": {
			input: `diff --git a/hello b/hello
new file mode 100755
index 0000000..0fdd330
Binary files /dev/null and b/hello differ
`,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "diff --git a/hello b/hello") {
					t.Error("file header was lost")
				}
				if !strings.Contains(output, "[binary file change suppressed]") {
					t.Error("binary file marker not added")
				}
				if strings.Contains(output, "Binary files") {
					t.Error("binary file diff content leaked through")
				}
			},
		},
		"mixed binary and text": {
			input: `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1 @@
-old
+new
diff --git a/binary b/binary
new file mode 100755
index 0000..ffff
Binary files /dev/null and b/binary differ
diff --git a/other.go b/other.go
--- a/other.go
+++ b/other.go
@@ -1 +1 @@
-foo
+bar
`,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "+new") {
					t.Error("text changes to main.go were lost")
				}
				if !strings.Contains(output, "+bar") {
					t.Error("text changes to other.go were lost")
				}
				if !strings.Contains(output, "[binary file change suppressed]") {
					t.Error("binary marker not added")
				}
				if strings.Contains(output, "Binary files") {
					t.Error("binary content leaked through")
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.check(t, stripBinaryDiffs(tc.input))
		})
	}
}

func TestIgnoreInternalPaths(t *testing.T) {
	predicate := ignoreInternalPaths("/home/user/project")

	cases := map[string]bool{
		"/home/user/project/main.go":                                        false,
		"/home/user/project/internal/foo.go":                                false,
		"/home/user/project/.coven/worktrees/test-abc/main.go":              true,
		"/home/user/project/.coven/some-other-state.json":                   true,
		"/home/user/project/.git/HEAD":                                      true,
		"/home/user/project/.git/objects/ab/cdef":                           true,
		"/home/user/project/coven.go":                                       false, // not .coven
		"/home/user/project/git.go":                                         false, // not .git
	}
	for path, expected := range cases {
		t.Run(path, func(t *testing.T) {
			got := predicate(path)
			if got != expected {
				t.Errorf("ignoreInternalPaths(%q) = %v, want %v", path, got, expected)
			}
		})
	}
}
