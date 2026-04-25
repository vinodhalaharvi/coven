package toolagent

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/worker"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// helper: post a healthy package fact for a known set.
func seedPackages(b *blackboard.Board[worker.PackageFact], pkgs ...worker.PackageID) {
	for _, p := range pkgs {
		b.Post("pkg:"+string(p), worker.PackageFact{
			Pkg:        p,
			Build:      worker.BuildResult{OK: true},
			Vet:        worker.VetResult{Clean: true},
			ObservedAt: time.Now(),
		}, "test")
	}
}

func TestPathPrefixScatter_BasicAssignment(t *testing.T) {
	dirs := map[worker.PackageID]string{
		"auth": "/proj/auth",
		"api":  "/proj/api",
	}
	scatter := PathPrefixScatter(dirs)
	issues := []Issue{
		{File: "/proj/auth/auth.go", Line: 10, Message: "issue1", Rule: "errcheck"},
		{File: "/proj/api/handler.go", Line: 5, Message: "issue2", Rule: "ineffassign"},
		{File: "/proj/auth/helper.go", Line: 3, Message: "issue3", Rule: "errcheck"},
	}
	out := scatter([]worker.PackageID{"auth", "api"}, issues)
	if len(out["auth"]) != 2 {
		t.Errorf("auth got %d issues, want 2: %+v", len(out["auth"]), out["auth"])
	}
	if len(out["api"]) != 1 {
		t.Errorf("api got %d issues, want 1: %+v", len(out["api"]), out["api"])
	}
}

func TestPathPrefixScatter_EmptyForUnaffectedPackages(t *testing.T) {
	dirs := map[worker.PackageID]string{
		"auth": "/proj/auth",
		"api":  "/proj/api",
	}
	scatter := PathPrefixScatter(dirs)
	out := scatter([]worker.PackageID{"auth", "api"}, nil)
	// Both should have keys (with nil/empty slices) so consumers know they were checked.
	if _, ok := out["auth"]; !ok {
		t.Error("missing auth entry")
	}
	if _, ok := out["api"]; !ok {
		t.Error("missing api entry")
	}
}

func TestToolAgent_HandleScattersFacts(t *testing.T) {
	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{})
	toolBoard := blackboard.New[ToolFact](blackboard.Config{})
	seedPackages(pkgBoard, "auth", "api")

	dirs := map[worker.PackageID]string{
		"auth": "/proj/auth",
		"api":  "/proj/api",
	}
	a := &agent{cfg: Config{
		AgentID:    "linter",
		ToolName:   "fake-lint",
		ModuleRoot: "/proj",
		PkgBoard:   pkgBoard,
		ToolBoard:  toolBoard,
		Runner: FakeLinter([]Issue{
			{File: "/proj/auth/auth.go", Line: 1, Message: "boom", Rule: "x"},
		}, "fake output"),
		Scatter: PathPrefixScatter(dirs),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prog := a.handle(Trigger{Triggered: []worker.PackageID{"auth"}})
	facts, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 facts, got %d", len(facts))
	}

	// Both blackboard keys should be populated.
	authF, ok := toolBoard.Get(Key("fake-lint", "auth"))
	if !ok {
		t.Fatal("auth tool fact missing")
	}
	if authF.Value.OK {
		t.Error("auth should not be OK (it has 1 issue)")
	}
	if len(authF.Value.Issues) != 1 {
		t.Errorf("auth issues = %d, want 1", len(authF.Value.Issues))
	}

	apiF, ok := toolBoard.Get(Key("fake-lint", "api"))
	if !ok {
		t.Fatal("api tool fact missing")
	}
	if !apiF.Value.OK {
		t.Error("api should be OK (no issues)")
	}
}

func TestToolAgent_RunnerFailure_PostsUnhealthyForAll(t *testing.T) {
	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{})
	toolBoard := blackboard.New[ToolFact](blackboard.Config{})
	seedPackages(pkgBoard, "auth", "api", "billing")

	a := &agent{cfg: Config{
		AgentID:    "linter",
		ToolName:   "fake-lint",
		ModuleRoot: "/proj",
		PkgBoard:   pkgBoard,
		ToolBoard:  toolBoard,
		Runner:     FailingLinter("install golangci-lint"),
		Scatter:    PathPrefixScatter(nil),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	facts, err := freeap.Run(ctx, a.handle(Trigger{}))
	if err != nil {
		t.Fatalf("invocation error should not bubble: %v", err)
	}
	if len(facts) != 3 {
		t.Errorf("got %d facts, want 3", len(facts))
	}
	for _, f := range facts {
		if f.OK {
			t.Errorf("fact %s should be OK=false", f.Pkg)
		}
	}
}

func TestToolAgent_Pipeline_FiresAfterSettle(t *testing.T) {
	pkgBoard := blackboard.New[worker.PackageFact](blackboard.Config{})
	toolBoard := blackboard.New[ToolFact](blackboard.Config{})

	// Seed initial state so when the trigger fires, allPackages is non-empty.
	seedPackages(pkgBoard, "auth")

	dirs := map[worker.PackageID]string{
		"auth": filepath.Clean("/proj/auth"),
	}
	w := BuildReactiveWorker(Config{
		AgentID:    "linter",
		ToolName:   "fake-lint",
		ModuleRoot: "/proj",
		PkgBoard:   pkgBoard,
		ToolBoard:  toolBoard,
		Runner:     FakeLinter(nil, "clean"),
		Scatter:    PathPrefixScatter(dirs),
		SettleFor:  150 * time.Millisecond,
	})

	sup := supervisor.New[Trigger, []ToolFact](supervisor.Config{
		Name: "lint-sup", Logger: quietLogger(),
	})
	sup.Attach(w)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	go func() {
		for range sup.Reports() {
		}
	}()

	// Allow subscription to bind (the BlackboardSource subscribes inside source()).
	time.Sleep(80 * time.Millisecond)

	// Burst of upstream activity.
	for i := 0; i < 3; i++ {
		seedPackages(pkgBoard, "auth")
		time.Sleep(30 * time.Millisecond)
	}

	// After settle, expect a tool fact for auth.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no lint fact arrived")
		default:
		}
		if _, ok := toolBoard.Get(Key("fake-lint", "auth")); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
