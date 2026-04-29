// Repair contains the LLM-mediated repair logic the integrator uses
// when a clean merge or clean validation isn't achievable on the first
// try.
//
// Two paths:
//
//   1. Conflict repair: textual merge conflicts (the standard
//      <<<<<<< / ======= / >>>>>>> markers from git merge). We read
//      the conflicted files, ask Claude to resolve, write the
//      resolved content back, complete the merge.
//
//   2. Validator-failure repair: clean merge but go build / vet /
//      test failed on the merged tree. We send Claude the failure
//      output plus the relevant files, ask for fixes, apply them,
//      re-validate.
//
// Design principle: repair is a best-effort optimization, not a
// correctness primitive. When repair fails (no resolution offered,
// resolution still fails validators, hits budget), we surface the
// failure to the integrator's caller, which requeues the work to
// the originating agent. The validators are the safety net — a
// hallucinated repair that doesn't build doesn't reach main.
//
// Budget: per-MR cap on total LLM calls (default 3). Conflict
// resolution + validator repair share the same budget — fixing a
// conflict and then having a build failure consumes 2 of 3 rounds.
// Fourth attempt = give up.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vinodhalaharvi/coven/llm"
)

// RepairConfig configures the LLM repair behavior. The integrator
// constructs one of these and passes it to repair functions; nil
// RepairConfig means "no repair" (legacy 6a behavior).
type RepairConfig struct {
	// Sender is the Claude API client used for repair prompts.
	// Required.
	Sender llm.Sender

	// MaxRoundsPerMR caps total LLM repair calls per MR across both
	// conflict-resolution and validator-failure paths. Default 3.
	MaxRoundsPerMR int

	// MaxConflictRounds caps just the conflict-resolution path.
	// Default 2 (per the design plan: "max 2 textual conflict rounds").
	MaxConflictRounds int

	// MaxValidatorRounds caps the validator-failure repair path.
	// Default 1 (per the design plan: "1 round for build failures").
	MaxValidatorRounds int
}

// withDefaults populates zero fields with sensible defaults.
func (c RepairConfig) withDefaults() RepairConfig {
	if c.MaxRoundsPerMR <= 0 {
		c.MaxRoundsPerMR = 3
	}
	if c.MaxConflictRounds <= 0 {
		c.MaxConflictRounds = 2
	}
	if c.MaxValidatorRounds <= 0 {
		c.MaxValidatorRounds = 1
	}
	return c
}

// RepairResult describes what happened during repair.
type RepairResult struct {
	// Succeeded is true if repair ran to completion AND the resulting
	// tree passes the relevant check (merge resolves cleanly, or
	// validators pass).
	Succeeded bool

	// RoundsUsed is the number of LLM calls actually made (regardless
	// of success). Useful for the budget tracker.
	RoundsUsed int

	// Reasoning captures Claude's explanations across rounds. Useful
	// for logging and for the requeue payload if repair fails.
	Reasoning []string

	// Err is non-nil if repair failed unrecoverably (LLM error,
	// budget exhausted, parse failure). nil means either Succeeded
	// is true OR the repair gave up cleanly (file writes worked but
	// validators still fail).
	Err error
}

// repairProposal is the JSON shape we expect Claude to return for
// both repair paths.
type repairProposal struct {
	Files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
	Reasoning string `json:"reasoning"`
}

// fileWithContent pairs a path with its content. Used as input to
// buildConflictPrompt and other helpers.
type fileWithContent struct {
	path    string
	content string
}

// resolveConflicts attempts to resolve textual merge conflicts in the
// worktree by asking Claude to rewrite the conflicted files.
//
// Preconditions: a `git merge` has been attempted in the worktree
// and produced conflict markers. The merge is paused (`git status`
// shows unmerged paths).
//
// Postconditions on success: all conflict markers are gone, the
// resolved files are committed (completing the merge), and the
// caller can proceed to run validators. Postconditions on failure:
// the worktree is left in its conflicted state for the caller to
// abort.
//
// Caller is responsible for tracking budget and stopping the loop.
// This function performs ONE round.
func resolveConflicts(ctx context.Context, sender llm.Sender, worktree string, conflictedFiles []string) (RepairResult, error) {
	if len(conflictedFiles) == 0 {
		return RepairResult{}, fmt.Errorf("no conflicted files to resolve")
	}
	result := RepairResult{}

	// Read each conflicted file (with markers intact).
	var conflicted []fileWithContent
	for _, p := range conflictedFiles {
		full := filepath.Join(worktree, p)
		data, err := os.ReadFile(full)
		if err != nil {
			result.Err = fmt.Errorf("read conflicted file %s: %w", p, err)
			return result, result.Err
		}
		conflicted = append(conflicted, fileWithContent{path: p, content: string(data)})
	}

	prompt := buildConflictPrompt(conflicted)

	msg, _, err := sender(ctx, conflictResolverSystemPrompt, []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Text: prompt}}},
	}, nil)
	result.RoundsUsed = 1
	if err != nil {
		result.Err = fmt.Errorf("conflict resolver LLM call: %w", err)
		return result, result.Err
	}

	var text string
	for _, b := range msg.Blocks {
		if b.Text != "" {
			text += b.Text
		}
	}
	if text == "" {
		result.Err = fmt.Errorf("conflict resolver: empty response")
		return result, result.Err
	}

	proposal, err := parseRepairProposal(text)
	if err != nil {
		result.Err = fmt.Errorf("conflict resolver: parsing response: %w", err)
		return result, result.Err
	}
	result.Reasoning = append(result.Reasoning, proposal.Reasoning)

	// Verify the proposal covers all conflicted files.
	proposedPaths := make(map[string]string, len(proposal.Files))
	for _, f := range proposal.Files {
		proposedPaths[f.Path] = f.Content
	}
	for _, p := range conflictedFiles {
		if _, ok := proposedPaths[p]; !ok {
			result.Err = fmt.Errorf("conflict resolver: did not propose content for conflicted file %s", p)
			return result, result.Err
		}
	}

	// Write resolved files back to the worktree.
	for path, content := range proposedPaths {
		full := filepath.Join(worktree, path)
		// Sanity: refuse to write outside the worktree (defense
		// against path-traversal in malformed proposals).
		clean := filepath.Clean(full)
		if !strings.HasPrefix(clean, filepath.Clean(worktree)+string(filepath.Separator)) && clean != filepath.Clean(worktree) {
			result.Err = fmt.Errorf("repair: refusing to write outside worktree: %s", path)
			return result, result.Err
		}
		// Verify no markers remain (defense against Claude returning
		// unresolved content).
		if hasConflictMarkers(content) {
			result.Err = fmt.Errorf("repair: proposed content for %s still has conflict markers", path)
			return result, result.Err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			result.Err = fmt.Errorf("mkdir for %s: %w", path, err)
			return result, result.Err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			result.Err = fmt.Errorf("write %s: %w", path, err)
			return result, result.Err
		}
	}

	// Stage and complete the merge commit.
	if err := runGitCmd(ctx, worktree, "add", "."); err != nil {
		result.Err = fmt.Errorf("git add after repair: %w", err)
		return result, result.Err
	}
	if err := runGitCmd(ctx, worktree, "commit", "--no-edit"); err != nil {
		// "--no-edit" preserves the merge commit message git generated.
		// If commit fails (e.g., no changes — repair was a no-op),
		// surface that.
		result.Err = fmt.Errorf("git commit after repair: %w", err)
		return result, result.Err
	}

	result.Succeeded = true
	return result, nil
}

// repairValidatorFailure attempts to fix files that caused validator
// failures, by sending Claude the failure output plus relevant files.
//
// Preconditions: the worktree is on a clean merge (no conflict
// markers, but build/vet/test failed). The integrator has the failed
// validator results.
//
// Postconditions on success: files are updated, a follow-up commit
// captures the fix, validators pass when re-run by the caller.
// Postconditions on failure: worktree is left modified or unchanged;
// caller decides whether to abort the merge.
//
// Performs ONE round. Caller tracks budget.
func repairValidatorFailure(ctx context.Context, sender llm.Sender, worktree string, results []ValidatorResult) (RepairResult, error) {
	result := RepairResult{}

	// Identify the candidate files for repair: union of files mentioned
	// in any failed validator's output, plus a few common suspects
	// (changed files since base). Cap to avoid huge prompts.
	candidates := extractFailingFilePaths(results, worktree)

	// Build {path: content} map for the prompt.
	files := make(map[string]string, len(candidates))
	for _, p := range candidates {
		full := filepath.Join(worktree, p)
		data, err := os.ReadFile(full)
		if err != nil {
			// File might not exist (e.g., test references missing
			// file). Skip silently; Claude still has the validator
			// output.
			continue
		}
		files[p] = string(data)
	}

	prompt := buildValidatorRepairPrompt(results, files)

	msg, _, err := sender(ctx, validatorRepairSystemPrompt, []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Text: prompt}}},
	}, nil)
	result.RoundsUsed = 1
	if err != nil {
		result.Err = fmt.Errorf("validator repair LLM call: %w", err)
		return result, result.Err
	}

	var text string
	for _, b := range msg.Blocks {
		if b.Text != "" {
			text += b.Text
		}
	}
	if text == "" {
		result.Err = fmt.Errorf("validator repair: empty response")
		return result, result.Err
	}

	proposal, err := parseRepairProposal(text)
	if err != nil {
		result.Err = fmt.Errorf("validator repair: parsing response: %w", err)
		return result, result.Err
	}
	result.Reasoning = append(result.Reasoning, proposal.Reasoning)

	if len(proposal.Files) == 0 {
		// Claude declined to propose any fixes. That's a valid response
		// when the failure is unfixable from this context (e.g.
		// missing dependency). Surface as a soft failure.
		result.Err = fmt.Errorf("validator repair: no fix proposed: %s", proposal.Reasoning)
		return result, result.Err
	}

	// Apply each proposed file write.
	for _, f := range proposal.Files {
		full := filepath.Join(worktree, f.Path)
		clean := filepath.Clean(full)
		if !strings.HasPrefix(clean, filepath.Clean(worktree)+string(filepath.Separator)) && clean != filepath.Clean(worktree) {
			result.Err = fmt.Errorf("validator repair: refusing to write outside worktree: %s", f.Path)
			return result, result.Err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			result.Err = fmt.Errorf("mkdir for %s: %w", f.Path, err)
			return result, result.Err
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			result.Err = fmt.Errorf("write %s: %w", f.Path, err)
			return result, result.Err
		}
	}

	// Stage and commit the repair.
	if err := runGitCmd(ctx, worktree, "add", "."); err != nil {
		result.Err = fmt.Errorf("git add after validator repair: %w", err)
		return result, result.Err
	}
	if err := runGitCmd(ctx, worktree, "commit", "-m", "repair: fix validator failures"); err != nil {
		// Could fail because no actual changes were made (Claude
		// returned files identical to existing). Treat as failure to
		// repair.
		result.Err = fmt.Errorf("git commit after validator repair: %w", err)
		return result, result.Err
	}

	result.Succeeded = true
	return result, nil
}

// parseRepairProposal extracts the JSON object from Claude's response.
// Reuses the same forgiving extraction logic as the router.
func parseRepairProposal(text string) (*repairProposal, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	if !strings.HasPrefix(text, "{") {
		if i := strings.Index(text, "{"); i >= 0 {
			text = text[i:]
		}
	}
	if !strings.HasSuffix(text, "}") {
		if i := strings.LastIndex(text, "}"); i >= 0 {
			text = text[:i+1]
		}
	}

	var prop repairProposal
	if err := json.Unmarshal([]byte(text), &prop); err != nil {
		return nil, err
	}
	return &prop, nil
}

// hasConflictMarkers checks for git conflict marker lines.
func hasConflictMarkers(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "<<<<<<<") || strings.HasPrefix(line, "=======") || strings.HasPrefix(line, ">>>>>>>") {
			return true
		}
	}
	return false
}

// listConflictedFiles returns the relative paths of files in the
// worktree currently in a conflicted (UU/AA/...) state. Uses git
// ls-files --unmerged.
func listConflictedFiles(ctx context.Context, worktree string) ([]string, error) {
	out, err := runGitCmdOutput(ctx, worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			files = append(files, line)
			seen[line] = true
		}
	}
	return files, nil
}

// extractFailingFilePaths scans validator output for file paths and
// returns relative paths that exist in the worktree.
//
// Heuristic: look for "path:line:col" patterns and "path:" patterns
// in stderr. Filter to files actually present in the worktree.
//
// We don't try to be clever — we just gather candidates. If we miss
// a file, the LLM still has the raw failure output.
func extractFailingFilePaths(results []ValidatorResult, worktree string) []string {
	seen := make(map[string]bool)
	var paths []string

	for _, r := range results {
		if r.Passed() {
			continue
		}
		for _, line := range strings.Split(r.Output, "\n") {
			// Match "path/to/file.go:42:7:" or "path/to/file.go:42:"
			// or "./path/file.go".
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Crude parser: if line starts with something looking like
			// a path (contains ".go" or ".proto" etc.), extract up to
			// the first colon.
			candidate := extractPathFromErrorLine(line)
			if candidate == "" {
				continue
			}
			if seen[candidate] {
				continue
			}
			full := filepath.Join(worktree, candidate)
			if _, err := os.Stat(full); err != nil {
				continue
			}
			seen[candidate] = true
			paths = append(paths, candidate)
			if len(paths) >= 10 {
				return paths // cap to avoid runaway prompts
			}
		}
	}
	return paths
}

// extractPathFromErrorLine pulls the file path out of a Go compiler
// error line. Returns "" if no path is detected.
func extractPathFromErrorLine(line string) string {
	// Strip leading ./
	line = strings.TrimPrefix(line, "./")

	// First colon-separated field, if it ends with a known extension.
	if i := strings.IndexByte(line, ':'); i > 0 {
		path := line[:i]
		// Extension check
		exts := []string{".go", ".proto", ".sql", ".yaml", ".yml", ".json"}
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				return path
			}
		}
	}
	return ""
}

// runGitCmd runs git in worktree, discarding stdout/stderr. Returns
// error with stderr included for debuggability.
func runGitCmd(ctx context.Context, worktree string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runGitCmdOutput runs git and returns combined output.
func runGitCmdOutput(ctx context.Context, worktree string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// =============================================================================
// PROMPTS
// =============================================================================

const conflictResolverSystemPrompt = `You are resolving a git merge conflict in a Go project.

You will receive one or more files containing conflict markers (<<<<<<<, =======, >>>>>>>). Your job is to produce the correct merged content for each file — combining both sides intelligently rather than picking one or the other.

Output STRICT JSON of the form:
{
  "files": [
    {"path": "relative/path/to/file.go", "content": "...full resolved file content..."},
    ...
  ],
  "reasoning": "brief explanation of how you resolved the conflicts"
}

Rules:
- Include EVERY conflicted file in your output, with full resolved content.
- Do not include conflict markers in the output. The result must be valid Go code (or whatever the file's language is).
- When in doubt, prefer combining changes (e.g., if one side adds a function and the other modifies a different function, the result has both). Only pick a single side when the changes are genuinely incompatible.
- Preserve formatting and conventions from the existing file. Don't reflow or reformat.
- Do not invent functionality. If the conflict is genuinely unresolvable from this context (e.g., conflicting renames whose semantics aren't clear), say so in reasoning and return an empty files list — the system will fall back to other handling.

Output the JSON object directly. No markdown fences, no prose before or after.`

const validatorRepairSystemPrompt = `You are repairing a Go project that fails to build, vet, or test cleanly after a merge.

You will receive:
1. The output of the failing validator(s) — go build, go vet, or go test.
2. The current contents of files implicated by the failures.

Your job is to propose minimal fixes that make the validators pass, returning the FULL new content of any files you change.

Output STRICT JSON of the form:
{
  "files": [
    {"path": "relative/path/to/file.go", "content": "...full new content..."},
    ...
  ],
  "reasoning": "brief explanation of what you changed and why"
}

Rules:
- Only include files you actually want to change. Don't include unchanged files.
- Provide the full file content, not a diff.
- Make the smallest changes that make the failures go away. Don't refactor unrelated code.
- If the failure is genuinely unfixable from this context (e.g., missing dependency, requires changes outside the visible files), explain in reasoning and return an empty files list. The system will requeue the work.
- Preserve formatting and conventions.

Output the JSON object directly. No markdown fences, no prose before or after.`

func buildConflictPrompt(files []fileWithContent) string {
	var b strings.Builder
	b.WriteString("Resolve the merge conflicts in the following file(s):\n\n")
	for _, f := range files {
		fmt.Fprintf(&b, "=== %s ===\n", f.path)
		b.WriteString(f.content)
		if !strings.HasSuffix(f.content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Respond with JSON only.")
	return b.String()
}

func buildValidatorRepairPrompt(results []ValidatorResult, files map[string]string) string {
	var b strings.Builder
	b.WriteString("The following validators failed after a merge:\n\n")
	for _, r := range results {
		if r.Passed() {
			continue
		}
		fmt.Fprintf(&b, "=== %s (FAILED) ===\n", r.Validator.Name)
		fmt.Fprintf(&b, "Command: %s\n", r.Validator.Command)
		fmt.Fprintf(&b, "Error: %v\n", r.Err)
		b.WriteString("Output:\n")
		// Cap at ~3KB per validator output to leave headroom for files.
		if len(r.Output) > 3000 {
			b.WriteString(r.Output[:1500])
			b.WriteString("\n...(truncated)...\n")
			b.WriteString(r.Output[len(r.Output)-1500:])
		} else {
			b.WriteString(r.Output)
		}
		b.WriteString("\n\n")
	}

	if len(files) > 0 {
		b.WriteString("Current contents of files implicated by the failures:\n\n")
		for path, content := range files {
			fmt.Fprintf(&b, "=== %s ===\n", path)
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("Respond with JSON only.")
	return b.String()
}
