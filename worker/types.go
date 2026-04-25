// Package worker implements the per-package agent. Each agent watches its
// own directory via fsnotify, reacts to file changes, parses the Go source,
// runs go build + go vet, and posts a PackageFact to the blackboard.
package worker

import "time"

// PackageID uniquely identifies a package (import path or directory).
type PackageID string

// Symbol describes an exported declaration.
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "func", "type", "const", "var"
	Signature string `json:"signature"`
}

// BuildResult captures the outcome of running go build on the package.
type BuildResult struct {
	OK       bool          `json:"ok"`
	Output   string        `json:"output,omitempty"`
	Duration time.Duration `json:"duration"`
}

// VetResult captures the outcome of running go vet on the package.
type VetResult struct {
	Clean  bool   `json:"clean"`
	Output string `json:"output,omitempty"`
}

// LLMReview captures the output of an LLM code-review pass on the package.
// It's optional — populated only when an LLM is configured and reachable.
type LLMReview struct {
	Summary string   `json:"summary"`
	Issues  []string `json:"issues"`
	Quality float64  `json:"quality"` // 0..1
}

// PackageFact is the full snapshot a package agent produces and posts to
// the blackboard after reacting to a change.
type PackageFact struct {
	Pkg        PackageID   `json:"pkg"`
	Dir        string      `json:"dir"`
	Checksum   string      `json:"checksum"`
	Imports    []PackageID `json:"imports"` // import paths this package depends on
	Exports    []Symbol    `json:"exports"`
	Build      BuildResult `json:"build"`
	Vet        VetResult   `json:"vet"`
	Review     *LLMReview  `json:"review,omitempty"`
	Author     string      `json:"author"`
	ObservedAt time.Time   `json:"observed_at"`
}

// Healthy returns true when build and vet both passed.
func (f PackageFact) Healthy() bool { return f.Build.OK && f.Vet.Clean }
