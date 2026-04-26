package agent

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vinodhalaharvi/coven/llm"
)

// StandardTools returns the tools every conversational agent should
// have available unless there's a reason not to. moduleRoot scopes file
// access so agents can't escape the project.
//
// Pure tools (read_file, list_files, search_text) run without confirmation.
// The exec tool always requires confirmation via the agent's ConfirmFunc.
func StandardTools(moduleRoot string) []Tool {
	return []Tool{
		ReadFileTool(moduleRoot),
		ListFilesTool(moduleRoot),
		SearchTextTool(moduleRoot),
		ExecTool(moduleRoot),
	}
}

// ReadFileTool reads a file relative to moduleRoot and returns its content.
// Refuses paths that escape the module.
func ReadFileTool(moduleRoot string) Tool {
	return Tool{
		Pure: true,
		Spec: llm.ToolSpec{
			Name:        "read_file",
			Description: "Read a file's contents. Returns the file body as a string. Path is relative to the project root.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File path relative to project root.",
					},
				},
				"required": []string{"path"},
			},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			path, _ := input["path"].(string)
			if path == "" {
				return "", fmt.Errorf("path is required")
			}
			abs, err := safeJoin(moduleRoot, path)
			if err != nil {
				return "", err
			}
			body, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			// Cap the response at 64KB to avoid blowing up context.
			const maxRead = 64 * 1024
			if len(body) > maxRead {
				return string(body[:maxRead]) + fmt.Sprintf("\n... (truncated; %d bytes total)", len(body)), nil
			}
			return string(body), nil
		},
	}
}

// ListFilesTool lists files matching a glob pattern relative to moduleRoot.
func ListFilesTool(moduleRoot string) Tool {
	return Tool{
		Pure: true,
		Spec: llm.ToolSpec{
			Name:        "list_files",
			Description: "List files in the project matching a glob pattern (e.g. '**/*.proto', 'gen/**/*.go', '*.yaml'). Returns one path per line, relative to project root.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"glob": map[string]any{
						"type":        "string",
						"description": "Glob pattern, e.g. '*.proto', '**/*.go', 'gen/**'.",
					},
				},
				"required": []string{"glob"},
			},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			glob, _ := input["glob"].(string)
			if glob == "" {
				return "", fmt.Errorf("glob is required")
			}
			matches, err := walkGlob(moduleRoot, glob)
			if err != nil {
				return "", err
			}
			if len(matches) == 0 {
				return "(no matches)", nil
			}
			// Return relative paths.
			rel := make([]string, 0, len(matches))
			for _, m := range matches {
				r, err := filepath.Rel(moduleRoot, m)
				if err != nil {
					r = m
				}
				rel = append(rel, r)
			}
			return strings.Join(rel, "\n"), nil
		},
	}
}

// SearchTextTool greps for a regex across the project. Returns matches
// with file:line prefix.
func SearchTextTool(moduleRoot string) Tool {
	return Tool{
		Pure: true,
		Spec: llm.ToolSpec{
			Name:        "search_text",
			Description: "Search project files for a regex pattern. Returns matching lines as 'path:line: content'. Use to find usages of types, function signatures, etc.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Regex pattern (Go regexp syntax).",
					},
					"glob": map[string]any{
						"type":        "string",
						"description": "Optional file glob to limit the search (default: '**/*').",
					},
				},
				"required": []string{"pattern"},
			},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			pattern, _ := input["pattern"].(string)
			if pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return "", fmt.Errorf("bad regex: %w", err)
			}
			glob, _ := input["glob"].(string)
			if glob == "" {
				glob = "**/*"
			}
			files, err := walkGlob(moduleRoot, glob)
			if err != nil {
				return "", err
			}
			var matches []string
			const maxMatches = 100
			for _, f := range files {
				if len(matches) >= maxMatches {
					matches = append(matches, fmt.Sprintf("(truncated at %d matches)", maxMatches))
					break
				}
				rel, _ := filepath.Rel(moduleRoot, f)
				lineMatches, err := grepFile(f, re)
				if err != nil {
					continue
				}
				for _, lm := range lineMatches {
					matches = append(matches, fmt.Sprintf("%s:%d: %s", rel, lm.line, lm.text))
					if len(matches) >= maxMatches {
						break
					}
				}
			}
			if len(matches) == 0 {
				return "(no matches)", nil
			}
			return strings.Join(matches, "\n"), nil
		},
	}
}

// ExecTool runs a shell command from moduleRoot. Always mutating —
// always requires confirmation.
func ExecTool(moduleRoot string) Tool {
	return Tool{
		Pure: false,
		Spec: llm.ToolSpec{
			Name:        "exec",
			Description: "Run a shell command from the project root. Use this for tools like 'buf generate', 'go get X', 'wire ./pkg', etc. The user will be asked to confirm before execution.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to run.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Brief justification (one sentence) shown to the user when asking for confirmation.",
					},
				},
				"required": []string{"command"},
			},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			cmdline, _ := input["command"].(string)
			if cmdline == "" {
				return "", fmt.Errorf("command is required")
			}
			cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cctx, "sh", "-c", cmdline)
			cmd.Dir = moduleRoot
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
			out, err := cmd.CombinedOutput()
			output := string(out)
			if err != nil {
				return output, fmt.Errorf("exit error: %w", err)
			}
			if strings.TrimSpace(output) == "" {
				return "(command exited 0 with no output)", nil
			}
			return output, nil
		},
	}
}

// safeJoin joins a relative path against root and refuses paths that
// escape root via "..".
func safeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed: %s", rel)
	}
	abs := filepath.Join(root, rel)
	rootAbs, _ := filepath.Abs(root)
	if r, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = r
	}
	absResolved := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		absResolved = r
	}
	if !strings.HasPrefix(absResolved, rootAbs) && absResolved != rootAbs {
		return "", fmt.Errorf("path escapes project root: %s", rel)
	}
	return abs, nil
}

// walkGlob walks root and returns files matching the glob. Supports
// '**' for recursive directory matching.
func walkGlob(root, glob string) ([]string, error) {
	var results []string
	hasDoublestar := strings.Contains(glob, "**")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		matched := false
		if hasDoublestar {
			matched = doublestarMatch(glob, rel)
		} else {
			ok, _ := filepath.Match(glob, rel)
			matched = ok
			if !matched {
				ok2, _ := filepath.Match(glob, filepath.Base(rel))
				matched = ok2
			}
		}
		if matched {
			results = append(results, path)
		}
		return nil
	})
	return results, err
}

// doublestarMatch implements glob matching with '**' for any-depth dirs.
// '**' matches zero or more path components.
func doublestarMatch(pattern, path string) bool {
	pathParts := strings.Split(filepath.ToSlash(path), "/")
	patternParts := strings.Split(pattern, "/")
	return matchParts(patternParts, pathParts)
}

func matchParts(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		// '**' matches zero or more components: try each.
		for i := 0; i <= len(path); i++ {
			if matchParts(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, _ := filepath.Match(pattern[0], path[0])
	if !ok {
		return false
	}
	return matchParts(pattern[1:], path[1:])
}

type lineMatch struct {
	line int
	text string
}

// grepFile reads a file line by line and returns matches.
func grepFile(path string, re *regexp.Regexp) ([]lineMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []lineMatch
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for i := 1; scanner.Scan(); i++ {
		text := scanner.Text()
		if re.MatchString(text) {
			out = append(out, lineMatch{line: i, text: strings.TrimSpace(text)})
		}
		if len(out) >= 50 {
			break
		}
	}
	return out, nil
}
