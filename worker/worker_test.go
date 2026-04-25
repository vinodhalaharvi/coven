package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestChecksumDir_Stable(t *testing.T) {
	dir := mustTestPkg(t)
	a, err := ChecksumDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ChecksumDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("checksum not stable: %q vs %q", a, b)
	}
}

func TestChecksumDir_ChangesOnEdit(t *testing.T) {
	dir := mustTempPkg(t, `package x
func Foo() int { return 1 }
`)
	a, _ := ChecksumDir(dir)
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(`package x
func Foo() int { return 2 }
`), 0644); err != nil {
		t.Fatal(err)
	}
	b, _ := ChecksumDir(dir)
	if a == b {
		t.Fatal("checksum should have changed after edit")
	}
}

func TestExtractExports(t *testing.T) {
	dir := mustTestPkg(t)
	syms, err := ExtractExports(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]string)
	for _, s := range syms {
		names[s.Name] = s.Kind
	}
	// Expect: Greeting (const), Name (var), Visitor (type), Greet (func).
	if names["Greeting"] != "const" {
		t.Errorf("Greeting kind = %q, want const", names["Greeting"])
	}
	if names["Name"] != "var" {
		t.Errorf("Name kind = %q, want var", names["Name"])
	}
	if names["Visitor"] != "type" {
		t.Errorf("Visitor kind = %q, want type", names["Visitor"])
	}
	if names["Greet"] != "func" {
		t.Errorf("Greet kind = %q, want func", names["Greet"])
	}
	if _, ok := names["unexportedHelper"]; ok {
		t.Error("unexportedHelper should not be in exports")
	}
}

func TestExtractImports(t *testing.T) {
	dir := mustTempPkg(t, `package x

import (
	"fmt"
	"strings"
	"github.com/example/foo"
)

func Use() { fmt.Println(strings.ToUpper("hi")); _ = foo.X }
`)
	imps, err := ExtractImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"fmt": true, "strings": true, "github.com/example/foo": true}
	got := map[string]bool{}
	for _, p := range imps {
		got[string(p)] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("missing import %q in %v", w, imps)
		}
	}
}

func TestExtractAll_BothInOnePass(t *testing.T) {
	dir := mustTempPkg(t, `package x

import "fmt"

func Hello() string { return fmt.Sprint("hi") }
type Greeter struct{}
`)
	syms, imps, err := ExtractAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) < 2 {
		t.Errorf("syms = %+v, want at least 2", syms)
	}
	if len(imps) < 1 || string(imps[0]) != "fmt" {
		t.Errorf("imps = %v, want [fmt]", imps)
	}
}

func TestRunGoBuild_Success(t *testing.T) {
	dir := mustTempModule(t, `package x
func Foo() int { return 1 }
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := RunGoBuild(ctx, dir)
	if !r.OK {
		t.Fatalf("build failed: %s", r.Output)
	}
}

func TestRunGoBuild_Failure(t *testing.T) {
	dir := mustTempModule(t, `package x
func Foo() int { return "not an int" }
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := RunGoBuild(ctx, dir)
	if r.OK {
		t.Fatal("build should have failed")
	}
	if r.Output == "" {
		t.Error("expected error output")
	}
}

func TestFSSource_Debounces(t *testing.T) {
	dir := mustTempPkg(t, "package x\n")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	src := FSSource(FSConfig{Dir: dir, Debounce: 100*time.Millisecond, Extensions: []string{".go"}})
	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Fire 5 writes quickly. Should collapse to one debounced event.
	for i := 0; i < 5; i++ {
		content := "package x\n// rev " + string(rune('a'+i)) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case ev := <-ch:
		if ev.RawCount < 2 {
			t.Errorf("expected multiple raw events collapsed, got %d", ev.RawCount)
		}
	case <-time.After(time.Second):
		t.Fatal("no debounced event received")
	}

	// Shouldn't fire a second event without more writes.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected second event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// OK, no spurious event.
	}
}

func TestFSSource_IgnoresNonGoFiles(t *testing.T) {
	dir := mustTempPkg(t, "package x\n")
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	src := FSSource(FSConfig{Dir: dir, Debounce: 50*time.Millisecond, Extensions: []string{".go"}})
	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Write a non-.go file — should not trigger.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event on non-Go file: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// Good.
	}
}

func TestFSSource_ChangedFilesPopulated(t *testing.T) {
	dir := mustTempPkg(t, "package x\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	src := FSSource(FSConfig{Dir: dir, Debounce: 80 * time.Millisecond, Extensions: []string{".go"}})
	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Write two distinct files; should appear in ChangedFiles.
	files := []string{"a.go", "b.go"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case ev := <-ch:
		if len(ev.ChangedFiles) < 2 {
			t.Errorf("ChangedFiles = %v, want at least 2", ev.ChangedFiles)
		}
		// Verify they're absolute paths inside dir.
		for _, p := range ev.ChangedFiles {
			if !strings.HasPrefix(p, dir) {
				t.Errorf("ChangedFile %q not under dir %q", p, dir)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestFSSource_MultipleExtensions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	src := FSSource(FSConfig{Dir: dir, Debounce: 60 * time.Millisecond, Extensions: []string{".go", ".proto"}})
	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "y.proto"), []byte("// proto\n"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if len(ev.ChangedFiles) == 0 {
			t.Fatal("no ChangedFiles in event")
		}
	case <-time.After(time.Second):
		t.Fatal("no event for .proto under multi-ext config")
	}
}

func TestFSSource_IgnoreFileHook(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "owned.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foreign.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	src := FSSource(FSConfig{
		Dir:        dir,
		Debounce:   60 * time.Millisecond,
		Extensions: []string{".go"},
		IgnoreFile: func(p string) bool { return strings.HasSuffix(p, "foreign.go") },
	})
	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Trigger only on the ignored file.
	if err := os.WriteFile(filepath.Join(dir, "foreign.go"), []byte("package x\n// edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("got event for ignored file: %+v", ev)
	case <-time.After(250 * time.Millisecond):
		// Good — ignored.
	}

	// Now edit the owned file; should fire.
	if err := os.WriteFile(filepath.Join(dir, "owned.go"), []byte("package x\n// edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		for _, p := range ev.ChangedFiles {
			if strings.HasSuffix(p, "foreign.go") {
				t.Errorf("ignored file leaked into ChangedFiles: %v", ev.ChangedFiles)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no event for owned file")
	}
}

func TestAgentProgram_Structure(t *testing.T) {
	// Verify the agent's Program has the expected static shape via analyzer.
	dir := mustTempModule(t, "package x\nfunc Foo() int { return 1 }\n")
	board := blackboard.New[PackageFact](blackboard.Config{})
	a := &agent{cfg: PackageAgentConfig{
		Pkg: "x", Dir: dir, Board: board,
	}}
	prog := a.handle(FSEvent{Dir: dir})

	// The top-level should be a flatmap (checksum → rest).
	d := prog.Describe()
	if d.Kind != "flatmap" {
		t.Fatalf("top-level should be flatmap, got %s", d.Kind)
	}
}

func TestAgentProgram_RunsEndToEnd(t *testing.T) {
	dir := mustTempModule(t, "package x\nfunc Foo() int { return 1 }\n")
	board := blackboard.New[PackageFact](blackboard.Config{})
	a := &agent{cfg: PackageAgentConfig{
		Pkg: "x", Dir: dir, Board: board,
	}}
	prog := a.handle(FSEvent{Dir: dir})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f, err := freeap.Run(ctx, prog)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !f.Healthy() {
		t.Fatalf("should be healthy; build=%+v vet=%+v", f.Build, f.Vet)
	}
	posted, ok := board.Get("pkg:x")
	if !ok {
		t.Fatal("fact not posted to blackboard")
	}
	if posted.Value.Checksum == "" {
		t.Error("checksum missing")
	}
	names := map[string]bool{}
	for _, s := range posted.Value.Exports {
		names[s.Name] = true
	}
	if !names["Foo"] {
		t.Errorf("Foo not in exports: %v", names)
	}
}

func TestFullPipeline_SupervisorDrivenAgent(t *testing.T) {
	dir := mustTempModule(t, "package x\nfunc Foo() int { return 1 }\n")
	board := blackboard.New[PackageFact](blackboard.Config{})

	w := BuildReactiveWorker(PackageAgentConfig{
		Pkg: "x", Dir: dir, Board: board,
	}, 80*time.Millisecond)

	sup := supervisor.New[FSEvent, PackageFact](supervisor.Config{
		Name: "test", Logger: quietLogger(),
	})
	sup.Attach(w)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	// Allow fsnotify watcher to attach.
	time.Sleep(200 * time.Millisecond)

	// Trigger a change.
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(
		"package x\nfunc Foo() int { return 2 }\nfunc Bar() string { return \"hi\" }\n",
	), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for a report.
	select {
	case r := <-sup.Reports():
		if !r.OK {
			t.Fatalf("report not healthy: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no report received")
	}

	// Blackboard should have a fact with Foo and Bar.
	posted, ok := board.Get("pkg:x")
	if !ok {
		t.Fatal("no fact on blackboard")
	}
	names := map[string]bool{}
	for _, s := range posted.Value.Exports {
		names[s.Name] = true
	}
	if !names["Foo"] || !names["Bar"] {
		t.Errorf("expected Foo and Bar, got %v", names)
	}

	cancel()
	<-done
}

// ─── test helpers ────────────────────────────────────────────────────────

func mustTestPkg(t *testing.T) string {
	t.Helper()
	// Copy of testdata/goodpkg into a temp dir to avoid mutating the source.
	src, err := filepath.Abs("../testdata/goodpkg")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// mustTempPkg creates a temp directory with a single x.go file. No go.mod —
// suitable only for AST / fsnotify tests that don't call go build.
func mustTempPkg(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mustTempModule creates a temp directory with go.mod + x.go. Suitable for
// running go build / go vet.
func mustTempModule(t *testing.T, xgoContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(
		"module example.test/x\n\ngo 1.22\n",
	), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(xgoContent), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// avoid-unused for the strings import in case tests get reordered
var _ = strings.HasSuffix
