package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vinodhalaharvi/coven/llm"
)

func TestHasConflictMarkers(t *testing.T) {
	cases := map[string]bool{
		"package main\nfunc x() {}\n":                                  false,
		"<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> branch\n":            true,
		"// some code\n<<<<<<< HEAD\nx\n=======\ny\n>>>>>>> b\n":       true,
		"// just =======\nfunc =======equal() {}\n":                   false, // markers must start the line
		"line 1\n<<<<<<< merge\nleft\n=======\nright\n>>>>>>> end\n":   true,
		"":                                                              false,
	}
	for input, want := range cases {
		got := hasConflictMarkers(input)
		if got != want {
			t.Errorf("hasConflictMarkers(%q) = %v, want %v", truncFor(input), got, want)
		}
	}
}

func truncFor(s string) string {
	if len(s) > 50 {
		return s[:50] + "..."
	}
	return s
}

func TestExtractPathFromErrorLine(t *testing.T) {
	cases := map[string]string{
		"main.go:5:1: undefined: foo":                  "main.go",
		"./internal/handlers/order.go:42:7: error":     "internal/handlers/order.go",
		"FAIL\texample.com/test\t0.001s":               "",
		"undefined: bar":                                "",
		"":                                              "",
		"path/to/file.go:1: syntax error":              "path/to/file.go",
		"queries.sql:10: syntax":                       "queries.sql",
		"foo.proto:5: missing":                         "foo.proto",
		"some random text":                             "",
	}
	for input, want := range cases {
		got := extractPathFromErrorLine(input)
		if got != want {
			t.Errorf("extractPathFromErrorLine(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseRepairProposal_ValidJSON(t *testing.T) {
	input := `{"files":[{"path":"main.go","content":"package main\n"}],"reasoning":"fixed it"}`
	prop, err := parseRepairProposal(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(prop.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(prop.Files))
	}
	if prop.Files[0].Path != "main.go" {
		t.Errorf("path = %q", prop.Files[0].Path)
	}
	if prop.Reasoning != "fixed it" {
		t.Errorf("reasoning = %q", prop.Reasoning)
	}
}

func TestParseRepairProposal_HandlesMarkdownAndProse(t *testing.T) {
	cases := map[string]string{
		"plain":     `{"files":[],"reasoning":"x"}`,
		"fenced":    "```json\n{\"files\":[],\"reasoning\":\"x\"}\n```",
		"with prose": "Here is the fix:\n\n{\"files\":[],\"reasoning\":\"x\"}\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			prop, err := parseRepairProposal(input)
			if err != nil {
				t.Errorf("parse failed: %v", err)
				return
			}
			if prop.Reasoning != "x" {
				t.Errorf("reasoning = %q", prop.Reasoning)
			}
		})
	}
}

func TestParseRepairProposal_RejectsMalformed(t *testing.T) {
	_, err := parseRepairProposal("this is not JSON")
	if err == nil {
		t.Error("expected error for non-JSON input")
	}
}

// Conflict repair end-to-end test. We set up a real conflict, have
// scripted Claude return a valid resolution, and verify the merge
// completes.
func TestIntegrator_Process_ConflictRepair_HappyPath(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	// Create a conflict: edit main.go on main, then in a worktree.
	_ = makeFileChange(t, proj, "main.go",
		"package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"main version\")\n}\n",
		"main edit")

	wt, _ := mgr.Provision(context.Background(), "alpha")
	defer mgr.Cleanup(context.Background(), wt)

	_ = makeFileChange(t, proj, "main.go",
		"package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"DIVERGED\")\n}\n",
		"main divergence")

	_ = makeFileChange(t, wt.Path, "main.go",
		"package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"agent version\")\n}\n",
		"agent edit")

	// Scripted Claude that returns a valid resolution combining both.
	resolved := "package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(\"DIVERGED\")\n\tfmt.Println(\"agent version\")\n}\n"
	resp := fmt.Sprintf(`{"files":[{"path":"main.go","content":%q}],"reasoning":"combined both prints"}`, resolved)
	sender := llm.ScriptedSender(llm.TextResponse(resp))

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
		Repair: &RepairConfig{
			Sender:            sender,
			MaxConflictRounds: 2,
			MaxRoundsPerMR:    3,
		},
	}
	in, _ := NewIntegrator(cfg)

	result := in.Process(context.Background(), IntegrationRequest{
		AgentName: "alpha",
		Branch:    wt.Branch,
		Worktree:  wt.Path,
	})

	if result.Outcome != OutcomeMerged {
		t.Errorf("Outcome = %s, want merged. MergeError: %v", result.Outcome, result.MergeError)
		t.Logf("RepairAttempts: %+v", result.RepairAttempts)
	}
	if len(result.RepairAttempts) == 0 {
		t.Error("expected at least one repair attempt to be recorded")
	}
	if !result.RepairAttempts[0].Succeeded {
		t.Error("first repair attempt should have succeeded")
	}

	// Verify main has the resolved content.
	out := gitInDir(t, proj, "log", "--oneline", "main")
	if !strings.Contains(out, "agent edit") || !strings.Contains(out, "main divergence") {
		t.Errorf("expected both commits on main:\n%s", out)
	}
}

func TestIntegrator_Process_ConflictRepair_RejectsMarkersInOutput(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	_ = makeFileChange(t, proj, "main.go",
		"package main\n\nfunc main() { x := 1; _ = x }\n",
		"main edit")

	wt, _ := mgr.Provision(context.Background(), "alpha")
	defer mgr.Cleanup(context.Background(), wt)

	_ = makeFileChange(t, proj, "main.go",
		"package main\n\nfunc main() { x := 100; _ = x }\n",
		"main divergence")

	_ = makeFileChange(t, wt.Path, "main.go",
		"package main\n\nfunc main() { x := 999; _ = x }\n",
		"agent edit")

	// Claude "resolution" still has conflict markers — should be
	// rejected by the integrator.
	bad := "package main\n<<<<<<< HEAD\nfunc main() { x := 100; _ = x }\n=======\nfunc main() { x := 999; _ = x }\n>>>>>>> agent\n"
	resp := fmt.Sprintf(`{"files":[{"path":"main.go","content":%q}],"reasoning":"failed"}`, bad)
	sender := llm.ScriptedSender(
		llm.TextResponse(resp),
		llm.TextResponse(resp), // same bad response twice; budget will exhaust
	)

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
		Repair: &RepairConfig{
			Sender:            sender,
			MaxConflictRounds: 2,
		},
	}
	in, _ := NewIntegrator(cfg)

	result := in.Process(context.Background(), IntegrationRequest{
		AgentName: "alpha",
		Branch:    wt.Branch,
		Worktree:  wt.Path,
	})

	if result.Outcome != OutcomeMergeConflict {
		t.Errorf("Outcome = %s, want merge-conflict (Claude returned bad resolution)", result.Outcome)
	}
}

func TestIntegrator_Process_ValidatorRepair_HappyPath(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	wt, _ := mgr.Provision(context.Background(), "alpha")
	defer mgr.Cleanup(context.Background(), wt)

	// Agent commits a broken file
	_ = makeFileChange(t, wt.Path, "broken.go",
		"package main\n\nfunc broken() { return undefinedSymbol }\n",
		"break the build")

	// Scripted Claude returns the fix (file with no return statement)
	fixed := "package main\n\nfunc broken() {}\n"
	resp := fmt.Sprintf(`{"files":[{"path":"broken.go","content":%q}],"reasoning":"removed undefined symbol"}`, fixed)
	sender := llm.ScriptedSender(llm.TextResponse(resp))

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
		Repair: &RepairConfig{
			Sender:             sender,
			MaxValidatorRounds: 1,
			MaxRoundsPerMR:     3,
		},
	}
	in, _ := NewIntegrator(cfg)

	result := in.Process(context.Background(), IntegrationRequest{
		AgentName: "alpha",
		Branch:    wt.Branch,
		Worktree:  wt.Path,
	})

	if result.Outcome != OutcomeMerged {
		t.Errorf("Outcome = %s, want merged. RepairAttempts: %+v", result.Outcome, result.RepairAttempts)
	}
	if len(result.RepairAttempts) == 0 {
		t.Error("expected at least one repair attempt")
	}
}

func TestIntegrator_Process_ValidatorRepair_NoFixOffered(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	wt, _ := mgr.Provision(context.Background(), "alpha")
	defer mgr.Cleanup(context.Background(), wt)

	_ = makeFileChange(t, wt.Path, "broken.go",
		"package main\n\nfunc broken() { return undefinedSymbol }\n",
		"break the build")

	// Claude declines to fix.
	sender := llm.ScriptedSender(
		llm.TextResponse(`{"files":[],"reasoning":"cannot fix from this context"}`),
	)

	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
		Repair: &RepairConfig{
			Sender:             sender,
			MaxValidatorRounds: 1,
		},
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
	// Should have attempted repair once
	if len(result.RepairAttempts) == 0 {
		t.Error("expected repair attempt to be recorded")
	}
}

func TestIntegrator_Process_NoRepairConfigPreserves6aBehavior(t *testing.T) {
	proj := initGoProject(t)
	mgr := NewWorktreeMgr(proj)
	defer mgr.CleanupOrphans(context.Background())

	wt, _ := mgr.Provision(context.Background(), "alpha")
	defer mgr.Cleanup(context.Background(), wt)

	_ = makeFileChange(t, wt.Path, "broken.go",
		"package main\n\nfunc broken() { return undefinedSymbol }\n",
		"break the build")

	// No Repair config — should fail immediately like 6a.
	cfg := IntegratorConfig{
		ProjectRoot: proj,
		WorktreeMgr: mgr,
		Validators:  NewValidatorRegistry(),
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
		// Repair: nil
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
	if len(result.RepairAttempts) != 0 {
		t.Errorf("no repair config but RepairAttempts = %d", len(result.RepairAttempts))
	}
}

// Test the path-traversal defense.
func TestResolveConflicts_RefusesPathTraversal(t *testing.T) {
	worktree := t.TempDir()
	if r, err := filepath.EvalSymlinks(worktree); err == nil {
		worktree = r
	}

	// Make a fake conflicted file that exists.
	conflicted := filepath.Join(worktree, "main.go")
	if err := os.WriteFile(conflicted, []byte("<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Claude proposes writing to ../../etc/passwd
	resp := `{"files":[{"path":"../../etc/passwd","content":"evil"}],"reasoning":"trying to escape"}`
	sender := llm.ScriptedSender(llm.TextResponse(resp))

	result, err := resolveConflicts(context.Background(), sender, worktree, []string{"main.go"})
	if err == nil {
		t.Error("expected error refusing path traversal")
	}
	if result.Succeeded {
		t.Error("repair should not have succeeded with traversal attempt")
	}
}

func TestRepairConfig_WithDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   RepairConfig
		want RepairConfig
	}{
		{"all zero", RepairConfig{}, RepairConfig{
			MaxRoundsPerMR: 3, MaxConflictRounds: 2, MaxValidatorRounds: 1,
		}},
		{"partial override", RepairConfig{MaxRoundsPerMR: 5}, RepairConfig{
			MaxRoundsPerMR: 5, MaxConflictRounds: 2, MaxValidatorRounds: 1,
		}},
		{"all set", RepairConfig{MaxRoundsPerMR: 10, MaxConflictRounds: 4, MaxValidatorRounds: 3}, RepairConfig{
			MaxRoundsPerMR: 10, MaxConflictRounds: 4, MaxValidatorRounds: 3,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.MaxRoundsPerMR != tc.want.MaxRoundsPerMR {
				t.Errorf("MaxRoundsPerMR = %d, want %d", got.MaxRoundsPerMR, tc.want.MaxRoundsPerMR)
			}
			if got.MaxConflictRounds != tc.want.MaxConflictRounds {
				t.Errorf("MaxConflictRounds = %d, want %d", got.MaxConflictRounds, tc.want.MaxConflictRounds)
			}
			if got.MaxValidatorRounds != tc.want.MaxValidatorRounds {
				t.Errorf("MaxValidatorRounds = %d, want %d", got.MaxValidatorRounds, tc.want.MaxValidatorRounds)
			}
		})
	}
}

func TestExtractFailingFilePaths_HandlesGoCompilerErrors(t *testing.T) {
	worktree := t.TempDir()
	// Create some real files
	for _, p := range []string{"main.go", "internal/foo.go"} {
		full := filepath.Join(worktree, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte("package x"), 0o644)
	}

	results := []ValidatorResult{
		{
			Validator: Validator{Name: "go-build"},
			Output: `# example.com/test
main.go:5:1: undefined: foo
internal/foo.go:10:5: cannot use bar
some-other-output`,
			Err: fmt.Errorf("exit 1"),
		},
	}

	paths := extractFailingFilePaths(results, worktree)

	// Should find both real files
	got := make(map[string]bool)
	for _, p := range paths {
		got[p] = true
	}
	if !got["main.go"] {
		t.Errorf("missing main.go in extracted paths: %v", paths)
	}
	if !got["internal/foo.go"] {
		t.Errorf("missing internal/foo.go in extracted paths: %v", paths)
	}
}

func TestExtractFailingFilePaths_SkipsNonexistentFiles(t *testing.T) {
	worktree := t.TempDir()
	// Don't create any real files

	results := []ValidatorResult{
		{
			Validator: Validator{Name: "go-build"},
			Output:    "imaginary.go:5:1: error",
			Err:       fmt.Errorf("exit 1"),
		},
	}

	paths := extractFailingFilePaths(results, worktree)
	if len(paths) != 0 {
		t.Errorf("expected no paths (file doesn't exist), got %v", paths)
	}
}

func TestListConflictedFiles_OnCleanRepo(t *testing.T) {
	repo := initGoProject(t)
	files, err := listConflictedFiles(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("clean repo should have no conflicts, got %v", files)
	}
}
