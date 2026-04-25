package toolagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vinodhalaharvi/coven/worker"
)

// GolangciLintRunner runs `golangci-lint run --output-format=json ./...` in
// moduleRoot. It returns issues parsed from the JSON output. A non-zero
// exit code with valid JSON output is NOT an invocation error — that's the
// expected case when issues are found.
func GolangciLintRunner(bin string) Runner {
	if bin == "" {
		bin = "golangci-lint"
	}
	return func(ctx context.Context, moduleRoot string) (string, []Issue, error) {
		cmd := exec.CommandContext(ctx, bin, "run", "--output-format=json", "./...")
		cmd.Dir = moduleRoot
		out, runErr := cmd.CombinedOutput()
		output := string(out)

		// golangci-lint emits JSON on stdout even when issues are found;
		// only treat it as a runner error if we can't parse the output.
		var parsed struct {
			Issues []struct {
				FromLinter string `json:"FromLinter"`
				Text       string `json:"Text"`
				Pos        struct {
					Filename string `json:"Filename"`
					Line     int    `json:"Line"`
					Column   int    `json:"Column"`
				} `json:"Pos"`
			} `json:"Issues"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			// Couldn't parse — combine errors for context.
			if runErr != nil {
				return output, nil, fmt.Errorf("golangci-lint failed: %w; output: %s", runErr, truncate(output, 200))
			}
			return output, nil, fmt.Errorf("parse golangci-lint output: %w", err)
		}

		issues := make([]Issue, 0, len(parsed.Issues))
		for _, p := range parsed.Issues {
			issues = append(issues, Issue{
				File: p.Pos.Filename, Line: p.Pos.Line, Col: p.Pos.Column,
				Message: p.Text, Rule: p.FromLinter,
			})
		}
		return output, issues, nil
	}
}

// PathPrefixScatter returns a Scatter that assigns each issue to a package
// by checking which package's directory is a prefix of the issue's file
// path. Requires a map from package ID to directory.
//
// This is the simplest correct strategy. More sophisticated strategies
// (e.g., loading the build graph) are plug-in replacements.
func PathPrefixScatter(pkgDirs map[worker.PackageID]string) Scatter {
	return func(allPkgs []worker.PackageID, issues []Issue) map[worker.PackageID][]Issue {
		out := make(map[worker.PackageID][]Issue, len(allPkgs))
		for _, p := range allPkgs {
			out[p] = nil
		}
		// Sort packages by directory length descending so we match the most
		// specific dir first (e.g. "auth/v2" wins over "auth").
		ordered := make([]worker.PackageID, 0, len(pkgDirs))
		for p := range pkgDirs {
			ordered = append(ordered, p)
		}
		// simple insertion sort by descending dir length
		for i := 1; i < len(ordered); i++ {
			for j := i; j > 0 && len(pkgDirs[ordered[j]]) > len(pkgDirs[ordered[j-1]]); j-- {
				ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
			}
		}

		for _, iss := range issues {
			abs, err := filepath.Abs(iss.File)
			if err != nil {
				abs = iss.File
			}
			for _, p := range ordered {
				dir := pkgDirs[p]
				if dir == "" {
					continue
				}
				absDir, err := filepath.Abs(dir)
				if err != nil {
					absDir = dir
				}
				if strings.HasPrefix(abs, absDir+string(filepath.Separator)) || abs == absDir {
					out[p] = append(out[p], iss)
					break
				}
			}
		}
		return out
	}
}

// FakeLinter returns a Runner that yields the given issues (no real exec).
// Useful in tests.
func FakeLinter(issues []Issue, output string) Runner {
	return func(ctx context.Context, moduleRoot string) (string, []Issue, error) {
		return output, issues, nil
	}
}

// FailingLinter returns a Runner that always errors out at invocation.
func FailingLinter(msg string) Runner {
	return func(ctx context.Context, moduleRoot string) (string, []Issue, error) {
		return "", nil, errors.New(msg)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
