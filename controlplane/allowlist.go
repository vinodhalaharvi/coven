// AllowList is the policy layer that decides which exec commands run
// silently and which require explicit user confirmation.
//
// Background:
//
// In v1, every exec tool call was gated by a y/N prompt. This was safe
// but tedious — file writes, codegen runs, build verifications all
// produced prompts even though they're routine. Users developed
// "approval fatigue" and risked silent-yes errors.
//
// In v2 the policy splits exec calls into two classes:
//
//   1. Standard operations (auto-approved, run silently): file writes,
//      codegen tools (buf generate, sqlc generate, wire), build/test
//      verification (go build, go test, go vet, go install), git
//      operations within the agent's own worktree.
//
//   2. Anything else (requires y/n): curl, npm, custom shell scripts,
//      rm, anything that could have side effects beyond the project
//      and its declared tooling.
//
// "Anything else" is the safe default — when in doubt, ask.
//
// The allow-list also supports per-pattern session memory: when the
// user explicitly approves a non-standard command, the allow-list
// remembers that approval for the rest of the session. This handles
// the case where an agent legitimately needs an unusual tool that's
// specific to the project (e.g. a custom CLI). The user approves
// once, the allow-list remembers, subsequent invocations of the same
// command run silently.
package controlplane

import (
	"strings"
	"sync"
)

// Decision is what AllowList returns for a given command.
type Decision int

const (
	// DecisionAllow means: run silently, no user prompt needed.
	DecisionAllow Decision = iota

	// DecisionConfirm means: ask the user via y/n. Default for unknown
	// commands.
	DecisionConfirm
)

// AllowList enforces the auto-approve policy. Safe for concurrent use.
type AllowList struct {
	mu sync.RWMutex

	// patterns is the set of pre-approved command patterns. Each
	// pattern matches a command if the command starts with the
	// pattern (after trimming whitespace). For example, the pattern
	// "go build" matches "go build ./...", "go build ./pkg",
	// "go build -v ./...", but NOT "go run" or "gobuild" (no space).
	patterns []string

	// remembered tracks user-approved patterns for the rest of the
	// session. When the user approves an unusual command via
	// RememberApproval, future commands matching the same pattern
	// auto-approve.
	remembered map[string]bool
}

// NewAllowList constructs an allow-list with the default v2 policy:
// standard codegen, build, test, git, and tool-installation operations
// pre-approved; everything else requires confirmation.
//
// Callers wanting a stricter or more permissive policy can construct
// the AllowList directly with their own pattern list.
func NewAllowList() *AllowList {
	return &AllowList{
		patterns:   defaultAllowedPatterns(),
		remembered: make(map[string]bool),
	}
}

// Decide returns the policy decision for the given command. The command
// should be the raw shell command string (e.g. "go build ./..."). It
// is matched against the allow-list patterns by prefix.
//
// Whitespace is trimmed and collapsed before matching, so "go  build"
// (extra space) is treated the same as "go build".
func (al *AllowList) Decide(command string) Decision {
	normalized := normalizeCommand(command)
	if normalized == "" {
		// Empty command — let the caller's existing validation handle it.
		// We say Confirm so it doesn't slip through silently.
		return DecisionConfirm
	}

	al.mu.RLock()
	defer al.mu.RUnlock()

	// Built-in patterns
	for _, pattern := range al.patterns {
		if matchesPrefix(normalized, pattern) {
			return DecisionAllow
		}
	}

	// User-remembered approvals
	for pattern := range al.remembered {
		if matchesPrefix(normalized, pattern) {
			return DecisionAllow
		}
	}

	return DecisionConfirm
}

// RememberApproval records that the user approved a specific command.
// Future commands matching the same pattern (the first two tokens of
// the command, in practice) will auto-approve for the rest of the
// session.
//
// The pattern derived from a command is the first 1-2 whitespace-
// separated tokens. For example:
//   - "npm install foo bar"      → remembered as "npm install"
//   - "make docker-build"        → remembered as "make docker-build"
//   - "./scripts/deploy.sh prod" → remembered as "./scripts/deploy.sh"
//
// This balances "remember enough to be useful" against "don't approve
// arbitrary commands just because one shaped like it was approved."
func (al *AllowList) RememberApproval(command string) {
	pattern := derivePattern(command)
	if pattern == "" {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	al.remembered[pattern] = true
}

// Remembered returns a snapshot of user-approved patterns. Useful for
// debugging and tests. Don't mutate the returned map.
func (al *AllowList) Remembered() map[string]bool {
	al.mu.RLock()
	defer al.mu.RUnlock()
	out := make(map[string]bool, len(al.remembered))
	for k, v := range al.remembered {
		out[k] = v
	}
	return out
}

// AllowedPatterns returns the built-in pattern list (without remembered
// approvals). Useful for debugging and inspection.
func (al *AllowList) AllowedPatterns() []string {
	al.mu.RLock()
	defer al.mu.RUnlock()
	out := make([]string, len(al.patterns))
	copy(out, al.patterns)
	return out
}

// defaultAllowedPatterns is the v2 standard policy. Documented here
// because changes to this list directly affect what runs without user
// approval — i.e., the trust contract.
//
// Conservative principles:
//   - Read-only operations: always allow
//   - Project-scoped writes (within worktree): allow
//   - Build/test/verify: allow (these don't have lasting external
//     effects)
//   - Codegen tools that the project's go.mod declares: allow
//   - Tool installation to GOPATH/bin: allow (consistent with v1
//     behavior; agents may legitimately need to install tools mid-task)
//   - Git operations within the worktree: allow (committing to the
//     agent's own branch is how agents finalize work)
//
// Things deliberately NOT in this list:
//   - "go run" — runs the user's program; could open ports, write
//     to stdout, have arbitrary side effects
//   - "rm" / "mv" — too easy for the LLM to write something destructive
//   - "curl" / "wget" — network access; might exfiltrate data or pull
//     untrusted code
//   - "git push" / "git checkout main" — modifies main branch or
//     remote state
//   - "npm" / "pip" / "cargo" — package managers can run arbitrary
//     install scripts
//   - "docker" — could start long-running containers, modify Docker
//     state outside the project
//   - "sudo" — should never auto-approve privilege escalation
//
// To add to the allow-list: justify it against these principles and
// add a comment explaining why.
func defaultAllowedPatterns() []string {
	return []string{
		// File writes: heredoc and direct redirect patterns. The LLM
		// uses these to author files. The user reviews via the
		// integrator's merge approval — that's the trust gate, not
		// individual file writes inside a worktree.
		"cat >",      // cat > file << EOF (heredoc) and cat > file (overwrite)
		"cat >>",     // cat >> file (append)
		"echo >",     // less common but used
		"echo >>",    // append via echo
		"mkdir -p",   // create directories
		"touch",      // create empty files

		// Go toolchain: build, test, vet, install, formatting,
		// dependency management. These are read-only or project-scoped.
		"go build",
		"go test",
		"go vet",
		"go fmt",
		"go install",
		"go get",
		"go mod tidy",
		"go mod download",
		"go list",
		"gofmt",
		"goimports",

		// Codegen tools — declared in the project's go.mod, run
		// deterministically, output is reviewable in the worktree.
		"buf generate",
		"buf lint",
		"buf format",
		"sqlc generate",
		"sqlc compile",
		"wire", // wire takes a package path, e.g. "wire ./app/..."
		"go generate",
		"mockgen",
		"stringer",

		// Git: worktree-scoped operations. Note: "git push" and
		// destructive operations are NOT here.
		"git status",
		"git diff",
		"git log",
		"git show",
		"git branch",
		"git add",
		"git commit",
		"git rm",          // git-tracked rm only; safer than raw rm
		"git mv",
		"git stash",
		"git fetch",       // read-only
		"git pull",        // can modify but only with the user's tracked remote
		"git config",      // local config; harmless
		"git rev-parse",
		"git ls-files",
		"git ls-tree",

		// Read-only inspection of the project
		"ls",
		"pwd",
		"cat",
		"head",
		"tail",
		"grep",
		"find",
		"wc",
		"file",
		"which",
	}
}

// normalizeCommand collapses whitespace and trims leading/trailing
// space. "go  build  ./..." → "go build ./...".
func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// matchesPrefix returns true if `cmd` starts with `pattern` followed by
// either end-of-string or whitespace. This prevents "gobuild" from
// matching pattern "go build" — there must be a word boundary.
//
// We DO want "go build" to match "go build ./..." (pattern is a prefix
// followed by space), and we want "ls" to match "ls -la" (same).
// Also: we want "git status" to match "git status --short", but NOT
// "git statusquo" (no space).
func matchesPrefix(cmd, pattern string) bool {
	if cmd == pattern {
		return true
	}
	if !strings.HasPrefix(cmd, pattern) {
		return false
	}
	// Character right after pattern must be whitespace (or end).
	rest := cmd[len(pattern):]
	if len(rest) == 0 {
		return true
	}
	return rest[0] == ' ' || rest[0] == '\t'
}

// derivePattern extracts a pattern from a command for use as a
// remembered approval. The pattern is what future commands will be
// matched against — it determines how broadly the user's approval
// generalizes.
//
// Three cases:
//
//   1. Path-like first token (./script.sh, /usr/bin/foo): remember just
//      the path. "./deploy.sh staging" → "./deploy.sh". This lets the
//      same script run with different arguments after one approval.
//
//   2. Tool with subcommand (npm install, git status, make build):
//      remember "tool subcommand". "npm install foo" → "npm install".
//      Different subcommands of the same tool require fresh approval.
//
//   3. Tool with flag-only args (rm -rf foo, kill -9 1234): remember
//      the FULL command. We don't generalize tools that don't have
//      a subcommand structure — letting "rm -rf foo" auto-approve any
//      future "rm" call would be a footgun. The user has to approve
//      each distinct rm explicitly.
func derivePattern(command string) string {
	tokens := strings.Fields(command)
	if len(tokens) == 0 {
		return ""
	}

	// Case 1: path-like first token
	if isPathLike(tokens[0]) {
		return tokens[0]
	}

	// Case 2: tool + subcommand
	if len(tokens) >= 2 && !looksLikeArg(tokens[1]) {
		return tokens[0] + " " + tokens[1]
	}

	// Case 3: bare tool with flag args — remember the full command so
	// future calls don't accidentally inherit this approval.
	return strings.Join(tokens, " ")
}

// isPathLike returns true if the token looks like a filesystem path
// rather than a tool name. Heuristic: starts with "./", "../", or "/".
func isPathLike(s string) bool {
	return strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/")
}

// looksLikeArg returns true if a token looks like a flag/argument
// rather than a subcommand. Used so that we don't remember "rm" from
// "rm -rf foo" as just "rm" (which would be too broad).
//
// Heuristic: starts with '-' (flag), or starts with '/' or contains '/'
// or '.' in a way that suggests a path.
func looksLikeArg(s string) bool {
	if s == "" {
		return true
	}
	if s[0] == '-' {
		return true
	}
	if isPathLike(s) {
		return true
	}
	// A token containing '.' or '/' that's NOT path-like is still
	// arg-shaped (filenames, version specs like @v1.2.3, etc.)
	if strings.ContainsAny(s, "/.") {
		return true
	}
	return false
}
