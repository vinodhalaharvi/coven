package fsmonitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
)

func TestFsMonitor_PublishesOnEdit(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	board := blackboard.New[FileChangeFact](blackboard.Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	go func() {
		_ = Run(ctx, Config{
			Root: root, Debounce: 80 * time.Millisecond, Board: board,
		})
	}()
	time.Sleep(120 * time.Millisecond) // let watcher attach

	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Subscribe via the high-level API.
	sub := Subscribe(board, AnyFiles)
	ch, err := sub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-ch:
		if len(f.ChangedFiles) == 0 {
			t.Fatalf("expected at least 1 changed file: %+v", f)
		}
		matched := false
		for _, p := range f.ChangedFiles {
			if filepath.Base(p) == "a.go" {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("expected a.go in fact: %+v", f.ChangedFiles)
		}
	case <-time.After(2 * time.Second):
		// Subscriber attached after the post; check the board directly.
		facts := board.List("*")
		if len(facts) == 0 {
			t.Fatal("no FileChangeFact ever published")
		}
	}
}

func TestFsMonitor_FilesUnder_Filter(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	innerA := filepath.Join(root, "a")
	innerB := filepath.Join(root, "b")
	for _, d := range []string{innerA, innerB} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	filter := FilesUnder(innerA)
	if !filter(FileChangeFact{ChangedFiles: []string{filepath.Join(innerA, "x.go")}}) {
		t.Error("filter should accept files under innerA")
	}
	if filter(FileChangeFact{ChangedFiles: []string{filepath.Join(innerB, "x.go")}}) {
		t.Error("filter should reject files under innerB")
	}
}

func TestFsMonitor_FilesWithExtension_Filter(t *testing.T) {
	filter := FilesWithExtension(".proto")
	if !filter(FileChangeFact{ChangedFiles: []string{"/x/y/foo.proto"}}) {
		t.Error("should match .proto")
	}
	if filter(FileChangeFact{ChangedFiles: []string{"/x/y/foo.go"}}) {
		t.Error("should not match .go")
	}
	if filter(FileChangeFact{ChangedFiles: []string{}}) {
		t.Error("empty list should not match")
	}
}

func TestFsMonitor_Subscribe_AppliesFilter(t *testing.T) {
	board := blackboard.New[FileChangeFact](blackboard.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sub := Subscribe(board, FilesWithExtension(".proto"))
	ch, err := sub(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Post non-matching first.
	board.Post("k1", FileChangeFact{ChangedFiles: []string{"/foo/x.go"}}, "test")
	// Then matching.
	board.Post("k2", FileChangeFact{ChangedFiles: []string{"/foo/y.proto"}}, "test")

	select {
	case f := <-ch:
		if filepath.Ext(f.ChangedFiles[0]) != ".proto" {
			t.Errorf("expected proto, got %v", f.ChangedFiles)
		}
	case <-time.After(time.Second):
		t.Fatal("filtered subscription delivered nothing")
	}
}
