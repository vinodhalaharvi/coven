package controlplane

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initGoProject sets up a minimal Go project in a temp dir with one
// commit. Returns the project path. Used by integrator + validator
// tests that need a real `go build` / `go test` target.
func initGoProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}

	// Minimal main.go that builds, vets, and has a test that passes.
	files := map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.22\n",
		"main.go": `package main

import "fmt"

func add(a, b int) int { return a + b }

func main() {
	fmt.Println(add(2, 3))
}
`,
		"main_test.go": `package main

import "testing"

func TestAdd(t *testing.T) {
	if add(2, 3) != 5 {
		t.Fatal("math is broken")
	}
}
`,
	}
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test"},
		{"git", "config", "user.name", "test"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "initial"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup failed: %s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	return dir
}

func TestValidator_DefaultsRunOnGoProject(t *testing.T) {
	proj := initGoProject(t)
	reg := NewValidatorRegistry()

	results := reg.RunAll(context.Background(), proj, nil)
	if len(results) != 3 {
		t.Fatalf("expected 3 results (build, vet, test), got %d", len(results))
	}
	for _, r := range results {
		if !r.Passed() {
			t.Errorf("[%s] failed unexpectedly: %v\n%s", r.Validator.Name, r.Err, r.Output)
		}
	}
}

func TestValidator_FailsOnBrokenBuild(t *testing.T) {
	proj := initGoProject(t)

	// Break the build by injecting a syntax error.
	if err := os.WriteFile(filepath.Join(proj, "broken.go"), []byte("package main\nfunc broken( ) { return notADefinedSymbol }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewValidatorRegistry()
	results := reg.RunAll(context.Background(), proj, nil)

	if !AnyFailed(results) {
		t.Error("expected at least one validator to fail")
	}

	// Specifically go-build should fail.
	var buildResult ValidatorResult
	for _, r := range results {
		if r.Validator.Name == "go-build" {
			buildResult = r
		}
	}
	if buildResult.Passed() {
		t.Error("go-build should have failed on broken syntax")
	}
	if !strings.Contains(buildResult.Output, "broken") && !strings.Contains(buildResult.Output, "notADefinedSymbol") {
		t.Errorf("output doesn't mention the broken file/symbol:\n%s", buildResult.Output)
	}
}

func TestValidator_FailureSummaryIncludesFailedValidators(t *testing.T) {
	results := []ValidatorResult{
		{
			Validator: Validator{Name: "go-build"},
			Output:    "main.go:5:1: undefined: foo",
			Err:       errors.New("exit status 1"),
		},
		{
			Validator: Validator{Name: "go-vet"},
			Output:    "ok",
			Err:       nil, // passed
		},
	}
	summary := FailureSummary(results)
	if !strings.Contains(summary, "go-build") {
		t.Error("summary missing failed validator name")
	}
	if strings.Contains(summary, "go-vet") {
		t.Error("summary should not include passed validators")
	}
	if !strings.Contains(summary, "main.go") {
		t.Error("summary should include validator output")
	}
}

func TestValidator_AppliesToExtension(t *testing.T) {
	pred := AppliesToExtension(".proto", ".sql")

	cases := map[string]bool{
		"foo.proto":         true,
		"sub/foo.proto":     true,
		"queries.sql":       true,
		"main.go":           false,
		"":                  false,
	}
	for path, expected := range cases {
		got := pred([]string{path})
		if got != expected {
			t.Errorf("AppliesToExtension(%q) = %v, want %v", path, got, expected)
		}
	}

	// Multi-file: matches if ANY file has a relevant extension.
	if !pred([]string{"main.go", "foo.proto"}) {
		t.Error("predicate should match if any file has a relevant extension")
	}
	if pred([]string{"main.go", "README.md"}) {
		t.Error("predicate should not match when no files match")
	}
}

func TestValidator_AppliesToFiltersOutValidators(t *testing.T) {
	reg := &ValidatorRegistry{
		validators: []Validator{
			{
				Name:      "always",
				Command:   "true",
				AppliesTo: nil, // always
			},
			{
				Name:      "go-only",
				Command:   "true",
				AppliesTo: AppliesToExtension(".go"),
			},
			{
				Name:      "proto-only",
				Command:   "true",
				AppliesTo: AppliesToExtension(".proto"),
			},
		},
	}

	results := reg.RunAll(context.Background(), t.TempDir(), []string{"foo.go"})
	names := map[string]bool{}
	for _, r := range results {
		names[r.Validator.Name] = true
	}
	if !names["always"] {
		t.Error("always-validator should run")
	}
	if !names["go-only"] {
		t.Error("go-only validator should run for .go change")
	}
	if names["proto-only"] {
		t.Error("proto-only validator should NOT run for .go change")
	}
}

func TestValidator_TimeoutHonored(t *testing.T) {
	reg := &ValidatorRegistry{
		validators: []Validator{
			{
				Name:    "slow",
				Command: "sleep 5",
				Timeout: 100 * time.Millisecond,
			},
		},
	}

	start := time.Now()
	results := reg.RunAll(context.Background(), t.TempDir(), nil)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("validator did not honor short timeout: ran for %v", elapsed)
	}
	if len(results) != 1 || results[0].Passed() {
		t.Error("expected the slow validator to fail")
	}
}

func TestValidator_TruncateLongOutput(t *testing.T) {
	long := strings.Repeat("abcdefghij", 1000) // 10000 chars
	truncated := truncateValidatorOutput(long, 200)
	if len(truncated) >= len(long) {
		t.Error("output was not truncated")
	}
	if !strings.Contains(truncated, "truncated") {
		t.Error("truncation marker missing")
	}
	// Should preserve both head and tail.
	if !strings.HasPrefix(truncated, "abcdef") {
		t.Error("head of output not preserved")
	}
}
