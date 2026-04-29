package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrator_Process_MergeWithDirtyWorkingTree verifies the
// integrator can complete a merge even when the project's working
// tree has uncommitted changes — the case that bit us in the first
// real-world v2 run.
func TestIntegrator_Process_MergeWithDirtyWorkingTree(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	// Provision agent worktree, agent makes changes, commits.
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	_ = makeFileChange(t, wt.Path, "agent_added.go", "package main\n\nfunc helper() int { return 1 }\n", "agent change")

	// Now dirty the project's working tree — uncommitted user edits.
	// This is the v1-real-world scenario: user is actively editing.
	mainGo := filepath.Join(proj, "main.go")
	if err := os.WriteFile(mainGo, []byte("package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"user is editing\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
		t.Errorf("Outcome = %s, want merged. Error: %v", result.Outcome, result.MergeError)
	}

	// Verify main has the agent's commit.
	out := gitInDir(t, proj, "log", "--oneline", "main")
	if !strings.Contains(out, "agent change") {
		t.Errorf("main missing agent commit:\n%s", out)
	}

	// Verify the user's uncommitted edits to main.go are still present.
	content, err := os.ReadFile(mainGo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "user is editing") {
		t.Errorf("user's uncommitted edits to main.go were clobbered after merge:\n%s", content)
	}
}
