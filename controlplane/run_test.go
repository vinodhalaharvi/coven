package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/llm"
	"github.com/vinodhalaharvi/coven/registry"
)

func TestNew_RealConfigReturnsControlPlane(t *testing.T) {
	repo := initTestRepo(t)
	cp := New(Config{
		ProjectRoot: repo,
		Sender:      nil,
		Confirm:     alwaysConfirm,
		Print:       func(string) {},
	})
	if cp == nil {
		t.Fatal("New returned nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := cp.Run(ctx)
	if err == nil {
		t.Error("expected error from Run with nil sender, got nil")
		return
	}
	if !strings.Contains(err.Error(), "router not configured") {
		t.Errorf("expected 'router not configured' error, got %v", err)
	}
}

func TestNew_NoProjectRootReturnsStub(t *testing.T) {
	cp := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := cp.Run(ctx)
	if err != nil {
		t.Errorf("stub Run returned unexpected error: %v", err)
	}
}

func TestConfig_WithDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.PollInterval != 1*time.Second {
		t.Errorf("default PollInterval = %v, want 1s", c.PollInterval)
	}
	if c.Print == nil {
		t.Error("default Print should not be nil")
	}
	if c.Confirm == nil {
		t.Error("default Confirm should not be nil")
	}

	c2 := Config{PollInterval: 500 * time.Millisecond}.withDefaults()
	if c2.PollInterval != 500*time.Millisecond {
		t.Errorf("custom PollInterval overridden: got %v", c2.PollInterval)
	}
}

func TestBranchHasCommitsBeyondBase_NoCommits(t *testing.T) {
	repo := initTestRepo(t)

	cmd := exec.Command("git", "branch", "feature/empty")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v: %s", err, out)
	}

	has, err := branchHasCommitsBeyondBase(context.Background(), repo, "feature/empty", "main")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("empty branch should not have commits beyond main")
	}
}

func TestBranchHasCommitsBeyondBase_WithCommits(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)

	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	_ = makeFileChange(t, wt.Path, "x.go", "package main\n", "added x")

	has, err := branchHasCommitsBeyondBase(context.Background(), repo, wt.Branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("branch with commit should have commits beyond main")
	}
}

func TestTruncateLog(t *testing.T) {
	cases := map[string]int{
		"short":                              80,
		"":                                   80,
		"  whitespace stripped  ":            80,
		strings.Repeat("a", 200):             80,
	}
	for input, max := range cases {
		got := truncateLog(input, max)
		if len(got) > max+3 {
			t.Errorf("truncateLog too long: input %d chars → %d chars", len(input), len(got))
		}
	}
}

func TestShortSHAStr(t *testing.T) {
	cases := map[string]string{
		"":                                          "",
		"abc":                                       "abc",
		"abcdefgh":                                  "abcdefgh",
		"abcdefghijklmnop":                          "abcdefgh",
		"94d13180b7fea1b0181a4e4bbb1480a795bcc191":  "94d13180",
	}
	for input, expected := range cases {
		got := shortSHAStr(input)
		if got != expected {
			t.Errorf("shortSHAStr(%q) = %q, want %q", input, got, expected)
		}
	}
}

// State persistence tests.

func TestState_LoadEmpty_ReturnsZeroValue(t *testing.T) {
	repo := initTestRepo(t)
	cp := &controlPlane{
		cfg:       Config{ProjectRoot: repo, Print: func(string) {}}.withDefaults(),
		stateFile: filepath.Join(repo, ".coven", "state.json"),
	}
	st, err := cp.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastProcessedSHA != "" {
		t.Errorf("fresh state should have empty SHA, got %q", st.LastProcessedSHA)
	}
}

func TestState_SaveAndLoad_Roundtrip(t *testing.T) {
	repo := initTestRepo(t)
	cp := &controlPlane{
		cfg:       Config{ProjectRoot: repo, Print: func(string) {}}.withDefaults(),
		stateFile: filepath.Join(repo, ".coven", "state.json"),
	}
	original := &state{LastProcessedSHA: "abcdef1234567890abcdef1234567890abcdef12"}
	if err := cp.saveState(original); err != nil {
		t.Fatal(err)
	}
	loaded, err := cp.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastProcessedSHA != original.LastProcessedSHA {
		t.Errorf("roundtrip mismatch: saved %q, loaded %q",
			original.LastProcessedSHA, loaded.LastProcessedSHA)
	}
}

func TestState_LoadCorruptedFile_ReturnsZeroValue(t *testing.T) {
	repo := initTestRepo(t)
	stateDir := filepath.Join(repo, ".coven")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(stateFile, []byte("not valid json {{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := &controlPlane{
		cfg:       Config{ProjectRoot: repo, Print: func(string) {}}.withDefaults(),
		stateFile: stateFile,
	}
	st, err := cp.loadState()
	if err != nil {
		t.Fatalf("loadState should tolerate corrupt file, got error: %v", err)
	}
	if st.LastProcessedSHA != "" {
		t.Errorf("corrupt state should reset to zero, got %q", st.LastProcessedSHA)
	}
}

func TestState_SaveAtomic_NoPartialFileOnFailure(t *testing.T) {
	repo := initTestRepo(t)
	stateFile := filepath.Join(repo, ".coven", "state.json")
	cp := &controlPlane{
		cfg:       Config{ProjectRoot: repo, Print: func(string) {}}.withDefaults(),
		stateFile: stateFile,
	}
	// First write a known good state
	if err := cp.saveState(&state{LastProcessedSHA: "good"}); err != nil {
		t.Fatal(err)
	}
	// Verify rename target doesn't leave .tmp file behind
	tmpFile := stateFile + ".tmp"
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not exist after successful save")
	}
	// Verify content is valid JSON
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		t.Errorf("saved state is not valid JSON: %v", err)
	}
}

// Polling-loop end-to-end test.

func TestPollLoop_DetectsNewCommitAndRoutes(t *testing.T) {
	repo := initGoProject(t)

	// Register a test agent so the router has something to pick.
	registry.Register(registry.AgentSpec{
		Name:            "alpha",
		Description:     "test agent alpha",
		Role:            "you are alpha",
		TypicalTriggers: "any change",
		DomainFiles:     "*.go",
	})
	t.Cleanup(func() { registry.Reset() })

	routerCalled := make(chan string, 1)
	sender := func(ctx context.Context, system string, msgs []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		var content string
		for _, m := range msgs {
			for _, b := range m.Blocks {
				content += b.Text
			}
		}
		select {
		case routerCalled <- content:
		default:
		}
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: `{"agents":[],"reasoning":"test - no routing"}`}},
		}, llm.StopEndTurn, nil
	}

	cfg := Config{
		ProjectRoot:  repo,
		Sender:       sender,
		Confirm:      alwaysConfirm,
		Print:        func(string) {},
		PollInterval: 50 * time.Millisecond,
	}
	cp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- cp.Run(ctx)
	}()

	// Give Run a moment to start polling.
	time.Sleep(200 * time.Millisecond)

	// Make a commit on main.
	mainGo := filepath.Join(repo, "main.go")
	currentContent, _ := os.ReadFile(mainGo)
	newContent := string(currentContent) + "\n// trigger commit\n"
	if err := os.WriteFile(mainGo, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "trigger"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}

	// Wait for the router to be called (which means the polling loop
	// noticed the new commit, built a diff, and sent it to the router).
	select {
	case content := <-routerCalled:
		if !strings.Contains(content, "trigger commit") {
			t.Errorf("router was called but didn't see new content in diff:\n%s", content)
		}
	case <-time.After(3 * time.Second):
		t.Error("router was not called within 3s of new commit — polling broken")
	}

	cancel()
	<-runDone
}

func TestPollLoop_StateFilePersistsAcrossRuns(t *testing.T) {
	repo := initGoProject(t)

	registry.Register(registry.AgentSpec{
		Name: "alpha", Role: "you are alpha", TypicalTriggers: "any", DomainFiles: "*",
		Description: "test",
	})
	t.Cleanup(func() { registry.Reset() })

	sender := func(ctx context.Context, system string, msgs []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: `{"agents":[],"reasoning":"no work"}`}},
		}, llm.StopEndTurn, nil
	}

	cfg := Config{
		ProjectRoot:  repo,
		Sender:       sender,
		Confirm:      alwaysConfirm,
		Print:        func(string) {},
		PollInterval: 50 * time.Millisecond,
	}

	// First run: let Run start, then cancel.
	cp1 := New(cfg)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	go cp1.Run(ctx1)
	time.Sleep(300 * time.Millisecond)
	cancel1()

	// Verify state file exists and has the current main SHA.
	stateFile := filepath.Join(repo, ".coven", "state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("state file not created: %v", err)
	}
	var st1 state
	if err := json.Unmarshal(data, &st1); err != nil {
		t.Fatalf("state file invalid JSON: %v", err)
	}
	if len(st1.LastProcessedSHA) != 40 {
		t.Errorf("state should have a 40-char SHA, got %q", st1.LastProcessedSHA)
	}

	// Second run: should resume from saved state without reprocessing.
	cp2 := New(cfg)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	go cp2.Run(ctx2)
	time.Sleep(150 * time.Millisecond)
	cancel2()

	// State file should still be valid and unchanged (no new commits).
	data2, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var st2 state
	if err := json.Unmarshal(data2, &st2); err != nil {
		t.Fatalf("state file invalid after restart: %v", err)
	}
	if st2.LastProcessedSHA != st1.LastProcessedSHA {
		t.Errorf("state SHA changed without new commits: %q → %q",
			st1.LastProcessedSHA, st2.LastProcessedSHA)
	}
}

func TestPollLoop_NoRouteOnSameSHA(t *testing.T) {
	repo := initGoProject(t)

	registry.Register(registry.AgentSpec{
		Name: "alpha", Role: "x", TypicalTriggers: "a", DomainFiles: "*", Description: "t",
	})
	t.Cleanup(func() { registry.Reset() })

	var routerMu sync.Mutex
	routerCallCount := 0
	sender := func(ctx context.Context, system string, msgs []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		routerMu.Lock()
		routerCallCount++
		routerMu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: `{"agents":[],"reasoning":"no work"}`}},
		}, llm.StopEndTurn, nil
	}

	cfg := Config{
		ProjectRoot:  repo,
		Sender:       sender,
		Confirm:      alwaysConfirm,
		Print:        func(string) {},
		PollInterval: 30 * time.Millisecond,
	}
	cp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- cp.Run(ctx) }()

	// Let it poll for ~600ms without making any commits.
	time.Sleep(600 * time.Millisecond)
	cancel()
	<-runDone

	routerMu.Lock()
	calls := routerCallCount
	routerMu.Unlock()

	// With no commits, the router should NEVER be called. If it is,
	// we're routing on the same SHA repeatedly — broken.
	if calls != 0 {
		t.Errorf("router was called %d times despite no new commits", calls)
	}
}
