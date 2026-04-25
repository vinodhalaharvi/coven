package integration

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
	"github.com/vinodhalaharvi/coven/buildhealth"
	"github.com/vinodhalaharvi/coven/codegen"
	"github.com/vinodhalaharvi/coven/fsmonitor"
	"github.com/vinodhalaharvi/coven/ownership"
	"github.com/vinodhalaharvi/coven/sqlcagent"
	"github.com/vinodhalaharvi/coven/wireagent"
	"github.com/vinodhalaharvi/coven/worker"
)

// TestCrossToolDrift_CaughtByBuildHealth proves the architectural claim:
// when two codegen tools produce code that compiles individually but
// disagrees with a hand-written consumer, BuildHealthAgent's module-wide
// `go build ./...` surfaces the drift via a parsed BuildHealthFact.
//
// Scenario:
//   - sqlc-equivalent fake: emits db package with type `User { Email string }`
//   - wire-equivalent fake: emits binding code that references db.User
//   - hand-written consumer code references db.User.EmailAddress (drift!)
//
// Without BuildHealthAgent, no agent would surface this — both codegen
// agents are individually healthy. With BuildHealthAgent, `go build ./...`
// runs at module root, fails, and posts an unhealthy BuildHealthFact for
// the consumer package.
func TestCrossToolDrift_CaughtByBuildHealth(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	// Layout:
	//   <root>/go.mod
	//   <root>/db/         (sqlc-generated)
	//   <root>/wire-pkg/   (wire-generated)
	//   <root>/consumer/   (hand-written, references db.User.EmailAddress)
	mustWrite(t, filepath.Join(root, "go.mod"), "module drift.test\n\ngo 1.22\n")

	dbDir := filepath.Join(root, "db")
	wireDir := filepath.Join(root, "wirepkg")
	consumerDir := filepath.Join(root, "consumer")
	for _, d := range []string{dbDir, wireDir, consumerDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// sqlc emits: type User { Email string }
	mustWrite(t, filepath.Join(dbDir, "models.go"), `package db

type User struct {
	Email string
}
`)
	// wire emits: returns a *db.User from injector
	mustWrite(t, filepath.Join(wireDir, "wire_gen.go"), `package wirepkg

import "drift.test/db"

func InjectUser() *db.User { return &db.User{} }
`)
	// consumer references EmailAddress — DRIFT.
	mustWrite(t, filepath.Join(consumerDir, "consumer.go"), `package consumer

import "drift.test/db"

func GetEmail(u *db.User) string {
	return u.EmailAddress  // drift! db.User has Email, not EmailAddress
}
`)

	// Boards.
	fsBoard := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{
		QuietFor: 500 * time.Millisecond, Rounds: 2,
	})
	buildBoard := blackboard.New[buildhealth.BuildHealthFact](blackboard.Config{
		QuietFor: 500 * time.Millisecond, Rounds: 2,
	})

	pkgDirs := map[worker.PackageID]string{
		"db":       dbDir,
		"wirepkg":  wireDir,
		"consumer": consumerDir,
	}

	// BuildHealthAgent.
	buildSup := supervisor.New[buildhealth.Trigger, []buildhealth.BuildHealthFact](supervisor.Config{
		Name: "build-sup", Logger: quietLogger(),
	})
	buildSup.Attach(buildhealth.BuildReactiveWorker(buildhealth.Config{
		AgentID:    "buildhealth",
		ModuleRoot: root,
		Packages:   pkgDirs,
		FSBoard:    fsBoard,
		BuildBoard: buildBoard,
		SettleFor:  300 * time.Millisecond,
	}))

	// fsmonitor.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		_ = fsmonitor.Run(ctx, fsmonitor.Config{
			Root:     root,
			Debounce: 80 * time.Millisecond,
			Board:    fsBoard,
		})
	}()
	done := make(chan struct{})
	go func() { buildSup.Run(ctx); close(done) }()
	go func() {
		for range buildSup.Reports() {
		}
	}()

	// Let watchers attach.
	time.Sleep(300 * time.Millisecond)

	// Touch a file to wake the chain — modify consumer.go.
	mustWrite(t, filepath.Join(consumerDir, "consumer.go"), `package consumer

import "drift.test/db"

func GetEmail(u *db.User) string {
	return u.EmailAddress  // still drift (this rewrite triggers the cascade)
}
`)

	// Wait for BuildHealthAgent to report consumer as broken.
	if !waitFor(20*time.Second, func() bool {
		f, ok := buildBoard.Get(buildhealth.Key("consumer"))
		return ok && !f.Value.OK && len(f.Value.Errors) > 0
	}) {
		modFact, _ := buildBoard.Get(buildhealth.ModuleKey)
		f, _ := buildBoard.Get(buildhealth.Key("consumer"))
		t.Fatalf("consumer drift not caught;\n  module output: %q\n  consumer fact: %+v",
			modFact.Value.Output, f.Value)
	}

	// Verify db is healthy (it compiles fine on its own).
	dbFact, _ := buildBoard.Get(buildhealth.Key("db"))
	if !dbFact.Value.OK {
		t.Errorf("db should be healthy individually; got %+v", dbFact.Value.Errors)
	}

	// Now FIX the drift and verify recovery.
	mustWrite(t, filepath.Join(consumerDir, "consumer.go"), `package consumer

import "drift.test/db"

func GetEmail(u *db.User) string {
	return u.Email  // fixed
}
`)

	if !waitFor(20*time.Second, func() bool {
		f, ok := buildBoard.Get(buildhealth.Key("consumer"))
		return ok && f.Value.OK
	}) {
		f, _ := buildBoard.Get(buildhealth.Key("consumer"))
		t.Fatalf("consumer did not recover after fix; fact: %+v", f.Value)
	}

	cancel()
	<-done
}

// TestWireAgent_FactPostedOnFakeRun proves the wire agent's wiring
// against the central FileChangeFact stream.
func TestWireAgent_FactPostedOnFakeRun(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	pkgDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "go.mod"), "module wire.test\n\ngo 1.22\n")
	// Mark as a wire package.
	mustWrite(t, filepath.Join(pkgDir, "wire.go"), `//go:build wireinject

package auth
`)

	// Boards.
	fsBoard := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{})
	wireBoard := blackboard.New[wireagent.Fact](blackboard.Config{})
	reg := ownership.New()

	// Wire agent with FAKE runner (no real wire binary). We construct the
	// codegen.Config directly to override the runner.
	cfg := codegen.Config[wireagent.Cfg, wireagent.Fact]{
		AgentID: "wire:auth",
		Cfg: wireagent.Cfg{
			PkgDir:     pkgDir,
			ModuleRoot: root,
		},
		Runner: func(ctx context.Context, c wireagent.Cfg) (codegen.RunResult, error) {
			return codegen.RunResult{
				Files: []codegen.GeneratedFile{
					{
						Path:  filepath.Join(pkgDir, "wire_gen.go"),
						Bytes: []byte("// Code generated by Wire. DO NOT EDIT.\n\npackage auth\n"),
					},
				},
				Output: "ok",
			}, nil
		},
		Project:  wireagent.Project("wire:auth"),
		Board:    wireBoard,
		BoardKey: func(f wireagent.Fact) string { return wireagent.Key(f.PkgDir) },
		Owner:    reg,
		Source: func(ctx context.Context) (<-chan codegen.Trigger, error) {
			raw := fsmonitor.Subscribe(fsBoard, fsmonitor.FilesUnder(pkgDir))
			ch, err := raw(ctx)
			if err != nil {
				return nil, err
			}
			out := make(chan codegen.Trigger, 4)
			go func() {
				defer close(out)
				for {
					select {
					case <-ctx.Done():
						return
					case f, ok := <-ch:
						if !ok {
							return
						}
						select {
						case out <- codegen.Trigger{Reason: "fs", ChangedFiles: f.ChangedFiles, At: f.ChangedAt}:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
			return out, nil
		},
		HealthFromFact: func(f wireagent.Fact) bool { return f.OK },
	}

	w := codegen.BuildReactiveWorker(cfg)
	sup := supervisor.New[codegen.Trigger, wireagent.Fact](supervisor.Config{
		Name: "wire-sup", Logger: quietLogger(),
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
	time.Sleep(200 * time.Millisecond)

	// Simulate an FS event by posting a FileChangeFact directly to fsBoard
	// (skipping fsmonitor.Run since we just want to verify the agent
	// wiring without OS-level fsnotify timing).
	fsBoard.Post("fs:k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(pkgDir, "wire.go")},
		ChangedAt:    time.Now(),
	}, "test")

	if !waitFor(5*time.Second, func() bool {
		_, ok := wireBoard.Get(wireagent.Key(pkgDir))
		return ok
	}) {
		t.Fatal("wire fact never arrived")
	}
	f, _ := wireBoard.Get(wireagent.Key(pkgDir))
	if !f.Value.OK {
		t.Errorf("wire fact should be OK: %+v", f.Value)
	}
	// File should exist on disk.
	gen := filepath.Join(pkgDir, "wire_gen.go")
	if _, err := os.Stat(gen); err != nil {
		t.Errorf("wire_gen.go not written: %v", err)
	}
	// Ownership.
	if owner, owned := reg.Owner(gen); !owned || owner != "wire:auth" {
		t.Errorf("ownership = %q,%v", owner, owned)
	}
	cancel()
	<-done
}

// TestSqlcAgent_FilterMatchesSqlFiles proves sqlc's filter only fires on
// .sql or sqlc.yaml changes, not on other Go file edits.
func TestSqlcAgent_FilterMatchesSqlFiles(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	queryDir := filepath.Join(root, "queries")
	genDir := filepath.Join(root, "gen")
	for _, d := range []string{queryDir, genDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "sqlc.yaml"), "version: \"2\"\n")
	mustWrite(t, filepath.Join(root, "go.mod"), "module sqlc.test\n\ngo 1.22\n")

	fsBoard := blackboard.New[fsmonitor.FileChangeFact](blackboard.Config{})
	sqlcBoard := blackboard.New[sqlcagent.Fact](blackboard.Config{})
	reg := ownership.New()

	// Override Runner with fake.
	fake := func(ctx context.Context, c sqlcagent.Cfg) (codegen.RunResult, error) {
		return codegen.RunResult{
			Files: []codegen.GeneratedFile{
				{Path: filepath.Join(genDir, "queries.sql.go"), Bytes: []byte("package gen\n")},
			},
			Output: "ok",
		}, nil
	}

	cfg := codegen.Config[sqlcagent.Cfg, sqlcagent.Fact]{
		AgentID: "sqlc",
		Cfg: sqlcagent.Cfg{
			ModuleRoot: root,
			GenRoot:    genDir,
		},
		Runner:   fake,
		Project:  sqlcagent.Project("sqlc"),
		Board:    sqlcBoard,
		BoardKey: func(f sqlcagent.Fact) string { return sqlcagent.Key(f.GenRoot) },
		Owner:    reg,
		Source: func(ctx context.Context) (<-chan codegen.Trigger, error) {
			filter := func(f fsmonitor.FileChangeFact) bool {
				for _, p := range f.ChangedFiles {
					base := filepath.Base(p)
					if filepath.Ext(p) == ".sql" || base == "sqlc.yaml" {
						return true
					}
				}
				return false
			}
			raw := fsmonitor.Subscribe(fsBoard, filter)
			ch, err := raw(ctx)
			if err != nil {
				return nil, err
			}
			out := make(chan codegen.Trigger, 4)
			go func() {
				defer close(out)
				for {
					select {
					case <-ctx.Done():
						return
					case f, ok := <-ch:
						if !ok {
							return
						}
						select {
						case out <- codegen.Trigger{Reason: "fs", ChangedFiles: f.ChangedFiles, At: f.ChangedAt}:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
			return out, nil
		},
		HealthFromFact: func(f sqlcagent.Fact) bool { return f.OK },
	}

	w := codegen.BuildReactiveWorker(cfg)
	sup := supervisor.New[codegen.Trigger, sqlcagent.Fact](supervisor.Config{
		Name: "sqlc-sup", Logger: quietLogger(),
	})
	sup.Attach(w)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	go func() {
		for range sup.Reports() {
		}
	}()
	time.Sleep(200 * time.Millisecond)

	// Post a .go file change — sqlc should NOT fire.
	fsBoard.Post("fs:k1", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(root, "main.go")},
		ChangedAt:    time.Now(),
	}, "test")
	time.Sleep(300 * time.Millisecond)
	if _, ok := sqlcBoard.Get(sqlcagent.Key(genDir)); ok {
		t.Fatal("sqlc fired on a .go change; should only fire on .sql / sqlc.yaml")
	}

	// Now post a .sql change — sqlc SHOULD fire.
	fsBoard.Post("fs:k2", fsmonitor.FileChangeFact{
		ChangedFiles: []string{filepath.Join(queryDir, "users.sql")},
		ChangedAt:    time.Now(),
	}, "test")

	if !waitFor(3*time.Second, func() bool {
		_, ok := sqlcBoard.Get(sqlcagent.Key(genDir))
		return ok
	}) {
		t.Fatal("sqlc did not fire on .sql change")
	}
	cancel()
	<-done
}

// quietLogger is shared with cascade_test.go; redeclare locally if needed.
func quietLogger2() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
var _ = quietLogger2
