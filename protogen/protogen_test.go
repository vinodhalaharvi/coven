package protogen

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
	"github.com/vinodhalaharvi/coven/ownership"
	"github.com/vinodhalaharvi/coven/worker"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setupProject(t *testing.T) (protoRoot, genRoot string) {
	t.Helper()
	root := t.TempDir()
	protoRoot = filepath.Join(root, "proto")
	genRoot = filepath.Join(root, "gen")
	if err := os.MkdirAll(protoRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(genRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoRoot, "user.proto"), []byte(
		`syntax = "proto3"; package user.v1; message User { string id = 1; }`,
	), 0644); err != nil {
		t.Fatal(err)
	}
	return
}

func TestProtoGen_HandleProgram_Success(t *testing.T) {
	protoRoot, genRoot := setupProject(t)
	board := blackboard.New[ProtoGenFact](blackboard.Config{})
	reg := ownership.New()

	runner := FakeRunner(map[string]string{
		"user/v1/user.pb.go": "// generated\npackage userv1\ntype User struct{ ID string }\n",
	})

	a := &agent{cfg: Config{
		AgentID:   "protogen",
		ProtoRoot: protoRoot,
		GenRoot:   genRoot,
		Board:     board,
		Ownership: reg,
		Runner:    runner,
		WriteFile: DefaultDiffWriter,
	}}

	prog := a.handle(worker.FSEvent{Dir: protoRoot})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !fact.OK {
		t.Fatalf("fact not OK: %+v", fact)
	}
	if len(fact.Generated) != 1 {
		t.Fatalf("Generated = %d, want 1", len(fact.Generated))
	}
	if len(fact.ChangedFiles) != 1 {
		t.Fatalf("ChangedFiles = %d, want 1 (first run)", len(fact.ChangedFiles))
	}

	// File should exist on disk.
	out := filepath.Join(genRoot, "user/v1/user.pb.go")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output not written: %v", err)
	}

	// Ownership should be claimed.
	owner, ok := reg.Owner(out)
	if !ok || owner != "protogen" {
		t.Errorf("ownership = %q,%v, want protogen,true", owner, ok)
	}

	// Fact should be on the blackboard.
	if _, ok := board.Get(factKey("protogen")); !ok {
		t.Error("fact not on blackboard")
	}
}

func TestProtoGen_DiffWriter_SkipsUnchanged(t *testing.T) {
	protoRoot, genRoot := setupProject(t)
	board := blackboard.New[ProtoGenFact](blackboard.Config{})

	content := "// stable\npackage userv1\n"
	runner := FakeRunner(map[string]string{
		"user/v1/user.pb.go": content,
	})

	a := &agent{cfg: Config{
		AgentID:   "protogen",
		ProtoRoot: protoRoot,
		GenRoot:   genRoot,
		Board:     board,
		Runner:    runner,
		WriteFile: DefaultDiffWriter,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First run: should write.
	f1, err := freeap.Run(ctx, a.handle(worker.FSEvent{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(f1.ChangedFiles) != 1 {
		t.Errorf("first run ChangedFiles = %d, want 1", len(f1.ChangedFiles))
	}
	if f1.Generated[0].Skipped {
		t.Error("first run should not be skipped")
	}

	// Second run: same content, should skip.
	f2, err := freeap.Run(ctx, a.handle(worker.FSEvent{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(f2.ChangedFiles) != 0 {
		t.Errorf("second run ChangedFiles = %d, want 0 (idempotent)", len(f2.ChangedFiles))
	}
	if !f2.Generated[0].Skipped {
		t.Error("second run should be skipped")
	}
}

func TestProtoGen_RunnerFailure_PostsUnhealthyFact(t *testing.T) {
	protoRoot, genRoot := setupProject(t)
	board := blackboard.New[ProtoGenFact](blackboard.Config{})

	a := &agent{cfg: Config{
		AgentID:   "protogen",
		ProtoRoot: protoRoot,
		GenRoot:   genRoot,
		Board:     board,
		Runner:    FailingRunner("compilation failed"),
		WriteFile: DefaultDiffWriter,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, a.handle(worker.FSEvent{}))
	if err != nil {
		t.Fatalf("program should not bubble up: %v", err)
	}
	if fact.OK {
		t.Error("fact should not be OK on runner failure")
	}
	posted, ok := board.Get(factKey("protogen"))
	if !ok || posted.Value.OK {
		t.Errorf("expected unhealthy fact on blackboard")
	}
}

func TestProtoGen_FullPipeline_FSNotifyToBoard(t *testing.T) {
	protoRoot, genRoot := setupProject(t)
	board := blackboard.New[ProtoGenFact](blackboard.Config{})
	reg := ownership.New()

	runner := FakeRunner(map[string]string{
		"user/v1/user.pb.go": "// gen v1\n",
	})

	w := BuildReactiveWorker(Config{
		AgentID:   "protogen",
		ProtoRoot: protoRoot,
		GenRoot:   genRoot,
		Board:     board,
		Ownership: reg,
		Runner:    runner,
		Debounce:  60 * time.Millisecond,
	})

	sup := supervisor.New[worker.FSEvent, ProtoGenFact](supervisor.Config{
		Name: "test", Logger: quietLogger(),
	})
	sup.Attach(w)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	go func() {
		for range sup.Reports() {
		}
	}()

	// Let watcher attach.
	time.Sleep(200 * time.Millisecond)

	// Edit a proto.
	if err := os.WriteFile(filepath.Join(protoRoot, "user.proto"), []byte(
		`syntax = "proto3"; package user.v1; message User { string id = 1; string name = 2; }`,
	), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for the fact.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no fact posted in time")
		default:
		}
		if f, ok := board.Get(factKey("protogen")); ok && f.Value.OK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done
}
