package wireagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/codegen"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/ownership"
)

func TestIsWirePackage_RecognizesBuildTag(t *testing.T) {
	dir := t.TempDir()
	if IsWirePackage(dir) {
		t.Error("empty dir should not be wire")
	}

	os.WriteFile(filepath.Join(dir, "wire.go"), []byte(`//go:build wireinject

package x
`), 0644)
	if !IsWirePackage(dir) {
		t.Error("dir with //go:build wireinject should be wire")
	}

	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "wire.go"), []byte(`// +build wireinject

package y
`), 0644)
	if !IsWirePackage(dir2) {
		t.Error("dir with legacy +build wireinject should be wire")
	}

	dir3 := t.TempDir()
	os.WriteFile(filepath.Join(dir3, "x.go"), []byte("package z\n"), 0644)
	if IsWirePackage(dir3) {
		t.Error("plain go dir should not be wire")
	}
}

func TestDiscoverWirePackages(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "nested", "b")
	c := filepath.Join(root, "c")
	for _, d := range []string{a, b, c} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(a, "wire.go"), []byte("//go:build wireinject\npackage a\n"), 0644)
	os.WriteFile(filepath.Join(b, "wire.go"), []byte("//go:build wireinject\npackage b\n"), 0644)
	os.WriteFile(filepath.Join(c, "x.go"), []byte("package c\n"), 0644)

	pkgs, err := DiscoverWirePackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d wire pkgs, want 2: %v", len(pkgs), pkgs)
	}
}

func TestProject_HealthyAndUnhealthy(t *testing.T) {
	p := Project("wire-x")

	f := p(Cfg{PkgDir: "/x/y"}, codegen.RunResult{Output: "ok"}, []string{"/x/y/wire_gen.go"}, nil)
	if !f.OK {
		t.Error("healthy projection should be OK")
	}
	if f.AgentID != "wire-x" || f.PkgDir != "/x/y" {
		t.Errorf("projection wrong: %+v", f)
	}

	f2 := p(Cfg{PkgDir: "/x/y"}, codegen.RunResult{Output: "boom"}, nil, errAny("boom"))
	if f2.OK {
		t.Error("unhealthy projection should be OK=false")
	}
}

type errAny string

func (e errAny) Error() string { return string(e) }

// fakeRunner pretends to be wire — produces a wire_gen.go bytes blob.
func fakeRunner(content []byte) codegen.Runner[Cfg] {
	return func(ctx context.Context, cfg Cfg) (codegen.RunResult, error) {
		path := filepath.Join(cfg.PkgDir, "wire_gen.go")
		return codegen.RunResult{
			Files:  []codegen.GeneratedFile{{Path: path, Bytes: content}},
			Output: "ok",
		}, nil
	}
}

// TestAgent_FakeRunner_WritesWireGen drives codegen.Agent end-to-end with
// a fake wire runner, asserting diff-write + ownership semantics hold.
func TestAgent_FakeRunner_WritesWireGen(t *testing.T) {
	pkgDir := t.TempDir()
	root := filepath.Dir(pkgDir)

	board := blackboard.New[Fact](blackboard.Config{})
	reg := ownership.New()

	cfg := codegen.Config[Cfg, Fact]{
		AgentID:  "wire-test",
		Cfg:      Cfg{PkgDir: pkgDir, ModuleRoot: root},
		Runner:   fakeRunner([]byte("// wire_gen content\n")),
		Project:  Project("wire-test"),
		Board:    board,
		BoardKey: func(f Fact) string { return Key(f.PkgDir) },
		Owner:    reg,
	}

	w := codegen.BuildReactiveWorker(cfg)
	prog := w.Handle(codegen.Trigger{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatal(err)
	}

	got := filepath.Join(pkgDir, "wire_gen.go")
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("wire_gen.go not written: %v", err)
	}
	if string(body) != "// wire_gen content\n" {
		t.Errorf("body = %q", body)
	}
	if !fact.OK {
		t.Error("fact should be OK")
	}
	if owner, owned := reg.Owner(got); !owned || owner != "wire-test" {
		t.Errorf("ownership = %q,%v, want wire-test,true", owner, owned)
	}
}

// TestAgent_FakeRunner_DiffWriteSkipsUnchanged verifies the cascade-breaker
// works for the wire agent: re-running with identical content produces no
// disk write and an empty ChangedFiles list.
func TestAgent_FakeRunner_DiffWriteSkipsUnchanged(t *testing.T) {
	pkgDir := t.TempDir()
	body := []byte("// wire_gen body\n")

	// Pre-populate.
	os.WriteFile(filepath.Join(pkgDir, "wire_gen.go"), body, 0644)

	cfg := codegen.Config[Cfg, Fact]{
		AgentID:  "wire-test",
		Cfg:      Cfg{PkgDir: pkgDir, ModuleRoot: filepath.Dir(pkgDir)},
		Runner:   fakeRunner(body),
		Project:  Project("wire-test"),
		BoardKey: func(f Fact) string { return Key(f.PkgDir) },
	}
	w := codegen.BuildReactiveWorker(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	fact, err := freeap.Run(ctx, w.Handle(codegen.Trigger{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(fact.ChangedFiles) != 0 {
		t.Errorf("expected 0 changed files, got %v", fact.ChangedFiles)
	}
}
