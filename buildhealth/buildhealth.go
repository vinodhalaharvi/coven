// Package buildhealth runs `go build ./...` at the module root and posts
// per-package BuildHealthFacts. It owns the central fsnotify watcher
// (via fsmonitor) — every other reactive agent in the system subscribes
// to FileChangeFacts on the shared blackboard rather than running its
// own watcher.
//
// Why the build is at module-level, not per-package:
//
// Per-package `go build` answers a different question than `go build
// ./...`. A package can compile in isolation and still be incompatible
// with downstream consumers — generated code from sqlc, wire, or proto
// can change a struct's shape without changing its package's own files,
// and per-package builds miss this. The module-level build is the only
// authoritative cross-cutting truth, and the Go compiler is the only
// honest reconciler of cross-tool drift.
//
// The agent's pipeline:
//   FileChangeFact stream → AggregatedSource(settle window)
//     → run `go build ./...` once at module root
//     → parse output → scatter per-package errors
//     → post BuildHealthFact per known package
package buildhealth

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/sources"
	"github.com/vinodhalaharvi/coven/worker"
)

// PackageID names a Go package by its module-relative path (e.g.
// "internal/auth", "gen/user/v1"). Mirrors worker.PackageID semantically.
type PackageID = worker.PackageID

// BuildError is one parsed error from `go build` output.
type BuildError struct {
	File    string `json:"file"`              // absolute path
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
}

// BuildHealthFact is the per-package fact posted after a module build.
type BuildHealthFact struct {
	Pkg        PackageID     `json:"pkg"`
	OK         bool          `json:"ok"`         // true if no errors attributed to this package
	Errors     []BuildError  `json:"errors"`
	Output     string        `json:"output,omitempty"` // raw output (for the "module-wide" fact)
	Duration   time.Duration `json:"duration"`
	ObservedAt time.Time     `json:"observed_at"`
}

// Trigger is the agent's wake-up event. Carries the burst of upstream
// FileChangeFacts that woke this run (for diagnostics; not used by the
// runner itself).
type Trigger struct {
	Bursts int       // how many FileChangeFacts collapsed into this trigger
	At     time.Time
}

// Config configures a buildhealth.Agent.
type Config struct {
	AgentID     string
	ModuleRoot  string                                // where `go build ./...` runs
	GoBin       string                                // override "go"; default "go"
	Packages    map[PackageID]string                  // PackageID → absolute dir
	FSBoard     *blackboard.Board[fsmonitor.FileChangeFact] // upstream
	BuildBoard  *blackboard.Board[BuildHealthFact]    // where BuildHealthFacts go
	SettleFor   time.Duration                         // wait for upstream to quiet; default 1500ms
}

// Key returns the blackboard key for a package's BuildHealthFact.
func Key(pkg PackageID) string { return "build:" + string(pkg) }

// ModuleKey is a special key for the module-wide fact (the raw output of
// the build, with no per-package attribution).
const ModuleKey = "build:__module__"

// BuildReactiveWorker assembles the supervisor.ReactiveWorker.
func BuildReactiveWorker(cfg Config) supervisor.ReactiveWorker[Trigger, []BuildHealthFact] {
	if cfg.GoBin == "" {
		cfg.GoBin = "go"
	}
	if cfg.SettleFor <= 0 {
		cfg.SettleFor = 1500 * time.Millisecond
	}
	a := &agent{cfg: cfg}

	// Project FileChangeFact subscription events to a count of bursts;
	// then aggregate into a Trigger after the upstream settles.
	fsStream := sources.BlackboardSource(
		cfg.FSBoard, "*",
		func(f blackboard.Fact[fsmonitor.FileChangeFact]) (int, bool) {
			if len(f.Value.ChangedFiles) == 0 {
				return 0, false
			}
			return 1, true
		}, 32,
	)
	triggered := sources.AggregatedSource(
		fsStream, cfg.SettleFor,
		func(counts []int) Trigger {
			n := 0
			for _, c := range counts {
				n += c
			}
			return Trigger{Bursts: n, At: time.Now()}
		},
	)

	return supervisor.ReactiveWorker[Trigger, []BuildHealthFact]{
		ID:     cfg.AgentID,
		Source: func(ctx context.Context) (<-chan Trigger, error) { return triggered(ctx) },
		Handle: a.handle,
		Report: a.report,
	}
}

type agent struct {
	cfg Config
}

func (a *agent) handle(t Trigger) freeap.Program[[]BuildHealthFact] {
	return freeap.Lift(freeap.Op[[]BuildHealthFact]{
		Name: "buildhealth.go-build",
		Kind: freeap.KindIO,
		Run: func(ctx context.Context, w freeap.World) ([]BuildHealthFact, error) {
			start := time.Now()
			goBin := a.cfg.GoBin
			if goBin == "" {
				goBin = "go"
			}
			cmd := exec.CommandContext(ctx, goBin, "build", "./...")
			cmd.Dir = a.cfg.ModuleRoot
			out, runErr := cmd.CombinedOutput()
			output := string(out)
			duration := time.Since(start)

			// Parse the output for typed errors.
			errs := parseGoBuildErrors(output)

			// Scatter errors to packages by file-path prefix matching.
			perPkgErrs := scatterByPackage(errs, a.cfg.Packages, a.cfg.ModuleRoot)

			// Post one BuildHealthFact per known package. A package with no
			// errors gets a healthy fact (so consumers can tell "checked and
			// clean" vs "we don't know"). Per-package OK is based only on
			// errors attributed to that package — not on the module-wide
			// runErr, which can be non-nil simply because some other package
			// has an error. The module-wide signal lives in the "__module__"
			// fact below.
			facts := make([]BuildHealthFact, 0, len(a.cfg.Packages)+1)
			now := time.Now()
			for pkg := range a.cfg.Packages {
				es := perPkgErrs[pkg]
				f := BuildHealthFact{
					Pkg:        pkg,
					OK:         len(es) == 0,
					Errors:     es,
					Duration:   duration,
					ObservedAt: now,
				}
				a.cfg.BuildBoard.Post(Key(pkg), f, a.cfg.AgentID)
				facts = append(facts, f)
			}
			// Module-wide fact captures errors that didn't attribute to a
			// known package (or the raw command failure).
			modFact := BuildHealthFact{
				Pkg:        "__module__",
				OK:         runErr == nil && len(errs) == 0,
				Errors:     unattributedErrors(errs, perPkgErrs),
				Output:     output,
				Duration:   duration,
				ObservedAt: now,
			}
			a.cfg.BuildBoard.Post(ModuleKey, modFact, a.cfg.AgentID)
			facts = append(facts, modFact)
			return facts, nil
		},
	})
}

func (a *agent) report(facts []BuildHealthFact, err error) supervisor.Report {
	r := supervisor.Report{WorkerID: a.cfg.AgentID, At: time.Now()}
	if err != nil {
		r.OK = false
		r.Detail = "error: " + err.Error()
		return r
	}
	clean, dirty := 0, 0
	for _, f := range facts {
		if f.Pkg == "__module__" {
			continue
		}
		if f.OK {
			clean++
		} else {
			dirty++
		}
	}
	r.OK = dirty == 0
	r.Detail = fmt.Sprintf("go build: %d clean, %d with errors", clean, dirty)
	return r
}

// parseGoBuildErrors extracts typed errors from `go build ./...` output.
//
// Lines look like:
//   ./auth/auth.go:10:5: undefined: foo
//   /abs/path/to/auth/auth.go:10:5: undefined: foo
//   pkg/foo/bar.go:42: syntax error
//
// Only ":" - separated forms with at least file:line:msg are matched.
// Lines that don't match the regex (e.g. summary lines) are ignored.
var goBuildErrorRe = regexp.MustCompile(`^(.*?\.go):(\d+)(?::(\d+))?:\s*(.*)$`)

func parseGoBuildErrors(output string) []BuildError {
	var errs []BuildError
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		m := goBuildErrorRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col := 0
		if m[3] != "" {
			col, _ = strconv.Atoi(m[3])
		}
		errs = append(errs, BuildError{
			File:    m[1],
			Line:    ln,
			Col:     col,
			Message: strings.TrimSpace(m[4]),
		})
	}
	return errs
}

// scatterByPackage assigns each BuildError to the package whose dir is
// the longest prefix of the error's file path. Errors that match no
// package go into the unattributed bucket (returned via
// unattributedErrors below).
//
// moduleRoot is the directory `go build` ran from; relative paths in
// error output are resolved against it.
func scatterByPackage(errs []BuildError, pkgDirs map[PackageID]string, moduleRoot string) map[PackageID][]BuildError {
	out := make(map[PackageID][]BuildError)
	// Sort packages by descending directory length for longest-prefix match.
	pkgs := make([]PackageID, 0, len(pkgDirs))
	for p := range pkgDirs {
		pkgs = append(pkgs, p)
	}
	for i := 1; i < len(pkgs); i++ {
		for j := i; j > 0 && len(pkgDirs[pkgs[j]]) > len(pkgDirs[pkgs[j-1]]); j-- {
			pkgs[j], pkgs[j-1] = pkgs[j-1], pkgs[j]
		}
	}
	for _, e := range errs {
		// Resolve the error's file path. If it's already absolute, use as-is.
		// If relative, resolve against the moduleRoot (where go build ran).
		abs := e.File
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(moduleRoot, abs)
		}
		if r, err := filepath.Abs(abs); err == nil {
			abs = r
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		}
		matched := false
		for _, p := range pkgs {
			d := pkgDirs[p]
			if d == "" {
				continue
			}
			absDir, err := filepath.Abs(d)
			if err != nil {
				absDir = d
			}
			if r, err := filepath.EvalSymlinks(absDir); err == nil {
				absDir = r
			}
			if abs == absDir || strings.HasPrefix(abs, absDir+string(filepath.Separator)) {
				out[p] = append(out[p], e)
				matched = true
				break
			}
		}
		_ = matched
	}
	return out
}

// unattributedErrors returns errors that didn't get assigned to any
// package by the scatter.
func unattributedErrors(all []BuildError, attributed map[PackageID][]BuildError) []BuildError {
	matched := make(map[string]bool)
	for _, errs := range attributed {
		for _, e := range errs {
			matched[errKey(e)] = true
		}
	}
	var out []BuildError
	for _, e := range all {
		if !matched[errKey(e)] {
			out = append(out, e)
		}
	}
	return out
}

func errKey(e BuildError) string {
	return fmt.Sprintf("%s:%d:%d", e.File, e.Line, e.Col)
}
