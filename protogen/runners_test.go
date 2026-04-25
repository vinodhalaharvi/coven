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

	// On macOS /var is a symlink to /private/var, so pwd may report either
	// form depending on shell PWD propagation. Compare both forms canonicalized.
	if !pathsEquivalent(strings.TrimSpace(output), protoRoot) {
		t.Errorf("expected pwd output to be equivalent to protoRoot %q, got %q", protoRoot, output)
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
	if !pathsEquivalent(strings.TrimSpace(output), moduleRoot) {
		t.Errorf("expected pwd output to be equivalent to moduleRoot %q, got %q", moduleRoot, output)
	}
}

// pathsEquivalent reports whether two paths refer to the same location
// after resolving symlinks. Either side may be in unresolved form (e.g.
// /var/folders/...) or resolved form (/private/var/folders/...) on macOS.
func pathsEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return a == b
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
