package controlplane

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrator_Process_MergeSyncsWorkingTreeWhenClean verifies the
// real bug we hit repeatedly: after a successful merge to main, the
// working tree was NOT updated, so the user's `ls` and editor saw a
// stale state.
func TestIntegrator_Process_MergeSyncsWorkingTreeWhenClean(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Agent adds a new file in its worktree.
	_ = makeFileChange(t, wt.Path, "new_helper.go", "package main\n\nfunc helper() int { return 42 }\n", "agent: add helper")

	// User's working tree is clean — nothing to preserve.

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	}
	in, err := NewIntegrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	result := in.Process(context.Background(), IntegrationRequest{
		AgentName: "alpha",
		Branch:    wt.Branch,
		Worktree:  wt.Path,
	})

	if result.Outcome != OutcomeMerged {
		t.Fatalf("Outcome = %s, want merged. Error: %v", result.Outcome, result.MergeError)
	}

	// The agent's new file should now exist in the project's working
	// tree — that's the bug we're fixing.
	helperPath := filepath.Join(proj, "new_helper.go")
	if _, err := os.Stat(helperPath); err != nil {
		t.Errorf("agent's new file not synced to working tree: %v", err)
	}
	if data, err := os.ReadFile(helperPath); err == nil {
		if !strings.Contains(string(data), "func helper()") {
			t.Errorf("agent's file synced but content wrong: %s", data)
		}
	}
}

// TestIntegrator_Process_MergeSyncsWorkingTreeWithDeletion verifies
// that file deletions on main also propagate to the working tree.
func TestIntegrator_Process_MergeSyncsWorkingTreeWithDeletion(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	mainTest := filepath.Join(proj, "main_test.go")
	if _, err := os.Stat(mainTest); err != nil {
		t.Fatalf("setup: main_test.go should exist initially: %v", err)
	}

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(wt.Path, "main_test.go")); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "agent: remove tests"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = wt.Path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	}
	in, _ := NewIntegrator(cfg)

	result := in.Process(context.Background(), IntegrationRequest{
		AgentName: "alpha",
		Branch:    wt.Branch,
		Worktree:  wt.Path,
	})

	if result.Outcome != OutcomeMerged {
		t.Fatalf("Outcome = %s, want merged. Error: %v", result.Outcome, result.MergeError)
	}

	// After merge, main_test.go should be GONE from the working tree.
	if _, err := os.Stat(mainTest); !os.IsNotExist(err) {
		t.Errorf("main_test.go should have been removed from working tree after merge")
	}
}
