package buildhealth

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/worker"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseGoBuildErrors_BasicLines(t *testing.T) {
	output := `./auth/auth.go:10:5: undefined: foo
./api/handler.go:42:1: syntax error
some unrelated line
/abs/path/to/lib/x.go:7:13: cannot use bar (type int) as type string`

	errs := parseGoBuildErrors(output)
	if len(errs) != 3 {
		t.Fatalf("got %d errors, want 3: %+v", len(errs), errs)
	}
	if errs[0].File != "./auth/auth.go" || errs[0].Line != 10 || errs[0].Col != 5 {
		t.Errorf("err[0] = %+v", errs[0])
	}
	if errs[1].Line != 42 || errs[1].Col != 1 {
		t.Errorf("err[1] = %+v", errs[1])
	}
	if errs[2].File != "/abs/path/to/lib/x.go" {
		t.Errorf("err[2] = %+v", errs[2])
	}
}

func TestParseGoBuildErrors_NoColumn(t *testing.T) {
	output := `./pkg/file.go:5: missing return at end of function`
	errs := parseGoBuildErrors(output)
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1", len(errs))
	}
	if errs[0].Line != 5 || errs[0].Col != 0 {
		t.Errorf("err = %+v", errs[0])
	}
}

func TestScatterByPackage_LongestPrefixMatches(t *testing.T) {
	pkgDirs := map[PackageID]string{
		"auth":    "/proj/auth",
		"auth/v2": "/proj/auth/v2",
		"api":     "/proj/api",
	}
	errs := []BuildError{
		{File: "/proj/auth/v2/x.go", Line: 1},
		{File: "/proj/auth/x.go", Line: 1},
		{File: "/proj/api/x.go", Line: 1},
		{File: "/proj/elsewhere/x.go", Line: 1},
	}
	out := scatterByPackage(errs, pkgDirs, "/proj")
	if len(out["auth/v2"]) != 1 {
		t.Errorf("auth/v2 got %d", len(out["auth/v2"]))
	}
	if len(out["auth"]) != 1 {
		t.Errorf("auth got %d", len(out["auth"]))
	}
	if len(out["api"]) != 1 {
		t.Errorf("api got %d", len(out["api"]))
	}
	// The /proj/elsewhere/ error should be unattributed.
	un := unattributedErrors(errs, out)
	if len(un) != 1 {
		t.Errorf("unattributed = %d, want 1", len(un))
	}
}

// TestHandle_RealGoBuild_Healthy runs the real `go build ./...` against a
// temp module that compiles cleanly. Skipped if go isn't on PATH.
func TestHandle_RealGoBuild_Healthy(t *testing.T) {
	if _, err := os.Stat("/usr/lib/go-1.22/bin/go"); err != nil {
		// Fallback: rely on PATH.
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	pkgDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "auth.go"), []byte("package auth\nfunc Hi() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	board := blackboard.New[BuildHealthFact](blackboard.Config{})
	a := &agent{cfg: Config{
		AgentID:    "build",
		ModuleRoot: root,
		Packages:   map[PackageID]string{"auth": pkgDir},
		BuildBoard: board,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	facts, err := freeap.Run(ctx, a.handle(Trigger{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 { // auth + module
		t.Fatalf("want 2 facts (auth + module), got %d", len(facts))
	}
	authFact, ok := board.Get(Key("auth"))
	if !ok {
		t.Fatal("auth fact missing")
	}
	if !authFact.Value.OK {
		// Find module fact for diagnostics
		modFact, _ := board.Get(ModuleKey)
		t.Errorf("auth should be clean, got errors: %+v\nmodule output: %q", authFact.Value.Errors, modFact.Value.Output)
	}
}

// TestHandle_RealGoBuild_BrokenSurfacesError uses a deliberately broken
// package so we can verify error scattering against a real go invocation.
func TestHandle_RealGoBuild_BrokenSurfacesError(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	pkgDir := filepath.Join(root, "broken")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "broken.go"), []byte("package broken\nfunc Hi() { return undefined }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	board := blackboard.New[BuildHealthFact](blackboard.Config{})
	a := &agent{cfg: Config{
		AgentID:    "build",
		ModuleRoot: root,
		Packages:   map[PackageID]string{"broken": pkgDir},
		BuildBoard: board,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := freeap.Run(ctx, a.handle(Trigger{}))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := board.Get(Key("broken"))
	if !ok {
		t.Fatal("broken fact missing")
	}
	if f.Value.OK {
		t.Error("broken should NOT be OK")
	}
	if len(f.Value.Errors) == 0 {
		modFact, _ := board.Get(ModuleKey)
		t.Errorf("expected at least one parsed error; module output: %q", modFact.Value.Output)
	}
}

// TestPipeline_TriggersAfterFsBurst verifies the Source plumbing — when
// FileChangeFacts arrive, after settle the build agent fires and posts.
func TestPipeline_TriggersAfterFsBurst(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	pkgDir := filepath.Join(root, "p")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "p.go"), []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}

	fsBoard := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{})
	buildBoard := blackboard.New[BuildHealthFact](blackboard.Config{})

	w := BuildReactiveWorker(Config{
		AgentID:    "build",
		ModuleRoot: root,
		Packages:   map[worker.PackageID]string{"p": pkgDir},
		FSBoard:    fsBoard,
		BuildBoard: buildBoard,
		SettleFor:  150 * time.Millisecond,
	})

	sup := supervisor.New[Trigger, []BuildHealthFact](supervisor.Config{
		Name: "build-sup", Logger: quietLogger(),
	})
	sup.Attach(w)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	go func() {
		for range sup.Reports() {
		}
	}()
	time.Sleep(200 * time.Millisecond) // let subscription bind

	// Burst of FileChangeFacts. After SettleFor, the build agent should fire.
	for i := 0; i < 3; i++ {
		fsBoard.Post("fs:k"+string(rune('a'+i)), fsmonitor.FileChangeFact{
			ChangedFiles: []string{filepath.Join(pkgDir, "p.go")},
			ChangedAt:    time.Now(),
		}, "test")
		time.Sleep(40 * time.Millisecond)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no BuildHealthFact arrived")
		default:
		}
		if _, ok := buildBoard.Get(Key("p")); ok {
			break
		}
		time.Sleep(80 * time.Millisecond)
	}
	cancel()
	<-done
}
