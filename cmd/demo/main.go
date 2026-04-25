// Demo CLI: watches a Go module, optionally a proto root, optionally runs a
// linter via the tool-agent abstraction. Three agent kinds in one process.
//
// Usage examples:
//
//   demo -root .                           # package agents only
//   demo -root . -proto-root proto -gen-root gen
//   demo -root . -lint
//   demo -root . -llm haiku -review 30s    # with Claude Haiku review
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/buildhealth"
	"github.com/vinodhalaharvi/coven/codegen"
	"github.com/vinodhalaharvi/coven/ensemble"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/ownership"
	"github.com/vinodhalaharvi/coven/protogen"
	"github.com/vinodhalaharvi/coven/sources"
	"github.com/vinodhalaharvi/coven/sqlcagent"
	"github.com/vinodhalaharvi/coven/toolagent"
	"github.com/vinodhalaharvi/coven/wireagent"
	"github.com/vinodhalaharvi/coven/worker"
)

func main() {
	var (
		root        = flag.String("root", ".", "Go module root to watch")
		protoRoot   = flag.String("proto-root", "", "directory containing .proto files (optional)")
		genRoot     = flag.String("gen-root", "", "directory where generated code is written (required if -proto-root set)")
		bufBin      = flag.String("buf", "buf", "buf binary name/path")
		enableLint  = flag.Bool("lint", false, "enable golangci-lint tool agent")
		lintBin     = flag.String("lint-bin", "golangci-lint", "golangci-lint binary name/path")
		lintSettle  = flag.Duration("lint-settle", 2*time.Second, "wait this long after package activity quiets before linting")
		enableBuild = flag.Bool("build", false, "enable BuildHealthAgent (runs go build ./... at module root)")
		goBin       = flag.String("go-bin", "go", "go binary name/path")
		buildSettle = flag.Duration("build-settle", 1500*time.Millisecond, "wait this long after activity quiets before module build")
		enableWire  = flag.Bool("wire", false, "enable WireAgent on packages containing wire.go")
		wireBin     = flag.String("wire-bin", "wire", "wire binary name/path")
		enableSqlc  = flag.Bool("sqlc", false, "enable SqlcAgent if sqlc.yaml is present at module root")
		sqlcBin     = flag.String("sqlc-bin", "sqlc", "sqlc binary name/path")
		sqlcGenRoot = flag.String("sqlc-gen-root", "", "directory containing sqlc-generated code (required if -sqlc)")
		tick        = flag.Duration("tick", 500*time.Millisecond, "ensemble polling interval")
		debounce    = flag.Duration("debounce", 200*time.Millisecond, "fsnotify debounce per agent")
		llmModel    = flag.String("llm", "", "LLM model: haiku|sonnet|opus|static (optional)")
		reviewEvery = flag.Duration("review", 0, "minimum time between LLM reviews per package; 0 = never")
		verbose     = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	var logHandler slog.Handler
	if *verbose {
		logHandler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	} else {
		logHandler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	}
	log := slog.New(logHandler)

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Error("resolving root", "err", err)
		os.Exit(1)
	}
	// Resolve symlinks so that what we pass to the watchers matches what
	// fsnotify reports back. macOS in particular: /tmp is a symlink to
	// /private/tmp, and kqueue events arrive with the canonical path.
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	if *protoRoot != "" && *genRoot == "" {
		log.Error("-proto-root requires -gen-root")
		os.Exit(1)
	}

	pkgs, err := discoverPackages(absRoot)
	if err != nil {
		log.Error("walking root", "err", err)
		os.Exit(1)
	}
	if len(pkgs) == 0 && *protoRoot == "" {
		log.Error("no Go packages found and no -proto-root given; nothing to do", "root", absRoot)
		os.Exit(1)
	}
	if len(pkgs) == 0 {
		fmt.Printf("no Go packages found under %s yet (will need to be present at startup; restart coven once buf generate has produced output)\n", absRoot)
	} else {
		fmt.Printf("discovered %d package(s) under %s:\n", len(pkgs), absRoot)
		for _, p := range pkgs {
			fmt.Printf("  • %s  (%s)\n", p.importPath, p.dir)
		}
	}

	var l llm.LLM
	if *llmModel != "" {
		l = buildLLM(*llmModel)
		if l == nil {
			log.Error("unknown LLM model", "model", *llmModel)
			os.Exit(1)
		}
	}

	// Boards.
	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{
		QuietFor: 1 * time.Second, Rounds: 3,
	})
	var genBoard *blackboard.Board[protogen.ProtoGenFact]
	var toolBoard *blackboard.Board[toolagent.ToolFact]
	if *protoRoot != "" {
		genBoard = blackboard.New[protogen.ProtoGenFact](blackboard.Config{
			QuietFor: 1 * time.Second, Rounds: 3,
		})
	}
	if *enableLint {
		toolBoard = blackboard.New[toolagent.ToolFact](blackboard.Config{
			QuietFor: 1 * time.Second, Rounds: 3,
		})
	}

	reg := ownership.New()

	pkgSup := supervisor.New[worker.FSEvent, worker.PackageFact](supervisor.Config{
		Name: "pkg-sup", Logger: log,
	})

	pkgDirs := make(map[worker.PackageID]string, len(pkgs))
	for _, p := range pkgs {
		pkgDirs[worker.PackageID(p.importPath)] = p.dir
	}

	for _, p := range pkgs {
		pkgID := worker.PackageID(p.importPath)
		dir := p.dir
		var extra sources.Source[worker.FSEvent]
		if genBoard != nil {
			extra = sources.BlackboardSource(genBoard, "*",
				func(f blackboard.Fact[protogen.ProtoGenFact]) (worker.FSEvent, bool) {
					if !f.Value.OK || len(f.Value.ChangedFiles) == 0 {
						return worker.FSEvent{}, false
					}
					for _, gp := range f.Value.ChangedFiles {
						if filepath.Dir(gp) == dir {
							return worker.FSEvent{Dir: dir, ChangedAt: time.Now(), ChangedFiles: f.Value.ChangedFiles}, true
						}
					}
					return worker.FSEvent{}, false
				}, 16,
			)
		}
		pkgSup.Attach(worker.BuildReactiveWorker(worker.PackageAgentConfig{
			Pkg:         pkgID,
			Dir:         dir,
			Board:       pkgBoard,
			LLM:         l,
			ReviewEvery: *reviewEvery,
			Ownership:   reg,
			ExtraSource: extra,
		}, *debounce))
	}

	var genSup *supervisor.Supervisor[worker.FSEvent, protogen.ProtoGenFact]
	if genBoard != nil {
		absProto, _ := filepath.Abs(*protoRoot)
		if r, err := filepath.EvalSymlinks(absProto); err == nil {
			absProto = r
		}
		absGen, _ := filepath.Abs(*genRoot)
		if r, err := filepath.EvalSymlinks(absGen); err == nil {
			absGen = r
		}
		genSup = supervisor.New[worker.FSEvent, protogen.ProtoGenFact](supervisor.Config{
			Name: "gen-sup", Logger: log,
		})
		genSup.Attach(protogen.BuildReactiveWorker(protogen.Config{
			AgentID:   "protogen",
			ProtoRoot: absProto,
			GenRoot:   absGen,
			Board:     genBoard,
			Ownership: reg,
			// Run buf at the module root so it discovers buf.yaml /
			// buf.gen.yaml there. Outputs still go under absGen.
			Runner:   protogen.ExecRunnerAt(*bufBin, []string{"generate"}, absRoot, absGen),
			Debounce: *debounce,
		}))
		fmt.Printf("\nproto agent: watching %s, generating into %s (via %s, run from %s)\n",
			absProto, absGen, *bufBin, absRoot)
	}

	var lintSup *supervisor.Supervisor[toolagent.Trigger, []toolagent.ToolFact]
	if toolBoard != nil {
		lintSup = supervisor.New[toolagent.Trigger, []toolagent.ToolFact](supervisor.Config{
			Name: "lint-sup", Logger: log,
		})
		lintSup.Attach(toolagent.BuildReactiveWorker(toolagent.Config{
			AgentID:    "linter",
			ToolName:   "golangci-lint",
			ModuleRoot: absRoot,
			PkgBoard:   pkgBoard,
			ToolBoard:  toolBoard,
			Runner:     toolagent.GolangciLintRunner(*lintBin),
			Scatter:    toolagent.PathPrefixScatter(pkgDirs),
			SettleFor:  *lintSettle,
		}))
		fmt.Printf("lint agent: golangci-lint on settle=%s\n", *lintSettle)
	}

	// ─── Centralized fsnotify + new codegen agents ───────────────────────
	// fsBoard is the central FileChangeFact stream consumed by:
	//   - BuildHealthAgent (module-wide go build ./...)
	//   - WireAgent(s)     (per-package wire regeneration)
	//   - SqlcAgent        (sqlc generate when .sql files change)
	//
	// Every existing legacy agent (PackageAgent, ProtoGenAgent) keeps its
	// own fsnotify for now — those continue to work as before. The new
	// agents are additive.
	var fsBoard *blackboard.Board[fsmonitor.FileChangeFact]
	if *enableBuild || *enableWire || *enableSqlc {
		fsBoard = blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{
			QuietFor: 1 * time.Second, Rounds: 3,
		})
		// One central watcher for the entire module.
		go func() {
			err := fsmonitor.Run(context.Background(), fsmonitor.Config{
				Root:     absRoot,
				Debounce: *debounce,
				Board:    fsBoard,
			})
			if err != nil {
				log.Error("fsmonitor exited", "err", err)
			}
		}()
		fmt.Printf("\nfsmonitor: watching %s (central FileChangeFact stream)\n", absRoot)
	}

	// BuildHealthAgent — module-level go build ./...
	var buildSup *supervisor.Supervisor[buildhealth.Trigger, []buildhealth.BuildHealthFact]
	var buildBoard *blackboard.Board[buildhealth.BuildHealthFact]
	if *enableBuild {
		buildBoard = blackboard.New[buildhealth.BuildHealthFact](blackboard.Config{
			QuietFor: 1 * time.Second, Rounds: 3,
		})
		buildSup = supervisor.New[buildhealth.Trigger, []buildhealth.BuildHealthFact](supervisor.Config{
			Name: "build-sup", Logger: log,
		})
		buildSup.Attach(buildhealth.BuildReactiveWorker(buildhealth.Config{
			AgentID:    "buildhealth",
			ModuleRoot: absRoot,
			GoBin:      *goBin,
			Packages:   pkgDirs,
			FSBoard:    fsBoard,
			BuildBoard: buildBoard,
			SettleFor:  *buildSettle,
		}))
		fmt.Printf("build agent: go build ./... settle=%s\n", *buildSettle)
	}

	// WireAgent(s) — one per package containing wire.go.
	var wireSup *supervisor.Supervisor[codegen.Trigger, wireagent.Fact]
	var wireBoard *blackboard.Board[wireagent.Fact]
	if *enableWire {
		wirePkgs, _ := wireagent.DiscoverWirePackages(absRoot)
		if len(wirePkgs) == 0 {
			fmt.Printf("wire agent: no wire-tagged packages found under %s\n", absRoot)
		} else {
			wireBoard = blackboard.New[wireagent.Fact](blackboard.Config{
				QuietFor: 1 * time.Second, Rounds: 3,
			})
			wireSup = supervisor.New[codegen.Trigger, wireagent.Fact](supervisor.Config{
				Name: "wire-sup", Logger: log,
			})
			for _, pkg := range wirePkgs {
				wireSup.Attach(wireagent.BuildReactiveWorker(
					"wire:"+pkg,
					wireagent.Cfg{
						PkgDir:     pkg,
						ModuleRoot: absRoot,
						WireBin:    *wireBin,
					},
					fsBoard, wireBoard, reg,
				))
			}
			fmt.Printf("wire agent: %d package(s) under %s\n", len(wirePkgs), absRoot)
			for _, p := range wirePkgs {
				rel, _ := filepath.Rel(absRoot, p)
				fmt.Printf("  • %s\n", rel)
			}
		}
	}

	// SqlcAgent — single agent for the module.
	var sqlcSup *supervisor.Supervisor[codegen.Trigger, sqlcagent.Fact]
	var sqlcBoard *blackboard.Board[sqlcagent.Fact]
	if *enableSqlc {
		if !sqlcagent.HasSqlcConfig(absRoot) {
			fmt.Printf("sqlc agent: no sqlc.yaml/yml/json at %s; skipping\n", absRoot)
		} else if *sqlcGenRoot == "" {
			fmt.Printf("sqlc agent: -sqlc-gen-root required; skipping\n")
		} else {
			sqlcAbsGen, _ := filepath.Abs(*sqlcGenRoot)
			if r, err := filepath.EvalSymlinks(sqlcAbsGen); err == nil {
				sqlcAbsGen = r
			}
			sqlcBoard = blackboard.New[sqlcagent.Fact](blackboard.Config{
				QuietFor: 1 * time.Second, Rounds: 3,
			})
			sqlcSup = supervisor.New[codegen.Trigger, sqlcagent.Fact](supervisor.Config{
				Name: "sqlc-sup", Logger: log,
			})
			sqlcSup.Attach(sqlcagent.BuildReactiveWorker(
				"sqlc",
				sqlcagent.Cfg{
					ModuleRoot: absRoot,
					GenRoot:    sqlcAbsGen,
					SqlcBin:    *sqlcBin,
				},
				fsBoard, sqlcBoard, reg,
			))
			fmt.Printf("sqlc agent: gen-root=%s\n", sqlcAbsGen)
		}
	}

	ens := ensemble.New(ensemble.Config{Tick: *tick, Convergence: ensemble.All()})
	ensemble.AttachSupervisor(ens, "pkg", pkgSup)
	ensemble.AttachBlackboard(ens, "pkg-board", pkgBoard)
	if genSup != nil {
		ensemble.AttachSupervisor(ens, "gen", genSup)
		ensemble.AttachBlackboard(ens, "gen-board", genBoard)
	}
	if lintSup != nil {
		ensemble.AttachSupervisor(ens, "lint", lintSup)
		ensemble.AttachBlackboard(ens, "lint-board", toolBoard)
	}
	if buildSup != nil {
		ensemble.AttachSupervisor(ens, "build", buildSup)
		ensemble.AttachBlackboard(ens, "build-board", buildBoard)
	}
	if wireSup != nil {
		ensemble.AttachSupervisor(ens, "wire", wireSup)
		ensemble.AttachBlackboard(ens, "wire-board", wireBoard)
	}
	if sqlcSup != nil {
		ensemble.AttachSupervisor(ens, "sqlc", sqlcSup)
		ensemble.AttachBlackboard(ens, "sqlc-board", sqlcBoard)
	}
	if fsBoard != nil {
		ensemble.AttachBlackboard(ens, "fs-board", fsBoard)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	doneChs := []chan struct{}{}
	startSup := func(name string, run func(context.Context) error, reports <-chan supervisor.Report, symbol string) {
		done := make(chan struct{})
		doneChs = append(doneChs, done)
		go func() { run(ctx); close(done) }()
		go func() {
			for r := range reports {
				sym := symbol
				if !r.OK {
					sym = "✗"
				}
				fmt.Printf("  %s %-30s %s\n", sym, r.WorkerID, r.Detail)
			}
		}()
		_ = name
	}
	startSup("pkg", pkgSup.Run, pkgSup.Reports(), "✓")
	if genSup != nil {
		startSup("gen", genSup.Run, genSup.Reports(), "⚙")
	}
	if lintSup != nil {
		startSup("lint", lintSup.Run, lintSup.Reports(), "🔍")
	}
	if buildSup != nil {
		startSup("build", buildSup.Run, buildSup.Reports(), "🔨")
	}
	if wireSup != nil {
		startSup("wire", wireSup.Run, wireSup.Reports(), "🪡")
	}
	if sqlcSup != nil {
		startSup("sqlc", sqlcSup.Run, sqlcSup.Reports(), "🗄")
	}

	fmt.Println("\nwatching for changes (Ctrl-C to stop)…")
	fmt.Println()

	go func() {
		t := time.NewTicker(*tick * 4)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				snap := ens.Snapshot()
				parts := make([]string, 0, len(snap))
				for _, w := range snap {
					parts = append(parts, w.String())
				}
				fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), strings.Join(parts, " | "))
			}
		}
	}()

	<-ctx.Done()
	fmt.Println("\nshutting down…")
	for _, d := range doneChs {
		<-d
	}
}

type pkgInfo struct {
	dir        string
	importPath string
}

func discoverPackages(root string) ([]pkgInfo, error) {
	var pkgs []pkgInfo
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "vendor" || base == "node_modules" || (strings.HasPrefix(base, ".") && base != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		for _, p := range pkgs {
			if p.dir == dir {
				return nil
			}
		}
		rel, _ := filepath.Rel(root, dir)
		if rel == "." {
			rel = filepath.Base(root)
		}
		pkgs = append(pkgs, pkgInfo{dir: dir, importPath: rel})
		return nil
	})
	return pkgs, err
}

func buildLLM(model string) llm.LLM {
	switch model {
	case "haiku":
		return llm.Claude(llm.ClaudeConfig{Model: llm.ClaudeHaiku})
	case "sonnet":
		return llm.Claude(llm.ClaudeConfig{Model: llm.ClaudeSonnet})
	case "opus":
		return llm.Claude(llm.ClaudeConfig{Model: llm.ClaudeOpus})
	case "static":
		return llm.Static(`{"summary":"demo","issues":[],"quality":0.9}`)
	default:
		return nil
	}
}
