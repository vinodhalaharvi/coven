// Validators are commands the integrator runs against a candidate
// merge to verify the result is correct before letting it land on
// main. They're the v2 equivalent of v1's per-agent build-health
// pings, but centralized and uniform.
//
// A validator is matched by file extension or by always running
// regardless of what changed. The default v2 set runs on Go projects:
//   - go build ./...
//   - go vet ./...
//   - go test ./...
//
// Adding a validator for a new file type (proto, sql, yaml) is a
// matter of registering it; the integrator code doesn't change.
//
// Validators are deliberately commands, not Go functions. This keeps
// them composable with any toolchain (Go, Bazel, Make, custom shell
// scripts) and lets users override or extend them via configuration
// without touching code.
package controlplane

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Validator is a command run against a worktree to verify the changes
// in it are correct.
type Validator struct {
	// Name is a short identifier for logs and failure reports
	// (e.g. "go-build", "go-test").
	Name string

	// Command is the shell command to run, executed via "sh -c" so
	// shell features (pipes, &&, ./...) work naturally.
	Command string

	// AppliesTo decides whether this validator should run for a given
	// changeset. Receives the list of changed file paths (relative to
	// the worktree). Returns true if the validator should run.
	//
	// nil AppliesTo means "always run" — the validator fires on every
	// changeset. Use this for cross-cutting validators like 'go build
	// ./...' that catch issues anywhere in the project regardless of
	// what specifically changed.
	AppliesTo func(changedFiles []string) bool

	// Timeout caps the validator's runtime. Zero means use a sensible
	// default (90 seconds — same as the standard exec tool's cap).
	Timeout time.Duration
}

// ValidatorResult is the outcome of running one validator.
type ValidatorResult struct {
	// Validator is the validator that ran. Useful for log lines and
	// for the LLM repair prompt later.
	Validator Validator

	// Output is the combined stdout + stderr of the validator command.
	// Truncated to a reasonable size for logging; full output goes to
	// the LLM repair prompt if needed.
	Output string

	// Err is the error from the validator command (if any). nil means
	// the validator passed. Non-nil means it failed — Output describes
	// what went wrong.
	Err error
}

// Passed reports whether this validator succeeded.
func (r ValidatorResult) Passed() bool {
	return r.Err == nil
}

// ValidatorRegistry holds the active set of validators. Concurrent-
// safe via the integrator's serial design (only one validation run at
// a time).
type ValidatorRegistry struct {
	validators []Validator
}

// NewValidatorRegistry returns a registry with the default v2 Go
// validators: build, vet, test. These run on every changeset (not
// just .go file changes) because cross-cutting issues — e.g. a proto
// regeneration affecting a Go file's imports — surface only when the
// build is exercised.
//
// Callers wanting a different set construct ValidatorRegistry directly.
func NewValidatorRegistry() *ValidatorRegistry {
	return &ValidatorRegistry{
		validators: defaultValidators(),
	}
}

// Add appends a validator to the registry. Order matters: validators
// run in registration order. Failures from earlier validators don't
// short-circuit later ones — we run all that apply and report all
// failures at once, so the LLM repair prompt has full context.
func (r *ValidatorRegistry) Add(v Validator) {
	r.validators = append(r.validators, v)
}

// All returns a copy of the registered validators in order. Useful
// for tests and inspection.
func (r *ValidatorRegistry) All() []Validator {
	out := make([]Validator, len(r.validators))
	copy(out, r.validators)
	return out
}

// RunAll executes every validator that applies to the changedFiles
// against the given workdir. Runs them serially to avoid resource
// contention (parallel go test invocations on the same module hit
// the build cache simultaneously and slow each other down).
//
// Returns ALL results, not just failures. The integrator uses passing
// results for logs and failing results for the repair prompt.
func (r *ValidatorRegistry) RunAll(ctx context.Context, workdir string, changedFiles []string) []ValidatorResult {
	var results []ValidatorResult
	for _, v := range r.validators {
		if v.AppliesTo != nil && !v.AppliesTo(changedFiles) {
			continue
		}
		results = append(results, runValidator(ctx, v, workdir))
	}
	return results
}

// AnyFailed returns true if any of the results show a failed
// validator.
func AnyFailed(results []ValidatorResult) bool {
	for _, r := range results {
		if !r.Passed() {
			return true
		}
	}
	return false
}

// FailureSummary builds a human-readable summary of failed validators
// suitable for logs or for showing the user. Passed validators are
// omitted. Used by the integrator to log what went wrong.
func FailureSummary(results []ValidatorResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Passed() {
			continue
		}
		fmt.Fprintf(&b, "[%s] FAIL: %v\n", r.Validator.Name, r.Err)
		if r.Output != "" {
			fmt.Fprintf(&b, "%s\n", truncateValidatorOutput(r.Output, 2000))
		}
	}
	return b.String()
}

// runValidator executes a single validator command in the given
// workdir, capturing combined output.
//
// We set the command into its own process group (Setpgid) so the
// context's timeout cancellation can kill the whole tree, not just
// the shell. Without this, "sh -c 'sleep 60'" would leak the sleep
// process when the context timed out — go's default CommandContext
// behavior signals only the top-level process.
func runValidator(ctx context.Context, v Validator, workdir string) ValidatorResult {
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", v.Command)
	cmd.Dir = workdir
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}

	out, err := cmd.CombinedOutput()
	return ValidatorResult{
		Validator: v,
		Output:    string(out),
		Err:       err,
	}
}

// defaultValidators returns the v2 standard Go validator set.
//
// These run unconditionally (AppliesTo == nil) because Go's
// cross-package compilation means a change in package A can break
// package B's build, and we have no cheap way to compute the affected
// set. 'go build ./...' is the bluntest correct answer; 'go vet' and
// 'go test' similarly want the full project.
//
// This is the place to add new validators. Each entry is just:
//   - a short Name for logs
//   - a Command (any shell expression)
//   - optionally an AppliesTo predicate to skip when irrelevant
//   - optionally a Timeout
func defaultValidators() []Validator {
	return []Validator{
		{
			Name:    "go-build",
			Command: "go build ./...",
		},
		{
			Name:    "go-vet",
			Command: "go vet ./...",
		},
		{
			Name:    "go-test",
			Command: "go test ./...",
			Timeout: 5 * time.Minute, // tests can be slow
		},
	}
}

// AppliesToExtension returns an AppliesTo predicate that matches any
// changed file with one of the given extensions. Extensions should
// include the dot (e.g. ".go", ".proto").
//
// Currently unused by defaultValidators (Go validators run always),
// but provided for users adding extension-specific validators like
// "buf lint" for .proto files.
func AppliesToExtension(exts ...string) func([]string) bool {
	return func(files []string) bool {
		for _, f := range files {
			ext := filepath.Ext(f)
			for _, e := range exts {
				if ext == e {
					return true
				}
			}
		}
		return false
	}
}

// truncateValidatorOutput shortens long validator output for
// inclusion in summaries, preserving both head and tail (compiler
// errors often appear at the very end).
func truncateValidatorOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + "\n...(truncated)...\n" + s[len(s)-half:]
}
