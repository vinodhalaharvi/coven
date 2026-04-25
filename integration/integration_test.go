// Package integration runs end-to-end tests across the full stack:
// ensemble + supervisor + blackboard + fsnotify + multi-package agent fleet.
package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/ensemble"
	"github.com/vinodhalaharvi/coven/worker"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// copyMiniproject copies the testdata/miniproject fixture into a temp dir so
// tests can mutate files without affecting the source tree.
func copyMiniproject(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs("../testdata/miniproject")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// discoverPackages finds all dirs under root containing non-test .go files.
func discoverPackages(root string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if len(path) > 8 && path[len(path)-8:] == "_test.go" {
			return nil
		}
		seen[filepath.Dir(path)] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func TestMultiPackage_InitialBuildConverges(t *testing.T) {
	root := copyMiniproject(t)
	dirs, err := discoverPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 3 {
		t.Fatalf("expected at least 3 package dirs, got %d", len(dirs))
	}

	board := blackboard.New[worker.PackageFact](blackboard.Config{
		QuietFor: 300 * time.Millisecond,
		Rounds:   2,
	})
	sup := supervisor.New[worker.FSEvent, worker.PackageFact](supervisor.Config{
		Name:   "test",
		Logger: quietLogger(),
	})

	for _, d := range dirs {
		rel, _ := filepath.Rel(root, d)
		if rel == "." {
			rel = "root"
		}
		w := worker.BuildReactiveWorker(worker.PackageAgentConfig{
			Pkg:   worker.PackageID(rel),
			Dir:   d,
			Board: board,
		}, 80*time.Millisecond)
		sup.Attach(w)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	supDone := make(chan struct{})
	go func() { sup.Run(ctx); close(supDone) }()

	// Give the fsnotify watchers a moment to attach.
	time.Sleep(300 * time.Millisecond)

	// Touch every .go file so each agent sees a change and posts a fact.
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
				continue
			}
			path := filepath.Join(d, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Append a harmless comment to trigger a write event.
			if err := os.WriteFile(path, append(data, []byte("\n// touched\n")...), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Consume reports in a goroutine so the supervisor doesn't block.
	go func() {
		for range sup.Reports() {
		}
	}()

	// Wait until all agents have posted at least one fact — otherwise the
	// blackboard's "quiet" state is pre-activity silence, not post-activity
	// equilibrium, and the ensemble would falsely report converged.
	waitDeadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(waitDeadline) {
			t.Fatalf("only %d of %d agents posted initial fact", len(board.List("pkg:*")), len(dirs))
		}
		if len(board.List("pkg:*")) >= len(dirs) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Now set up the ensemble and wait for convergence (all agents healthy
	// + blackboard quiescent).
	ens := ensemble.New(ensemble.Config{
		Tick:        100 * time.Millisecond,
		Convergence: ensemble.All(),
	})
	ensemble.AttachSupervisor(ens, "org-chart", sup)
	ensemble.AttachBlackboard(ens, "buildgraph", board)

	snap, msg, err := ens.Await(ctx)
	if err != nil {
		t.Fatalf("ensemble did not converge: %v; last snap: %v", err, snap)
	}
	t.Logf("converged: %s; witnesses: %v", msg, snap)

	// Verify every package has a fact on the blackboard.
	facts := board.List("pkg:*")
	if len(facts) != len(dirs) {
		t.Errorf("want %d facts, got %d", len(dirs), len(facts))
	}
	for _, f := range facts {
		if !f.Value.Healthy() {
			t.Errorf("package %s not healthy: build=%+v vet=%+v",
				f.Value.Pkg, f.Value.Build, f.Value.Vet)
		}
	}

	cancel()
	<-supDone
}

func TestMultiPackage_DetectsBrokenPackage(t *testing.T) {
	root := copyMiniproject(t)

	board := blackboard.New[worker.PackageFact](blackboard.Config{
		QuietFor: 300 * time.Millisecond,
		Rounds:   2,
	})
	sup := supervisor.New[worker.FSEvent, worker.PackageFact](supervisor.Config{
		Name:   "test",
		Logger: quietLogger(),
	})

	dirs, _ := discoverPackages(root)
	for _, d := range dirs {
		rel, _ := filepath.Rel(root, d)
		if rel == "." {
			rel = "root"
		}
		w := worker.BuildReactiveWorker(worker.PackageAgentConfig{
			Pkg:   worker.PackageID(rel),
			Dir:   d,
			Board: board,
		}, 80*time.Millisecond)
		sup.Attach(w)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	supDone := make(chan struct{})
	go func() { sup.Run(ctx); close(supDone) }()
	go func() {
		for range sup.Reports() {
		}
	}()

	time.Sleep(300 * time.Millisecond)

	// Break the auth package specifically.
	authFile := filepath.Join(root, "auth", "auth.go")
	brokenSrc := []byte(`package auth

type Token string

func Validate(t Token) bool {
	return undefinedIdentifier  // build error
}
`)
	if err := os.WriteFile(authFile, brokenSrc, 0644); err != nil {
		t.Fatal(err)
	}

	// Poll blackboard until auth reports unhealthy.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			f, _ := board.Get("pkg:auth")
			t.Fatalf("auth never reported unhealthy; last fact: %+v", f.Value)
		default:
		}
		f, ok := board.Get("pkg:auth")
		if ok && !f.Value.Healthy() {
			if f.Value.Build.OK {
				t.Fatalf("auth reported healthy build despite broken code: %+v", f.Value.Build)
			}
			// Got the expected unhealthy signal.
			t.Logf("auth correctly reported unhealthy; build output preview: %q",
				truncate(f.Value.Build.Output, 120))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Now fix it and confirm it recovers to healthy.
	fixedSrc := []byte(`package auth

type Token string

func Validate(t Token) bool {
	return t != ""
}
`)
	if err := os.WriteFile(authFile, fixedSrc, 0644); err != nil {
		t.Fatal(err)
	}

	deadline = time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			f, _ := board.Get("pkg:auth")
			t.Fatalf("auth never recovered; last fact: %+v", f.Value)
		default:
		}
		f, ok := board.Get("pkg:auth")
		if ok && f.Value.Healthy() {
			t.Log("auth recovered to healthy")
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-supDone
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
