// Package toolagent implements cross-cutting analysis agents that react to
// blackboard activity rather than to filesystem events. The canonical
// example is golangci-lint: it runs over the whole module after package
// builds settle, and posts per-package LintFacts.
//
// Shape:
//
//   PackageFact stream → AggregatedSource(settleFor) → run tool → scatter
//   results into per-package ToolFacts on the lint blackboard
//
// Design seams (function-typed, no interfaces):
//
//   Runner   — invokes the actual tool (golangci-lint, staticcheck, ...)
//   Scatter  — splits one tool output into per-package ToolFacts
//
// Both are function values, so different tools plug in by supplying
// different Runner+Scatter pairs. The agent's structure is identical.
package toolagent

import (
	"context"
	"fmt"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/sources"
	"github.com/vinodhalaharvi/coven/worker"
)

// Issue is one finding emitted by a tool.
type Issue struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
	Rule    string `json:"rule"` // e.g. "errcheck", "ineffassign"
}

// ToolFact is the per-package result of one tool run. Multiple tools may
// post facts for the same package; they are distinguished by Tool.
type ToolFact struct {
	Pkg        worker.PackageID `json:"pkg"`
	Tool       string           `json:"tool"`
	OK         bool             `json:"ok"` // true if no issues + no error
	Issues     []Issue          `json:"issues"`
	Output     string           `json:"output,omitempty"` // raw tool output for debugging
	Duration   time.Duration    `json:"duration"`
	ObservedAt time.Time        `json:"observed_at"`
}

// Trigger is the synthetic event delivered to the agent's Handle. It
// carries the batch of upstream PackageFacts that woke this run.
type Trigger struct {
	Triggered []worker.PackageID
	At        time.Time
}

// Runner runs the actual tool once. It receives the working directory
// (typically the module root) and returns the raw output, the parsed
// per-issue list, and an error if the invocation itself failed.
//
// Note: a tool reporting issues is NOT an invocation error — that's a
// healthy run with findings. Errors here mean "the tool didn't run".
type Runner func(ctx context.Context, moduleRoot string) (output string, issues []Issue, err error)

// Scatter partitions issues into per-package buckets. The agent uses this
// to construct one ToolFact per affected package. Packages with zero
// issues from this run still get a clean ToolFact (so consumers can tell
// "this package was checked and is clean" vs "we don't know").
//
// allPackages is the set of packages currently on the package blackboard;
// the scatter MUST include a ToolFact for every package in this list.
type Scatter func(allPackages []worker.PackageID, issues []Issue) map[worker.PackageID][]Issue

// Config configures a ToolAgent.
type Config struct {
	AgentID    string
	ToolName   string                              // e.g. "golangci-lint"
	ModuleRoot string                              // where to run the tool
	PkgBoard   *blackboard.Board[worker.PackageFact] // upstream
	ToolBoard  *blackboard.Board[ToolFact]           // where ToolFacts go
	Runner     Runner                              // injected; required
	Scatter    Scatter                             // injected; required
	SettleFor  time.Duration                       // wait this long after last upstream change before running; default 1s
}

// BuildReactiveWorker assembles a supervisor.ReactiveWorker for this agent.
// The source is: subscribe to PackageFact changes → aggregate (settle) →
// produce one Trigger per settled batch.
func BuildReactiveWorker(cfg Config) supervisor.ReactiveWorker[Trigger, []ToolFact] {
	if cfg.SettleFor <= 0 {
		cfg.SettleFor = 1 * time.Second
	}
	a := &agent{cfg: cfg}

	// Project PackageFact subscription events to PackageIDs.
	pkgIDStream := sources.BlackboardSource(
		cfg.PkgBoard, "pkg:*",
		func(f blackboard.Fact[worker.PackageFact]) (worker.PackageID, bool) {
			return f.Value.Pkg, true
		}, 32,
	)

	// Aggregate IDs into a Trigger after the upstream settles.
	triggered := sources.AggregatedSource(
		pkgIDStream,
		cfg.SettleFor,
		func(ids []worker.PackageID) Trigger {
			// De-dup IDs while preserving order.
			seen := make(map[worker.PackageID]bool, len(ids))
			out := make([]worker.PackageID, 0, len(ids))
			for _, id := range ids {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
			return Trigger{Triggered: out, At: time.Now()}
		},
	)

	return supervisor.ReactiveWorker[Trigger, []ToolFact]{
		ID:     cfg.AgentID,
		Source: func(ctx context.Context) (<-chan Trigger, error) { return triggered(ctx) },
		Handle: a.handle,
		Report: a.report,
	}
}

type agent struct {
	cfg Config
}

// handle runs the tool, scatters results, posts per-package ToolFacts.
func (a *agent) handle(t Trigger) freeap.Program[[]ToolFact] {
	return freeap.Lift(freeap.Op[[]ToolFact]{
		Name: "run-tool:" + a.cfg.ToolName,
		Kind: freeap.KindIO,
		Run: func(ctx context.Context, w freeap.World) ([]ToolFact, error) {
			start := time.Now()
			output, issues, err := a.cfg.Runner(ctx, a.cfg.ModuleRoot)
			duration := time.Since(start)

			// Snapshot all packages from the upstream board so the scatter
			// can produce a fact per known package.
			allFacts := a.cfg.PkgBoard.List("pkg:*")
			allPkgs := make([]worker.PackageID, 0, len(allFacts))
			for _, f := range allFacts {
				allPkgs = append(allPkgs, f.Value.Pkg)
			}

			if err != nil {
				// Invocation error — post an unhealthy fact for every package
				// so consumers know the lint state is unknown.
				facts := make([]ToolFact, 0, len(allPkgs))
				for _, p := range allPkgs {
					f := ToolFact{
						Pkg: p, Tool: a.cfg.ToolName, OK: false,
						Output: output, Duration: duration, ObservedAt: time.Now(),
					}
					a.cfg.ToolBoard.Post(toolFactKey(a.cfg.ToolName, p), f, a.cfg.AgentID)
					facts = append(facts, f)
				}
				return facts, nil // do NOT bubble: invocation failure is a fact
			}

			scattered := a.cfg.Scatter(allPkgs, issues)
			facts := make([]ToolFact, 0, len(allPkgs))
			for _, p := range allPkgs {
				pkgIssues := scattered[p]
				f := ToolFact{
					Pkg:        p,
					Tool:       a.cfg.ToolName,
					OK:         len(pkgIssues) == 0,
					Issues:     pkgIssues,
					Duration:   duration,
					ObservedAt: time.Now(),
				}
				a.cfg.ToolBoard.Post(toolFactKey(a.cfg.ToolName, p), f, a.cfg.AgentID)
				facts = append(facts, f)
			}
			return facts, nil
		},
	})
}

func (a *agent) report(facts []ToolFact, err error) supervisor.Report {
	r := supervisor.Report{WorkerID: a.cfg.AgentID, At: time.Now()}
	if err != nil {
		r.OK = false
		r.Detail = "error: " + err.Error()
		return r
	}
	clean, dirty := 0, 0
	for _, f := range facts {
		if f.OK {
			clean++
		} else {
			dirty++
		}
	}
	r.OK = dirty == 0
	r.Detail = fmt.Sprintf("%s: %d clean, %d with issues", a.cfg.ToolName, clean, dirty)
	return r
}

// toolFactKey returns the blackboard key for a tool fact.
func toolFactKey(tool string, pkg worker.PackageID) string {
	return "tool:" + tool + ":" + string(pkg)
}

// Key is exported so other packages (e.g. the demo) can query the board
// for a specific tool/package combination.
func Key(tool string, pkg worker.PackageID) string { return toolFactKey(tool, pkg) }
