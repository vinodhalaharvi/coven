package protogen

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestExecRunner_WorkDirIsProtoRoot verifies the legacy ExecRunner runs
// the command from protoRoot.
func TestExecRunner_WorkDirIsProtoRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell semantics")
	}
	protoRoot := t.TempDir()
	genRoot := t.TempDir()

	// Use 'pwd' as our "tool" so we can capture which dir it ran from.
	r := ExecRunner("pwd", nil, genRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, output, err := r(ctx, protoRoot, genRoot)
	if err != nil {
		t.Fatalf("pwd should succeed: %v; output=%s", err, output)
	}

	// macOS: protoRoot may resolve through /private/...; do the same lookup.
	expected := protoRoot
	if r, err := filepath.EvalSymlinks(protoRoot); err == nil {
		expected = r
	}
	if !strings.Contains(strings.TrimSpace(output), strings.TrimSpace(expected)) {
		t.Errorf("expected pwd output to contain %q, got %q", expected, output)
	}
}

// TestExecRunnerAt_WorkDirIsExplicit verifies ExecRunnerAt overrides protoRoot.
func TestExecRunnerAt_WorkDirIsExplicit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell semantics")
	}
	protoRoot := t.TempDir()
	moduleRoot := t.TempDir()
	genRoot := t.TempDir()

	r := ExecRunnerAt("pwd", nil, moduleRoot, genRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, output, err := r(ctx, protoRoot, genRoot)
	if err != nil {
		t.Fatalf("pwd should succeed: %v", err)
	}
	expected := moduleRoot
	if r, err := filepath.EvalSymlinks(moduleRoot); err == nil {
		expected = r
	}
	if !strings.Contains(strings.TrimSpace(output), strings.TrimSpace(expected)) {
		t.Errorf("expected pwd output to contain moduleRoot %q, got %q", expected, output)
	}
}

// TestExecRunner_CollectsGeneratedFiles checks the genRoot walk picks up
// files produced by the command.
func TestExecRunner_CollectsGeneratedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell semantics")
	}
	protoRoot := t.TempDir()
	genRoot := t.TempDir()

	// Pre-populate genRoot since our "tool" is just /bin/true.
	if err := os.MkdirAll(filepath.Join(genRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genRoot, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genRoot, "sub", "b.txt"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}

	r := ExecRunner("true", nil, genRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	files, _, err := r(ctx, protoRoot, genRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 files (a.txt, sub/b.txt), got %d: %+v", len(files), files)
	}
}
