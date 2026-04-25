package integration

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/ownership"
	"github.com/vinodhalaharvi/coven/protogen"
	"github.com/vinodhalaharvi/coven/sources"
	"github.com/vinodhalaharvi/coven/toolagent"
	"github.com/vinodhalaharvi/coven/worker"
)

// setupCascadeProject builds a minimal Go module + proto root in a temp
// directory. Layout:
//
//   <root>/
//     go.mod
//     proto/user/v1/user.proto
//     gen/        (empty; protogen will populate)
//     consumer/consumer.go (imports from gen/...)
func setupCascadeProject(t *testing.T) (root, protoRoot, genRoot, consumerDir string) {
	t.Helper()
	root = t.TempDir()
	protoRoot = filepath.Join(root, "proto", "user", "v1")
	genRoot = filepath.Join(root, "gen", "user", "v1")
	consumerDir = filepath.Join(root, "consumer")

	for _, d := range []string{protoRoot, genRoot, consumerDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "go.mod"),
		"module cascade.test\n\ngo 1.22\n")
	mustWrite(t, filepath.Join(protoRoot, "user.proto"),
		`syntax = "proto3"; package user.v1; message User { string id = 1; }`)
	// gen package starts empty — protogen will write into it.
	// consumer imports from gen.
	mustWrite(t, filepath.Join(consumerDir, "consumer.go"),
		`package consumer

import gen "cascade.test/gen/user/v1"

func Use(u gen.User) string { return u.ID }
`)
	return
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCascade_ProtoChangeRipplesToConsumer(t *testing.T) {
	root, protoRoot, genRoot, consumerDir := setupCascadeProject(t)

	// Boards.
	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{
		QuietFor: 300 * time.Millisecond,
		Rounds:   2,
	})
	genBoard := blackboard.New[protogen.ProtoGenFact](blackboard.Config{
		QuietFor: 300 * time.Millisecond,
		Rounds:   2,
	})

	// Ownership registry.
	reg := ownership.New()

	// FakeRunner produces a valid Go file in genRoot. Subsequent versions
	// add a field; this lets us verify the cascade — consumer must rebuild
	// when gen changes.
	currentSchema := []byte(`package userv1

type User struct {
	ID string
}
`)
	genRunner := func(ctx context.Context, pr, gr string) ([]protogen.GeneratedFile, string, error) {
		return []protogen.GeneratedFile{
			{Path: filepath.Join(genRoot, "user.pb.go"), Bytes: append([]byte(nil), currentSchema...)},
		}, "ok", nil
	}

	// Supervisor (one tree managing every agent kind).
	sup := supervisor.New[any, any](supervisor.Config{Name: "root", Logger: quietLogger()})
	_ = sup // we'll attach to specialized supervisors per agent kind

	// Use one supervisor per worker-event-type because Go generics require
	// homogeneous attach. This is the existential boundary in practice.
	pkgSup := supervisor.New[worker.FSEvent, worker.PackageFact](supervisor.Config{
		Name: "pkg-sup", Logger: quietLogger(),
	})
	genSup := supervisor.New[worker.FSEvent, protogen.ProtoGenFact](supervisor.Config{
		Name: "gen-sup", Logger: quietLogger(),
	})

	// Attach the protogen agent.
	genSup.Attach(protogen.BuildReactiveWorker(protogen.Config{
		AgentID:   "protogen",
		ProtoRoot: protoRoot,
		GenRoot:   genRoot,
		Board:     genBoard,
		Ownership: reg,
		Runner:    genRunner,
		Debounce:  60 * time.Millisecond,
	}))

	// Attach package agents for each Go package in the project. Each gets
	// TWO extra sources merged with its own fsnotify:
	//   - genBoard: synthetic event when a generated file lands in its dir
	//   - pkgBoard: synthetic event when an upstream import's fact changes
	pkgs := map[worker.PackageID]string{
		"gen/user/v1": genRoot,
		"consumer":    consumerDir,
	}
	// Static upstream map for this test (in real life: derived from PackageFact.Imports).
	upstreams := map[worker.PackageID][]string{
		"consumer": {"pkg:gen/user/v1"},
	}
	for pkgID, dir := range pkgs {
		pkgIDLocal := pkgID
		dirLocal := dir
		fromGen := sources.BlackboardSource(genBoard, "*",
			func(f blackboard.Fact[protogen.ProtoGenFact]) (worker.FSEvent, bool) {
				if !f.Value.OK || len(f.Value.ChangedFiles) == 0 {
					return worker.FSEvent{}, false
				}
				for _, p := range f.Value.ChangedFiles {
					if filepath.Dir(p) == dirLocal {
						return worker.FSEvent{
							Dir:          dirLocal,
							ChangedAt:    time.Now(),
							ChangedFiles: f.Value.ChangedFiles,
						}, true
					}
				}
				return worker.FSEvent{}, false
			}, 16,
		)
		extra := sources.Source[worker.FSEvent](fromGen)

		// If this package has declared upstreams, also subscribe to their facts.
		if ups, ok := upstreams[pkgIDLocal]; ok {
			upSet := make(map[string]bool, len(ups))
			for _, u := range ups {
				upSet[u] = true
			}
			fromPkgs := sources.BlackboardSource(pkgBoard, "pkg:*",
				func(f blackboard.Fact[worker.PackageFact]) (worker.FSEvent, bool) {
					if !upSet[f.Key] {
						return worker.FSEvent{}, false
					}
					return worker.FSEvent{
						Dir:       dirLocal,
						ChangedAt: time.Now(),
					}, true
				}, 16,
			)
			extra = sources.MergeSources(extra, sources.Source[worker.FSEvent](fromPkgs))
		}

		pkgSup.Attach(worker.BuildReactiveWorker(worker.PackageAgentConfig{
			Pkg:         pkgIDLocal,
			Dir:         dirLocal,
			Board:       pkgBoard,
			Ownership:   reg,
			ExtraSource: extra,
		}, 60*time.Millisecond))
	}

	// Run both supervisors.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pkgDone := make(chan struct{})
	genDone := make(chan struct{})
	go func() { pkgSup.Run(ctx); close(pkgDone) }()
	go func() { genSup.Run(ctx); close(genDone) }()
	go func() {
		for range pkgSup.Reports() {
		}
	}()
	go func() {
		for range genSup.Reports() {
		}
	}()

	// Allow watchers + subscriptions to bind.
	time.Sleep(300 * time.Millisecond)

	// 1. Trigger initial proto change → expect protogen to write user.pb.go.
	mustWrite(t, filepath.Join(protoRoot, "user.proto"),
		`syntax = "proto3"; package user.v1; message User { string id = 1; string name = 2; }`)

	// Wait for protogen to post a fact.
	if !waitFor(15*time.Second, func() bool {
		f2, ok2 := genBoard.Get("protogen:protogen")
		return ok2 && f2.Value.OK && len(f2.Value.ChangedFiles) > 0
	}) {
		t.Fatal("protogen did not produce an initial fact")
	}

	// Wait for both package agents to post facts (gen/user/v1 and consumer).
	if !waitFor(15*time.Second, func() bool {
		_, okGen := pkgBoard.Get("pkg:gen/user/v1")
		_, okCon := pkgBoard.Get("pkg:consumer")
		return okGen && okCon
	}) {
		facts := pkgBoard.List("pkg:*")
		got := []string{}
		for _, f := range facts {
			got = append(got, string(f.Value.Pkg))
		}
		sort.Strings(got)
		t.Fatalf("did not see both package facts; got: %v", got)
	}

	// 2. Both packages should be healthy after the cascade settles.
	if !waitFor(15*time.Second, func() bool {
		gf, okG := pkgBoard.Get("pkg:gen/user/v1")
		cf, okC := pkgBoard.Get("pkg:consumer")
		return okG && okC && gf.Value.Healthy() && cf.Value.Healthy()
	}) {
		gf, _ := pkgBoard.Get("pkg:gen/user/v1")
		cf, _ := pkgBoard.Get("pkg:consumer")
		t.Fatalf("packages not healthy: gen=%+v consumer=%+v",
			gf.Value.Build, cf.Value.Build)
	}

	// 3. Verify ownership is correctly tracked.
	owner, owned := reg.Owner(filepath.Join(genRoot, "user.pb.go"))
	if !owned || owner != "protogen" {
		t.Errorf("user.pb.go ownership = %q,%v, want protogen,true", owner, owned)
	}

	// 4. Now make a breaking change in the schema and verify consumer goes red.
	currentSchema = []byte(`package userv1

type User struct {
	UserID string  // renamed field — consumer still references .ID
}
`)
	mustWrite(t, filepath.Join(protoRoot, "user.proto"),
		`syntax = "proto3"; package user.v1; message User { string user_id = 1; }`)

	if !waitFor(20*time.Second, func() bool {
		cf, ok := pkgBoard.Get("pkg:consumer")
		return ok && !cf.Value.Build.OK // consumer should fail to build
	}) {
		cf, _ := pkgBoard.Get("pkg:consumer")
		t.Fatalf("consumer did not detect break; build=%+v", cf.Value.Build)
	}

	// 5. Fix the consumer; it should recover.
	mustWrite(t, filepath.Join(consumerDir, "consumer.go"),
		`package consumer

import gen "cascade.test/gen/user/v1"

func Use(u gen.User) string { return u.UserID }
`)

	if !waitFor(20*time.Second, func() bool {
		cf, ok := pkgBoard.Get("pkg:consumer")
		return ok && cf.Value.Healthy()
	}) {
		cf, _ := pkgBoard.Get("pkg:consumer")
		t.Fatalf("consumer did not recover after fix; build=%+v", cf.Value.Build)
	}

	cancel()
	<-pkgDone
	<-genDone

	_ = root
	_ = strings.HasSuffix
}

func TestCascade_OwnershipPreventsFeedbackLoop(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "go.mod"), "module loop.test\n\ngo 1.22\n")
	mustWrite(t, filepath.Join(pkgDir, "owned.go"), "package pkg\nfunc Hi() {}\n")

	reg := ownership.New()
	// Pretend a generator owns "foreign.go" inside this package's dir.
	foreignPath := filepath.Join(pkgDir, "foreign.go")
	mustWrite(t, foreignPath, "package pkg\nfunc Foreign() {}\n")
	reg.Claim(foreignPath, "some-other-agent")

	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{})

	pkgSup := supervisor.New[worker.FSEvent, worker.PackageFact](supervisor.Config{
		Name: "loop-test", Logger: quietLogger(),
	})
	pkgSup.Attach(worker.BuildReactiveWorker(worker.PackageAgentConfig{
		Pkg:       "pkg",
		Dir:       pkgDir,
		Board:     pkgBoard,
		Ownership: reg,
	}, 50*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { pkgSup.Run(ctx); close(done) }()
	go func() {
		for range pkgSup.Reports() {
		}
	}()
	time.Sleep(200 * time.Millisecond)

	// Edit the foreign-owned file. This should NOT trigger the agent.
	mustWrite(t, foreignPath, "package pkg\nfunc Foreign() { /* edited */ }\n")

	// Sleep past the debounce + a margin. The agent should still have no fact.
	time.Sleep(400 * time.Millisecond)
	if _, ok := pkgBoard.Get("pkg:pkg"); ok {
		t.Fatal("agent reacted to foreign-owned file change")
	}

	// Edit the owned file. This SHOULD trigger.
	mustWrite(t, filepath.Join(pkgDir, "owned.go"), "package pkg\nfunc Hi() { /* edit */ }\n")
	if !waitFor(5*time.Second, func() bool {
		_, ok := pkgBoard.Get("pkg:pkg")
		return ok
	}) {
		t.Fatal("agent did not react to its own file")
	}

	cancel()
	<-done
}

func TestCascade_LintFiresAfterPackagesSettle(t *testing.T) {
	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{})
	toolBoard := blackboard.New[toolagent.ToolFact](blackboard.Config{})

	// Pre-seed two packages so the scatter has something to assign to.
	for _, p := range []worker.PackageID{"a", "b"} {
		pkgBoard.Post("pkg:"+string(p), worker.PackageFact{
			Pkg:        p,
			Build:      worker.BuildResult{OK: true},
			Vet:        worker.VetResult{Clean: true},
			ObservedAt: time.Now(),
		}, "test")
	}

	dirs := map[worker.PackageID]string{"a": "/proj/a", "b": "/proj/b"}
	w := toolagent.BuildReactiveWorker(toolagent.Config{
		AgentID:    "linter",
		ToolName:   "fake-lint",
		ModuleRoot: "/proj",
		PkgBoard:   pkgBoard,
		ToolBoard:  toolBoard,
		Runner: toolagent.FakeLinter([]toolagent.Issue{
			{File: "/proj/a/main.go", Line: 1, Message: "uh oh", Rule: "fake"},
		}, "ok"),
		Scatter:   toolagent.PathPrefixScatter(dirs),
		SettleFor: 100 * time.Millisecond,
	})
	sup := supervisor.New[toolagent.Trigger, []toolagent.ToolFact](supervisor.Config{
		Name: "lint-sup", Logger: quietLogger(),
	})
	sup.Attach(w)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	go func() {
		for range sup.Reports() {
		}
	}()
	time.Sleep(150 * time.Millisecond)

	// Trigger a burst of upstream changes.
	for i := 0; i < 4; i++ {
		pkgBoard.Post("pkg:a", worker.PackageFact{
			Pkg:        "a",
			Build:      worker.BuildResult{OK: true},
			Vet:        worker.VetResult{Clean: true},
			ObservedAt: time.Now(),
		}, "test")
		time.Sleep(20 * time.Millisecond)
	}

	if !waitFor(5*time.Second, func() bool {
		_, okA := toolBoard.Get(toolagent.Key("fake-lint", "a"))
		_, okB := toolBoard.Get(toolagent.Key("fake-lint", "b"))
		return okA && okB
	}) {
		t.Fatal("lint facts did not arrive")
	}

	fa, _ := toolBoard.Get(toolagent.Key("fake-lint", "a"))
	fb, _ := toolBoard.Get(toolagent.Key("fake-lint", "b"))
	if fa.Value.OK {
		t.Error("a should have issues")
	}
	if !fb.Value.OK {
		t.Error("b should be clean")
	}

	cancel()
	<-done
}

// waitFor polls cond every 50ms until it's true or timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}
