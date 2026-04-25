package codegen

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/ownership"
)

// testFact is a minimal fact type used by the tests.
type testFact struct {
	AgentID      string
	OK           bool
	ChangedFiles []string
	Output       string
}

type testCfg struct {
	OutputDir string
	Body      []byte
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRunner returns a Runner that emits a fixed file with body cfg.Body.
func fakeRunner() Runner[testCfg] {
	return func(ctx context.Context, cfg testCfg) (RunResult, error) {
		return RunResult{
			Files: []GeneratedFile{
				{Path: filepath.Join(cfg.OutputDir, "out.go"), Bytes: cfg.Body},
			},
			Output: "ok",
		}, nil
	}
}

// failingRunner returns a Runner that always errors at invocation.
func failingRunner(msg string) Runner[testCfg] {
	return func(ctx context.Context, cfg testCfg) (RunResult, error) {
		return RunResult{}, errors.New(msg)
	}
}

// projector wires testCfg + RunResult into testFact.
func projector(cfg testCfg, res RunResult, changed []string, runErr error) testFact {
	return testFact{
		AgentID:      "test-agent",
		OK:           runErr == nil,
		ChangedFiles: changed,
		Output:       res.Output,
	}
}

// staticTrigger returns a Source that emits exactly one trigger then
// stays open. The supervisor's loop expects a long-lived source; the
// test cancels the context to terminate.
func staticTrigger(t Trigger) func(ctx context.Context) (<-chan Trigger, error) {
	return func(ctx context.Context) (<-chan Trigger, error) {
		ch := make(chan Trigger, 1)
		ch <- t
		return ch, nil
	}
}

func TestAgent_HandleProducesFile(t *testing.T) {
	dir := t.TempDir()
	board := blackboard.New[testFact](blackboard.Config{})
	reg := ownership.New()

	cfg := Config[testCfg, testFact]{
		AgentID:  "test-agent",
		Cfg:      testCfg{OutputDir: dir, Body: []byte("package x\n")},
		Runner:   fakeRunner(),
		Project:  projector,
		Board:    board,
		BoardKey: func(f testFact) string { return "test:" + f.AgentID },
		Owner:    reg,
	}

	a := &agent[testCfg, testFact]{cfg: cfg}
	prog := a.handle(Trigger{Reason: "test"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatal(err)
	}

	// File should exist on disk.
	out := filepath.Join(dir, "out.go")
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	if string(body) != "package x\n" {
		t.Errorf("body mismatch: %q", body)
	}

	// Fact should be healthy and list the changed file.
	if !fact.OK {
		t.Error("fact should be OK")
	}
	if len(fact.ChangedFiles) != 1 || fact.ChangedFiles[0] != out {
		t.Errorf("ChangedFiles = %v, want [%s]", fact.ChangedFiles, out)
	}

	// Ownership claimed.
	if owner, owned := reg.Owner(out); !owned || owner != "test-agent" {
		t.Errorf("ownership = %q,%v, want test-agent,true", owner, owned)
	}

	// Board has the fact.
	bf, ok := board.Get("test:test-agent")
	if !ok {
		t.Fatal("fact missing from board")
	}
	if !bf.Value.OK {
		t.Error("board fact should be OK")
	}
}

func TestAgent_DiffWriteSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	body := []byte("package x\nconst V = 1\n")

	// Pre-populate the output file with the same bytes the runner will emit.
	out := filepath.Join(dir, "out.go")
	if err := os.WriteFile(out, body, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config[testCfg, testFact]{
		AgentID:  "test-agent",
		Cfg:      testCfg{OutputDir: dir, Body: body},
		Runner:   fakeRunner(),
		Project:  projector,
		BoardKey: func(f testFact) string { return "k" },
	}
	a := &agent[testCfg, testFact]{cfg: cfg}
	prog := a.handle(Trigger{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(fact.ChangedFiles) != 0 {
		t.Errorf("expected 0 changed files (bytes unchanged), got %v", fact.ChangedFiles)
	}
}

func TestAgent_RunnerFailureProducesUnhealthyFact(t *testing.T) {
	board := blackboard.New[testFact](blackboard.Config{})

	cfg := Config[testCfg, testFact]{
		AgentID:  "test-agent",
		Cfg:      testCfg{OutputDir: t.TempDir()},
		Runner:   failingRunner("install the tool"),
		Project:  projector,
		Board:    board,
		BoardKey: func(f testFact) string { return "test:fail" },
	}
	a := &agent[testCfg, testFact]{cfg: cfg}
	prog := a.handle(Trigger{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatalf("Program should not bubble runner errors: %v", err)
	}
	if fact.OK {
		t.Error("fact should be unhealthy after runner failure")
	}
	bf, ok := board.Get("test:fail")
	if !ok {
		t.Fatal("unhealthy fact missing from board")
	}
	if bf.Value.OK {
		t.Error("board fact should be OK=false")
	}
}

func TestAgent_ProgramShape_FlatMapAtCascadeBreaker(t *testing.T) {
	cfg := Config[testCfg, testFact]{
		AgentID:  "shape-test",
		Cfg:      testCfg{OutputDir: t.TempDir()},
		Runner:   fakeRunner(),
		Project:  projector,
		BoardKey: func(f testFact) string { return "k" },
	}
	a := &agent[testCfg, testFact]{cfg: cfg}
	prog := a.handle(Trigger{})

	// The top-level node should be FlatMap (run → continuation).
	d := prog.Describe()
	if d.Kind != "flatmap" {
		t.Errorf("top-level should be flatmap, got %s", d.Kind)
	}
}

func TestAgent_ReportProjectsHealth(t *testing.T) {
	cfg := Config[testCfg, testFact]{
		AgentID: "report-test",
		HealthFromFact: func(f testFact) bool {
			return f.OK
		},
	}
	a := &agent[testCfg, testFact]{cfg: cfg}

	r := a.report(testFact{OK: true}, nil)
	if !r.OK {
		t.Error("healthy fact should produce OK report")
	}
	r2 := a.report(testFact{OK: false}, nil)
	if r2.OK {
		t.Error("unhealthy fact should produce non-OK report")
	}
	r3 := a.report(testFact{}, errors.New("runtime"))
	if r3.OK {
		t.Error("error should produce non-OK report")
	}
}

// End-to-end: drive an agent with one trigger via the supervisor, expect
// the fact to land on the board.
func TestAgent_EndToEndWithSupervisor(t *testing.T) {
	dir := t.TempDir()
	board := blackboard.New[testFact](blackboard.Config{})
	reg := ownership.New()

	cfg := Config[testCfg, testFact]{
		AgentID:  "e2e",
		Cfg:      testCfg{OutputDir: dir, Body: []byte("package x\n")},
		Runner:   fakeRunner(),
		Project:  projector,
		Board:    board,
		BoardKey: func(f testFact) string { return "test:" + f.AgentID },
		Owner:    reg,
		Source:   staticTrigger(Trigger{Reason: "test", At: time.Now()}),
	}
	w := BuildReactiveWorker(cfg)

	sup := supervisor.New[Trigger, testFact](supervisor.Config{
		Name: "e2e-sup", Logger: quietLogger(),
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

	// Wait for the fact to land. Note: the projector hardcodes AgentID
	// = "test-agent", so the board key includes that, not the e2e agent ID.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("fact never arrived on board")
		default:
		}
		if _, ok := board.Get("test:test-agent"); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
