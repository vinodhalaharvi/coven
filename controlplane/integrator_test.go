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
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/agent"
)

func TestIntegrationQueue_SubmitAndNext(t *testing.T) {
	q := NewIntegrationQueue(4)

	req := IntegrationRequest{AgentName: "alpha", Branch: "agent-alpha/abc"}
	if err := q.Submit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 1 {
		t.Errorf("Len = %d, want 1", q.Len())
	}

	got, err := q.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentName != "alpha" {
		t.Errorf("got AgentName %q, want alpha", got.AgentName)
	}
	if q.Len() != 0 {
		t.Errorf("Len after Next = %d, want 0", q.Len())
	}
}

func TestIntegrationQueue_FIFO(t *testing.T) {
	q := NewIntegrationQueue(8)

	for _, name := range []string{"a", "b", "c"} {
		_ = q.Submit(context.Background(), IntegrationRequest{AgentName: name})
	}

	for _, want := range []string{"a", "b", "c"} {
		got, _ := q.Next(context.Background())
		if got.AgentName != want {
			t.Errorf("FIFO violation: got %q, want %q", got.AgentName, want)
		}
	}
}

func TestIntegrationQueue_NextBlocksUntilContextCancel(t *testing.T) {
	q := NewIntegrationQueue(4)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := q.Next(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected deadline error, got %v", err)
	}
}

func TestNewIntegrator_RejectsIncompleteConfig(t *testing.T) {
	cases := map[string]IntegratorConfig{
		"missing project root": {WorktreeMgr: NewWorktreeMgr("/tmp"), Validators: NewValidatorRegistry(), Confirm: alwaysConfirm},
		"missing worktree mgr": {ProjectRoot: "/tmp", Validators: NewValidatorRegistry(), Confirm: alwaysConfirm},
		"missing validators":   {ProjectRoot: "/tmp", WorktreeMgr: NewWorktreeMgr("/tmp"), Confirm: alwaysConfirm},
		"missing confirm":      {ProjectRoot: "/tmp", WorktreeMgr: NewWorktreeMgr("/tmp"), Validators: NewValidatorRegistry()},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewIntegrator(cfg)
			if err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

// Test helpers used by integrator tests.
func alwaysConfirm(context.Context, string, string) bool { return true }
func neverConfirm(context.Context, string, string) bool  { return false }

// makeFileChange writes a file in the worktree, commits it on the
// current branch, returns the resulting commit SHA.
func makeFileChange(t *testing.T, worktree, filename, content, commitMsg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktree, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "config", "user.email", "test@test"},
		{"git", "config", "user.name", "test"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", commitMsg},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = worktree
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup: %s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// gitInDir runs `git args...` in dir and returns combined output.
func gitInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func TestIntegrator_Process_CleanMergeAndApprove(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	// Provision a worktree, add a non-conflicting file there, commit.
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	_ = makeFileChange(t, wt.Path, "added_by_agent.go", "package main\n\nfunc helper() int { return 42 }\n", "agent change")

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
		t.Errorf("Outcome = %s, want merged", result.Outcome)
		t.Logf("ValidatorResults: %+v", result.ValidatorResults)
		t.Logf("MergeError: %v", result.MergeError)
	}
	if result.MergeCommit == "" {
		t.Error("expected MergeCommit to be populated")
	}

	// Verify main now has the agent's file.
	out := gitInDir(t, proj, "log", "--oneline", "main")
	if !strings.Contains(out, "agent change") {
		t.Errorf("main does not have agent commit:\n%s", out)
	}

	// Verify the worktree was cleaned up.
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Error("agent worktree was not cleaned up after successful merge")
	}
}

func TestIntegrator_Process_UserDecline(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)
	_ = makeFileChange(t, wt.Path, "added.go", "package main\n", "agent change")

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     neverConfirm, // user says no
		Print:       func(string) {},
	}
	in, _ := NewIntegrator(cfg)

	result := in.Process(context.Background(), IntegrationRequest{
		AgentName: "alpha",
		Branch:    wt.Branch,
		Worktree:  wt.Path,
	})

	if result.Outcome != OutcomeUserDeclined {
		t.Errorf("Outcome = %s, want user-declined", result.Outcome)
	}

	// Main should NOT have the agent's commit.
	out := gitInDir(t, proj, "log", "--oneline", "main")
	if strings.Contains(out, "agent change") {
		t.Errorf("user declined but main still has agent commit:\n%s", out)
	}

	// Agent worktree should still exist (we don't clean up on decline
	// — that's the user's choice; they may want to inspect or rerun).
	if _, err := os.Stat(wt.Path); err != nil {
		t.Errorf("worktree was unexpectedly cleaned up after user decline: %v", err)
	}
}

func TestIntegrator_Process_ValidatorFailure(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Inject a build break (file that doesn't compile)
	_ = makeFileChange(t, wt.Path, "broken.go",
		"package main\n\nfunc broken() { return undefinedSymbol }\n",
		"break the build")

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

	if result.Outcome != OutcomeValidatorFailed {
		t.Errorf("Outcome = %s, want validator-failed", result.Outcome)
	}
	if !AnyFailed(result.ValidatorResults) {
		t.Error("expected at least one failed validator result")
	}

	// Main should NOT have the broken commit.
	out := gitInDir(t, proj, "log", "--oneline", "main")
	if strings.Contains(out, "break the build") {
		t.Errorf("main has broken commit despite validator failure:\n%s", out)
	}
}

func TestIntegrator_Process_MergeConflict(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	// Modify main directly to set up a conflict.
	_ = makeFileChange(t, proj, "main.go",
		"package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"main version\")\n}\n",
		"main edit")

	// Then provision the agent worktree (branches off the new main, but
	// we'll edit main again to create divergence).
	wt, err := mgr.Provision(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Edit the SAME file on main again (after worktree created, so this
	// only lives on main).
	_ = makeFileChange(t, proj, "main.go",
		"package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"DIVERGED from main\")\n}\n",
		"main divergence")

	// Edit the same line on the agent's branch.
	_ = makeFileChange(t, wt.Path, "main.go",
		"package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"agent version\")\n}\n",
		"agent edit")

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

	if result.Outcome != OutcomeMergeConflict {
		t.Errorf("Outcome = %s, want merge-conflict\nMergeError: %v", result.Outcome, result.MergeError)
	}

	// Main should not have the agent's commit.
	out := gitInDir(t, proj, "log", "--oneline", "main")
	if strings.Contains(out, "agent edit") {
		t.Errorf("main has agent commit despite conflict:\n%s", out)
	}
}

func TestIntegrator_Run_DrainsQueue(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	}
	in, _ := NewIntegrator(cfg)

	q := NewIntegrationQueue(4)

	// Provision two non-conflicting branches.
	wt1, _ := mgr.Provision(context.Background(), "agent1")
	_ = makeFileChange(t, wt1.Path, "feature_a.go", "package main\n\nfunc featureA() {}\n", "feature a")

	wt2, _ := mgr.Provision(context.Background(), "agent2")
	_ = makeFileChange(t, wt2.Path, "feature_b.go", "package main\n\nfunc featureB() {}\n", "feature b")

	// Submit both
	_ = q.Submit(context.Background(), IntegrationRequest{
		AgentName: "agent1", Branch: wt1.Branch, Worktree: wt1.Path,
	})
	_ = q.Submit(context.Background(), IntegrationRequest{
		AgentName: "agent2", Branch: wt2.Branch, Worktree: wt2.Path,
	})

	// Run the integrator until both commits land on main.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = in.Run(ctx, q)
		close(done)
	}()

	// Poll main's log until both commits appear (up to 60 seconds —
	// validators take real time, especially under -race).
	deadline := time.Now().Add(60 * time.Second)
	var lastLog string
	for time.Now().Before(deadline) {
		lastLog = gitInDir(t, proj, "log", "--oneline", "main")
		if strings.Contains(lastLog, "feature a") && strings.Contains(lastLog, "feature b") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-done

	if !strings.Contains(lastLog, "feature a") {
		t.Errorf("main missing feature a:\n%s", lastLog)
	}
	if !strings.Contains(lastLog, "feature b") {
		t.Errorf("main missing feature b:\n%s", lastLog)
	}
}

func TestIntegrator_Run_CtxCancellationStopsCleanly(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	}
	in, _ := NewIntegrator(cfg)

	q := NewIntegrationQueue(4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- in.Run(ctx, q)
	}()

	// Cancel without submitting anything.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestHasUnmergedPaths(t *testing.T) {
	cases := map[string]bool{
		"":                   false,
		"M  foo.go":          false,
		"UU main.go":         true,
		"AA new.go":          true,
		" M foo.go\nUU bar":  true,
		"M  a.go\n M b.go":   false,
	}
	for input, want := range cases {
		got := hasUnmergedPaths(input)
		if got != want {
			t.Errorf("hasUnmergedPaths(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestShortBranch(t *testing.T) {
	cases := map[string]string{
		"agent-proto/a1b2c3":  "a1b2c3",
		"agent-connect/xyz":   "xyz",
		"main":                "main",
		"feature/sub/branch":  "branch",
	}
	for input, want := range cases {
		got := shortBranch(input)
		if got != want {
			t.Errorf("shortBranch(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestIntegrator_ParallelProcessIsSerial verifies that calling Process
// from multiple goroutines doesn't race on main's state. The mutex
// in Integrator should serialize them.
func TestIntegrator_ParallelProcessIsSerial(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	}
	in, _ := NewIntegrator(cfg)

	// Provision 3 non-conflicting branches.
	type branchInfo struct {
		name string
		wt   *Worktree
	}
	var branches []branchInfo
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("agent%d", i)
		wt, err := mgr.Provision(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		_ = makeFileChange(t, wt.Path, fmt.Sprintf("file_%d.go", i),
			fmt.Sprintf("package main\n\nfunc fn%d() {}\n", i),
			fmt.Sprintf("change %d", i))
		branches = append(branches, branchInfo{name, wt})
	}

	var wg sync.WaitGroup
	results := make([]IntegrationResult, len(branches))
	for i, b := range branches {
		wg.Add(1)
		go func(idx int, b branchInfo) {
			defer wg.Done()
			results[idx] = in.Process(context.Background(), IntegrationRequest{
				AgentName: b.name,
				Branch:    b.wt.Branch,
				Worktree:  b.wt.Path,
			})
		}(i, b)
	}
	wg.Wait()

	mergedCount := 0
	for _, r := range results {
		if r.Outcome == OutcomeMerged {
			mergedCount++
		}
	}
	if mergedCount != len(branches) {
		t.Errorf("merged %d/%d branches; outcomes: %+v", mergedCount, len(branches),
			func() []string {
				out := make([]string, len(results))
				for i, r := range results {
					out[i] = r.Outcome.String()
				}
				return out
			}())
	}
}

// Need an agent.ConfirmFunc-typed nil for some tests
var _ agent.ConfirmFunc = alwaysConfirm
