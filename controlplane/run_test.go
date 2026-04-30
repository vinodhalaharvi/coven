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

// Tag-related tests.

func TestTagAgentCommits_SinglesCommit_Amends(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Make one commit on the agent's branch.
	_ = makeFileChange(t, wt.Path, "foo.go", "package main\n", "agent did work")

	// Tag.
	if err := tagAgentCommits(context.Background(), wt.Path, "main"); err != nil {
		t.Fatalf("tagAgentCommits: %v", err)
	}

	// Verify the commit subject was prefixed.
	out, err := runGitCmdOutput(context.Background(), wt.Path, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatal(err)
	}
	subject := strings.TrimSpace(out)
	if !strings.HasPrefix(subject, covenCommitPrefix) {
		t.Errorf("subject not prefixed: %q", subject)
	}
	if !strings.Contains(subject, "agent did work") {
		t.Errorf("original message lost: %q", subject)
	}
}

func TestTagAgentCommits_AlreadyTagged_NoOp(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Make a pre-tagged commit.
	_ = makeFileChange(t, wt.Path, "foo.go", "package main\n", covenCommitPrefix+"already tagged")

	// Tag again (should be idempotent).
	if err := tagAgentCommits(context.Background(), wt.Path, "main"); err != nil {
		t.Fatal(err)
	}

	// Verify subject is unchanged (not double-prefixed).
	out, _ := runGitCmdOutput(context.Background(), wt.Path, "log", "-1", "--format=%s")
	subject := strings.TrimSpace(out)
	expectedPrefix := covenCommitPrefix
	if strings.HasPrefix(subject, expectedPrefix+expectedPrefix) {
		t.Errorf("commit was double-tagged: %q", subject)
	}
	if !strings.HasPrefix(subject, expectedPrefix) {
		t.Errorf("tag lost on idempotent retag: %q", subject)
	}
}

func TestTagAgentCommits_MultipleCommits_AllTagged(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	// Make three commits in sequence.
	_ = makeFileChange(t, wt.Path, "a.go", "package main\n", "first commit")
	_ = makeFileChange(t, wt.Path, "b.go", "package main\n", "second commit")
	_ = makeFileChange(t, wt.Path, "c.go", "package main\n", "third commit")

	if err := tagAgentCommits(context.Background(), wt.Path, "main"); err != nil {
		t.Fatalf("tagAgentCommits: %v", err)
	}

	// Verify each commit is prefixed.
	out, err := runGitCmdOutput(context.Background(), wt.Path, "log", "main..HEAD", "--format=%s")
	if err != nil {
		t.Fatal(err)
	}
	subjects := strings.Split(strings.TrimSpace(out), "\n")
	if len(subjects) != 3 {
		t.Fatalf("expected 3 commits, got %d:\n%s", len(subjects), out)
	}
	for _, s := range subjects {
		if !strings.HasPrefix(s, covenCommitPrefix) {
			t.Errorf("commit not tagged: %q", s)
		}
	}

	// Verify original messages preserved.
	allMsgs := strings.Join(subjects, "|")
	for _, original := range []string{"first commit", "second commit", "third commit"} {
		if !strings.Contains(allMsgs, original) {
			t.Errorf("original message %q lost: %s", original, allMsgs)
		}
	}
}

func TestAllCommitsAreCovenTagged_AllTagged_ReturnsTrue(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	mainSHA, _ := runGitCmdOutput(context.Background(), repo, "rev-parse", "main")
	mainSHA = strings.TrimSpace(mainSHA)

	_ = makeFileChange(t, wt.Path, "a.go", "package main\n", covenCommitPrefix+"first")
	_ = makeFileChange(t, wt.Path, "b.go", "package main\n", covenCommitPrefix+"second")
	endSHA, _ := runGitCmdOutput(context.Background(), wt.Path, "rev-parse", "HEAD")
	endSHA = strings.TrimSpace(endSHA)

	tagged, err := allCommitsAreCovenTagged(context.Background(), repo, mainSHA, endSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !tagged {
		t.Error("all-tagged range should return true")
	}
}

func TestAllCommitsAreCovenTagged_MixedRange_ReturnsFalse(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewWorktreeMgr(repo)
	wt, err := mgr.Provision(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Cleanup(context.Background(), wt)

	mainSHA, _ := runGitCmdOutput(context.Background(), repo, "rev-parse", "main")
	mainSHA = strings.TrimSpace(mainSHA)

	_ = makeFileChange(t, wt.Path, "a.go", "package main\n", covenCommitPrefix+"agent commit")
	_ = makeFileChange(t, wt.Path, "b.go", "package main\n", "user commit")
	endSHA, _ := runGitCmdOutput(context.Background(), wt.Path, "rev-parse", "HEAD")
	endSHA = strings.TrimSpace(endSHA)

	tagged, err := allCommitsAreCovenTagged(context.Background(), repo, mainSHA, endSHA)
	if err != nil {
		t.Fatal(err)
	}
	if tagged {
		t.Error("mixed range should return false (presence of any non-tagged commit means route)")
	}
}

func TestAllCommitsAreCovenTagged_EmptyRange_ReturnsFalse(t *testing.T) {
	repo := initTestRepo(t)
	mainSHA, _ := runGitCmdOutput(context.Background(), repo, "rev-parse", "main")
	mainSHA = strings.TrimSpace(mainSHA)

	tagged, err := allCommitsAreCovenTagged(context.Background(), repo, mainSHA, mainSHA)
	if err != nil {
		t.Fatal(err)
	}
	if tagged {
		t.Error("empty range should return false (don't trigger skip on empty diff)")
	}
}

// End-to-end: polling skips coven-tagged commits.
func TestPollLoop_SkipsCovenTaggedCommits(t *testing.T) {
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
		PollInterval: 50 * time.Millisecond,
	}
	cp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- cp.Run(ctx) }()

	// Let it start.
	time.Sleep(200 * time.Millisecond)

	// Make a coven-tagged commit on main.
	mainGo := filepath.Join(repo, "main.go")
	currentContent, _ := os.ReadFile(mainGo)
	newContent := string(currentContent) + "\n// coven did this\n"
	if err := os.WriteFile(mainGo, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", covenCommitPrefix + "agent fixed something"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}

	// Wait for poll cycles.
	time.Sleep(800 * time.Millisecond)
	cancel()
	<-runDone

	routerMu.Lock()
	calls := routerCallCount
	routerMu.Unlock()

	if calls != 0 {
		t.Errorf("router was called %d times for a coven-tagged commit (should skip)", calls)
	}
}

func TestPollLoop_DoesNotSkipUserCommit(t *testing.T) {
	repo := initGoProject(t)

	registry.Register(registry.AgentSpec{
		Name: "alpha", Role: "x", TypicalTriggers: "a", DomainFiles: "*", Description: "t",
	})
	t.Cleanup(func() { registry.Reset() })

	routerCalled := make(chan bool, 1)
	sender := func(ctx context.Context, system string, msgs []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		select {
		case routerCalled <- true:
		default:
		}
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
	cp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- cp.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	// Make a user (non-tagged) commit on main.
	mainGo := filepath.Join(repo, "main.go")
	currentContent, _ := os.ReadFile(mainGo)
	if err := os.WriteFile(mainGo, []byte(string(currentContent)+"\n// user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "user did this"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}

	select {
	case <-routerCalled:
		// good
	case <-time.After(2 * time.Second):
		t.Error("router was not called for a user commit (should NOT skip)")
	}

	cancel()
	<-runDone
}

// TestPollLoop_CallsIntentPromptAndPassesIntentToRouter verifies
// the full wiring: when a new commit is detected, IntentPrompt is
// called, and the user's intent reaches the router's LLM prompt.
func TestPollLoop_CallsIntentPromptAndPassesIntentToRouter(t *testing.T) {
	repo := initGoProject(t)

	registry.Register(registry.AgentSpec{
		Name: "alpha", Role: "x", TypicalTriggers: "a", DomainFiles: "*", Description: "t",
	})
	t.Cleanup(func() { registry.Reset() })

	const userIntent = "I deleted Multiply by mistake; please restore it"

	var mu sync.Mutex
	intentCalls := 0
	intentPrompt := func(ctx context.Context, summary string) string {
		mu.Lock()
		intentCalls++
		mu.Unlock()
		return userIntent
	}

	var capturedRouterPrompt string
	sender := func(ctx context.Context, system string, conv []llm.Message, tools []llm.ToolSpec) (llm.Message, llm.StopReason, error) {
		mu.Lock()
		for _, m := range conv {
			if m.Role == llm.RoleUser {
				for _, b := range m.Blocks {
					capturedRouterPrompt += b.Text
				}
			}
		}
		mu.Unlock()
		return llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Text: `{"agents":[],"reasoning":"no work"}`}},
		}, llm.StopEndTurn, nil
	}

	cfg := Config{
		ProjectRoot:  repo,
		Sender:       sender,
		Confirm:      alwaysConfirm,
		IntentPrompt: intentPrompt,
		Print:        func(string) {},
		PollInterval: 50 * time.Millisecond,
	}
	cp := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- cp.Run(ctx) }()

	// Let it start, then make a user commit.
	time.Sleep(200 * time.Millisecond)
	mainGo := filepath.Join(repo, "main.go")
	currentContent, _ := os.ReadFile(mainGo)
	if err := os.WriteFile(mainGo, []byte(string(currentContent)+"\n// trigger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "user trigger"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}

	// Wait for the router to be called.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := capturedRouterPrompt
		mu.Unlock()
		if got != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-runDone

	mu.Lock()
	finalCalls := intentCalls
	finalPrompt := capturedRouterPrompt
	mu.Unlock()

	if finalCalls == 0 {
		t.Error("IntentPrompt was never called")
	}
	if !strings.Contains(finalPrompt, "deleted Multiply by mistake") {
		t.Errorf("router prompt missing the user's intent:\n%s", finalPrompt)
	}
	if !strings.Contains(finalPrompt, "USER'S INTENT") {
		t.Errorf("router prompt missing intent header:\n%s", finalPrompt)
	}
}
