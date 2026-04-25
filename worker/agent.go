package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/ownership"
	"github.com/vinodhalaharvi/coven/sources"
)

// PackageAgentConfig configures one agent instance.
type PackageAgentConfig struct {
	Pkg         PackageID
	Dir         string
	Board       *blackboard.Board[PackageFact]
	LLM         llm.LLM       // optional; nil disables LLM review
	ReviewEvery time.Duration // throttle LLM reviews; 0 = never
	Ownership   ownership.Registry // optional; if set, fsnotify ignores foreign-owned files
	ExtraSource sources.Source[FSEvent] // optional: an extra event source merged with fsnotify (e.g. blackboard-driven)
}

// ReactiveWorker assembles the supervisor.ReactiveWorker for a package. The
// per-event program is:
//   Checksum (compute) → (ExtractAll || Build || Vet) → Review (llm, optional) → Post (io)
// The middle three run in parallel via Ap; Review runs after they complete
// (FlatMap boundary) because it depends on their outputs.
func BuildReactiveWorker(cfg PackageAgentConfig, debounce time.Duration) supervisor.ReactiveWorker[FSEvent, PackageFact] {
	agent := &agent{cfg: cfg, lastReview: time.Time{}}

	// Build the source: fsnotify on the package dir, optionally with an
	// ownership-based ignore hook, optionally merged with an extra source.
	fsCfg := FSConfig{
		Dir:        cfg.Dir,
		Debounce:   debounce,
		Extensions: []string{".go"},
	}
	if cfg.Ownership != nil {
		fsCfg.IgnoreFile = ownership.IgnoreForeignFiles(cfg.Ownership, ownership.AgentID(cfg.Pkg))
	}
	source := sources.Source[FSEvent](FSSource(fsCfg))
	if cfg.ExtraSource != nil {
		source = sources.MergeSources(source, cfg.ExtraSource)
	}

	return supervisor.ReactiveWorker[FSEvent, PackageFact]{
		ID:     string(cfg.Pkg),
		Source: source,
		Handle: agent.handle,
		Report: agent.report,
	}
}

// agent carries instance state (last review time) across event handlings.
type agent struct {
	cfg        PackageAgentConfig
	lastReview time.Time
}

func (a *agent) handle(_ FSEvent) freeap.Program[PackageFact] {
	// Stage 1 — checksum (cheap, deterministic).
	checksum := freeap.Lift(freeap.Op[string]{
		Name: "checksum",
		Kind: freeap.KindCompute,
		Run: func(ctx context.Context, w freeap.World) (string, error) {
			return ChecksumDir(a.cfg.Dir)
		},
	})

	// Stage 2 — three independent analyses. Composed with Ap so they run in
	// parallel. The combining function assembles a partial PackageFact from
	// them (the checksum is threaded in after the ap via FlatMap).
	astAnalysis := freeap.Lift(freeap.Op[astResult]{
		Name: "extract-ast",
		Kind: freeap.KindCompute,
		Run: func(ctx context.Context, w freeap.World) (astResult, error) {
			syms, imps, err := ExtractAll(a.cfg.Dir)
			return astResult{exports: syms, imports: imps}, err
		},
	})
	build := freeap.Lift(freeap.Op[BuildResult]{
		Name: "go-build",
		Kind: freeap.KindIO,
		Run: func(ctx context.Context, w freeap.World) (BuildResult, error) {
			return RunGoBuild(ctx, a.cfg.Dir), nil
		},
	})
	vet := freeap.Lift(freeap.Op[VetResult]{
		Name: "go-vet",
		Kind: freeap.KindIO,
		Run: func(ctx context.Context, w freeap.World) (VetResult, error) {
			return RunGoVet(ctx, a.cfg.Dir), nil
		},
	})

	// Apply combinator: combine(ast)(build)(vet) -> partial.
	combine := func(ar astResult) func(BuildResult) func(VetResult) partial {
		return func(b BuildResult) func(VetResult) partial {
			return func(v VetResult) partial {
				return partial{exports: ar.exports, imports: ar.imports, build: b, vet: v}
			}
		}
	}
	step2 := freeap.Ap(
		freeap.Ap(freeap.Map(astAnalysis, combine), build),
		vet,
	)

	// Stage 3 — thread checksum through, optionally do LLM review, build fact.
	return freeap.FlatMap(checksum, func(sum string) freeap.Program[PackageFact] {
		return freeap.FlatMap(step2, func(pp partial) freeap.Program[PackageFact] {
			fact := PackageFact{
				Pkg:        a.cfg.Pkg,
				Dir:        a.cfg.Dir,
				Checksum:   sum,
				Imports:    pp.imports,
				Exports:    pp.exports,
				Build:      pp.build,
				Vet:        pp.vet,
				Author:     string(a.cfg.Pkg),
				ObservedAt: time.Now(),
			}
			// If LLM configured and throttle has elapsed, review the package.
			if a.cfg.LLM != nil && a.cfg.ReviewEvery > 0 &&
				time.Since(a.lastReview) >= a.cfg.ReviewEvery {
				return freeap.FlatMap(a.reviewProgram(fact), func(r LLMReview) freeap.Program[PackageFact] {
					fact.Review = &r
					a.lastReview = time.Now()
					return a.postProgram(fact)
				})
			}
			return a.postProgram(fact)
		})
	})
}

type astResult struct {
	exports []Symbol
	imports []PackageID
}

type partial struct {
	exports []Symbol
	imports []PackageID
	build   BuildResult
	vet     VetResult
}

func (a *agent) reviewProgram(f PackageFact) freeap.Program[LLMReview] {
	return freeap.Lift(freeap.Op[LLMReview]{
		Name: "llm-review",
		Kind: freeap.KindLLM,
		Run: func(ctx context.Context, w freeap.World) (LLMReview, error) {
			parse := llm.Structured[LLMReview](
				a.cfg.LLM,
				`{summary: string, issues: [string], quality: number}`,
			)
			prompt := fmt.Sprintf(
				"Review Go package %s. Exports: %v. Build OK: %v. Vet clean: %v.\n"+
					"Return a concise summary, a list of issues (may be empty), and a quality score in [0,1].",
				f.Pkg, symbolNames(f.Exports), f.Build.OK, f.Vet.Clean,
			)
			return parse(ctx, prompt)
		},
	})
}

func (a *agent) postProgram(f PackageFact) freeap.Program[PackageFact] {
	return freeap.Lift(freeap.Op[PackageFact]{
		Name: "post-fact",
		Kind: freeap.KindIO,
		Run: func(ctx context.Context, w freeap.World) (PackageFact, error) {
			a.cfg.Board.Post("pkg:"+string(a.cfg.Pkg), f, string(a.cfg.Pkg))
			return f, nil
		},
	})
}

func (a *agent) report(f PackageFact, err error) supervisor.Report {
	r := supervisor.Report{WorkerID: string(a.cfg.Pkg), At: time.Now()}
	if err != nil {
		r.OK = false
		r.Detail = "error: " + err.Error()
		return r
	}
	r.OK = f.Healthy()
	if r.OK {
		r.Detail = "healthy"
	} else {
		r.Detail = "build=" + boolStr(f.Build.OK) + " vet=" + boolStr(f.Vet.Clean)
	}
	return r
}

func symbolNames(syms []Symbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.Name
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "ok"
	}
	return "fail"
}
