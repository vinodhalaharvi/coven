package diagnostic

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/llm"
)

// scriptedLLM returns a fake LLM that returns the given JSON string.
func scriptedLLM(json string) llm.LLM {
	return func(ctx context.Context, prompt string) (string, error) {
		return json, nil
	}
}

// recordingLLM captures the prompt for assertions.
func recordingLLM(json string, captured *string) llm.LLM {
	return func(ctx context.Context, prompt string) (string, error) {
		*captured = prompt
		return json, nil
	}
}

// out returns a printer that appends lines to a slice for assertions.
type sink struct {
	mu    sync.Mutex
	lines []string
}

func (s *sink) print(text string) {
	s.mu.Lock()
	s.lines = append(s.lines, text)
	s.mu.Unlock()
}

func (s *sink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "")
}

func TestObserve_AsksClaude_AndShowsProposal(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0644)

	s := &sink{}
	in := bufio.NewReader(strings.NewReader("n\n")) // user says no
	a := New(Config{
		ModuleRoot: root,
		LLM: scriptedLLM(`{
			"problem": "missing protobuf runtime",
			"command": "go get google.golang.org/protobuf",
			"why": "the generated .pb.go files import it but go.mod doesn't list it",
			"confidence": "high"
		}`),
		In:        in,
		Out:       s.print,
		DedupeFor: time.Millisecond,
	})

	a.Observe(context.Background(), UnhealthyFact{
		Source:    "buildhealth",
		Subject:   "gen/user/v1",
		ErrorText: `gen/user/v1/user.pb.go:7:8: cannot find module providing package google.golang.org/protobuf/runtime/protoimpl`,
		When:      time.Now(),
	})

	// Wait for the goroutine.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("diagnosis output never appeared. Got: %s", s.all())
		default:
		}
		if strings.Contains(s.all(), "missing protobuf runtime") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	out := s.all()
	if !strings.Contains(out, "go get google.golang.org/protobuf") {
		t.Errorf("missing proposal in output:\n%s", out)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("user said 'n' but skip wasn't shown:\n%s", out)
	}
}

func TestObserve_AutoRunExecutesCommand(t *testing.T) {
	root := t.TempDir()

	s := &sink{}
	in := bufio.NewReader(strings.NewReader(""))
	a := New(Config{
		ModuleRoot: root,
		// `touch` is a safe no-op that creates a file we can verify.
		LLM: scriptedLLM(`{
			"problem": "needs marker file",
			"command": "touch marker.txt",
			"why": "demo",
			"confidence": "high"
		}`),
		In:        in,
		Out:       s.print,
		AutoRun:   true,
		DedupeFor: time.Millisecond,
	})

	a.Observe(context.Background(), UnhealthyFact{
		Source: "test", Subject: "x", ErrorText: "demo",
	})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("command never ran. Output:\n%s", s.all())
		default:
		}
		if _, err := os.Stat(filepath.Join(root, "marker.txt")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !strings.Contains(s.all(), "OK") {
		t.Errorf("expected 'OK' in output:\n%s", s.all())
	}
}

func TestObserve_DedupeWithinWindow(t *testing.T) {
	root := t.TempDir()

	var calls int
	var mu sync.Mutex
	llmCounter := func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return `{"problem":"x","command":"","why":"","confidence":"low"}`, nil
	}

	s := &sink{}
	a := New(Config{
		ModuleRoot: root,
		LLM:       llmCounter,
		In:        bufio.NewReader(strings.NewReader("")),
		Out:       s.print,
		DedupeFor: 1 * time.Hour,
	})

	for i := 0; i < 5; i++ {
		a.Observe(context.Background(), UnhealthyFact{
			Source: "test", Subject: "same",
		})
	}
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 LLM call (dedupe), got %d", got)
	}
}

func TestObserve_DifferentSubjectsBothDiagnosed(t *testing.T) {
	root := t.TempDir()

	var calls int
	var mu sync.Mutex
	llmCounter := func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return `{"problem":"x","command":"","why":"","confidence":"low"}`, nil
	}

	s := &sink{}
	a := New(Config{
		ModuleRoot: root,
		LLM:       llmCounter,
		In:        bufio.NewReader(strings.NewReader("")),
		Out:       s.print,
		DedupeFor: 1 * time.Hour,
	})

	// Different subjects → both should be diagnosed.
	a.Observe(context.Background(), UnhealthyFact{Source: "buildhealth", Subject: "pkg-a"})
	// Wait for the first to finish before the second (they serialize).
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("first diagnosis didn't print")
		default:
		}
		mu.Lock()
		c := calls
		mu.Unlock()
		if c >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	a.Observe(context.Background(), UnhealthyFact{Source: "buildhealth", Subject: "pkg-b"})
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Errorf("expected 2 LLM calls, got %d", got)
	}
}

func TestBuildPrompt_IncludesErrorAndContext(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644)
	os.MkdirAll(filepath.Join(root, "proto", "user", "v1"), 0755)

	prompt := buildPrompt(root, UnhealthyFact{
		Source:    "buildhealth",
		Subject:   "gen/user/v1",
		ErrorText: "cannot find module providing package XYZ",
	})
	for _, must := range []string{
		"buildhealth",
		"gen/user/v1",
		"cannot find module providing package XYZ",
		"example.com/test",
		"proto/",
	} {
		if !strings.Contains(prompt, must) {
			t.Errorf("prompt missing %q:\n%s", must, prompt)
		}
	}
}

func TestObserve_NoCommandMeansNoExecution(t *testing.T) {
	root := t.TempDir()
	s := &sink{}
	a := New(Config{
		ModuleRoot: root,
		LLM: scriptedLLM(`{
			"problem": "needs human attention",
			"command": "",
			"why": "complex code change required",
			"confidence": "low"
		}`),
		In:        bufio.NewReader(strings.NewReader("")),
		Out:       s.print,
		AutoRun:   true,
		DedupeFor: time.Millisecond,
	})

	a.Observe(context.Background(), UnhealthyFact{Source: "test", Subject: "x"})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("never printed")
		default:
		}
		if strings.Contains(s.all(), "needs human attention") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !strings.Contains(s.all(), "needs human attention") {
		t.Errorf("expected human-attention message:\n%s", s.all())
	}
	if strings.Contains(s.all(), "running:") {
		t.Errorf("nothing should have run when command is empty:\n%s", s.all())
	}
}

// Verify the static llm package shape we depend on works as expected
// with structured calls — sanity test that doesn't hit the network.
func TestLLM_StaticForDocs(t *testing.T) {
	getDiag := llm.Structured[Diagnosis](
		llm.Static(`{"problem":"p","command":"c","why":"w","confidence":"high"}`),
		diagnosisSchema,
	)
	d, err := getDiag(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if d.Problem != "p" || d.Command != "c" || d.Confidence != "high" {
		t.Errorf("unexpected: %+v", d)
	}
}

// Sanity: ensure the snapshot doesn't blow up on a typical project layout.
func TestSnapshotContext_HandlesNestedDirs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0755)
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0644)
	out := snapshotContext(root, UnhealthyFact{})
	if !strings.Contains(out, "go.mod") {
		t.Errorf("snapshot missing go.mod marker:\n%s", out)
	}
	if !strings.Contains(out, "a/") {
		t.Errorf("snapshot missing layout entries:\n%s", out)
	}
	_ = fmt.Sprint
}
