// Package diagnostic provides an agent that consults Claude when other
// agents post unhealthy facts. Where deterministic agents run subprocesses
// and report what happened, the diagnostic agent reads what happened,
// asks Claude what's wrong and what to do, proposes a shell command,
// and runs it on user confirmation.
//
// This is the cognition layer. The other agents are reflexes; this is the
// piece that thinks.
package diagnostic

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/llm"
)

// Diagnosis is what Claude returns. Structured so the user sees the
// proposed action before it runs.
type Diagnosis struct {
	Problem    string `json:"problem"`              // one-line summary
	Command    string `json:"command,omitempty"`    // shell command; empty = no proposal
	Why        string `json:"why,omitempty"`        // 1-3 sentence rationale
	Confidence string `json:"confidence,omitempty"` // "high" | "medium" | "low"
}

// UnhealthyFact is the input shape. Other packages adapt their fact types
// into this. We keep it loose-typed on purpose — diagnostic doesn't need
// to know the schema of every agent.
type UnhealthyFact struct {
	Source    string    // "buildhealth" | "protogen" | "wire" | "sqlc" | "package"
	Subject   string    // package name, file path, etc.
	ErrorText string    // raw output from the failing command
	When      time.Time
}

// Config configures a diagnostic agent.
type Config struct {
	ModuleRoot string         // where commands run
	LLM        llm.LLM        // the cognition seam
	In         *bufio.Reader  // user stdin (defaults to os.Stdin)
	Out        func(string)   // print to user (defaults to fmt.Print)
	AutoRun    bool           // skip confirmation entirely
	DedupeFor  time.Duration  // don't re-diagnose same key within this window; default 30s
}

// Agent is a long-running diagnostic loop.
type Agent struct {
	cfg     Config
	mu      sync.Mutex
	pending map[string]time.Time // dedupe: source+subject -> last-diagnosed
	asking  bool                 // only one prompt at a time
}

// New creates a diagnostic agent.
func New(cfg Config) *Agent {
	if cfg.In == nil {
		cfg.In = bufio.NewReader(os.Stdin)
	}
	if cfg.Out == nil {
		cfg.Out = func(s string) { fmt.Print(s) }
	}
	if cfg.DedupeFor <= 0 {
		cfg.DedupeFor = 30 * time.Second
	}
	return &Agent{cfg: cfg, pending: make(map[string]time.Time)}
}

// Observe is called by the wiring layer when an unhealthy fact arrives.
// Non-blocking: returns immediately, runs diagnosis in a goroutine.
// Deduplicates: same (Source, Subject) within DedupeFor is dropped.
// Serializes: only one diagnosis prompt is shown to the user at a time.
func (a *Agent) Observe(ctx context.Context, f UnhealthyFact) {
	key := f.Source + ":" + f.Subject
	a.mu.Lock()
	if last, seen := a.pending[key]; seen && time.Since(last) < a.cfg.DedupeFor {
		a.mu.Unlock()
		return
	}
	a.pending[key] = time.Now()
	if a.asking {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	go a.diagnose(ctx, f)
}

func (a *Agent) diagnose(ctx context.Context, f UnhealthyFact) {
	a.mu.Lock()
	if a.asking {
		a.mu.Unlock()
		return
	}
	a.asking = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.asking = false
		a.mu.Unlock()
	}()

	prompt := buildPrompt(a.cfg.ModuleRoot, f)

	getDiag := llm.Structured[Diagnosis](a.cfg.LLM, diagnosisSchema)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := getDiag(cctx, prompt)
	if err != nil {
		a.cfg.Out(fmt.Sprintf("\n  [diagnostic] Claude unavailable: %v\n", err))
		return
	}

	a.cfg.Out("\n")
	a.cfg.Out(fmt.Sprintf("  [DIAGNOSIS] %s\n", d.Problem))
	if d.Why != "" {
		a.cfg.Out(fmt.Sprintf("     why: %s\n", d.Why))
	}
	if d.Command == "" {
		a.cfg.Out("     no automated proposal — needs human attention\n\n")
		return
	}
	a.cfg.Out(fmt.Sprintf("     proposed: %s\n", d.Command))
	if d.Confidence != "" {
		a.cfg.Out(fmt.Sprintf("     confidence: %s\n", d.Confidence))
	}

	if !a.cfg.AutoRun {
		a.cfg.Out("     run it? [y/N] ")
		line, _ := a.cfg.In.ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		if line != "y" && line != "yes" {
			a.cfg.Out("     (skipped)\n\n")
			return
		}
	}

	a.cfg.Out(fmt.Sprintf("     running: %s\n", d.Command))
	out, runErr := a.runCommand(ctx, d.Command)
	if runErr != nil {
		a.cfg.Out(fmt.Sprintf("     FAILED: %v\n", runErr))
		if strings.TrimSpace(out) != "" {
			a.cfg.Out(fmt.Sprintf("     output:\n%s\n\n", indent(out, "       ")))
		} else {
			a.cfg.Out("\n")
		}
		return
	}
	a.cfg.Out("     OK\n")
	if strings.TrimSpace(out) != "" {
		a.cfg.Out(fmt.Sprintf("     output:\n%s\n", indent(out, "       ")))
	}
	a.cfg.Out("\n")
}

func (a *Agent) runCommand(ctx context.Context, cmdline string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// Use sh -c so users can write `cd x && wire ./y` style commands.
	cmd := exec.CommandContext(cctx, "sh", "-c", cmdline)
	cmd.Dir = a.cfg.ModuleRoot
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

const diagnosisSchema = `{
  "problem": "<one sentence describing what's wrong>",
  "command": "<a single shell command to run from the module root, or empty string if no automated fix is appropriate>",
  "why": "<1-3 sentences explaining why this command should fix the problem>",
  "confidence": "high | medium | low"
}`

func buildPrompt(moduleRoot string, f UnhealthyFact) string {
	var b strings.Builder
	b.WriteString("You are an expert Go developer diagnosing a build/codegen failure in a Go module.\n\n")
	b.WriteString(fmt.Sprintf("Source agent: %s\n", f.Source))
	b.WriteString(fmt.Sprintf("Subject: %s\n", f.Subject))
	b.WriteString(fmt.Sprintf("Module root: %s\n", moduleRoot))
	b.WriteString("\n--- ERROR OUTPUT ---\n")
	b.WriteString(truncate(f.ErrorText, 3000))
	b.WriteString("\n--- END ERROR ---\n\n")

	// Snapshot a few key files for context.
	b.WriteString(snapshotContext(moduleRoot, f))

	b.WriteString("\nPropose ONE shell command that, run from the module root, would most likely fix the problem.\n")
	b.WriteString("Common useful commands include:\n")
	b.WriteString("  - `go get <module>` to add a missing dep\n")
	b.WriteString("  - `go mod tidy` to fix go.sum issues\n")
	b.WriteString("  - `buf generate` to regenerate proto files\n")
	b.WriteString("  - `wire ./<pkg>` to regenerate wire injectors\n")
	b.WriteString("  - `mkdir -p gen && buf generate` for first-time proto setup\n")
	b.WriteString("\nIf no automated fix is appropriate (e.g. the error needs human code edits), set command to empty string.\n")
	b.WriteString("Respond with JSON matching exactly this schema, no extra text:\n")
	b.WriteString(diagnosisSchema)
	return b.String()
}

func snapshotContext(moduleRoot string, f UnhealthyFact) string {
	var b strings.Builder
	b.WriteString("--- PROJECT CONTEXT ---\n")

	// go.mod
	if body, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod")); err == nil {
		b.WriteString("go.mod:\n")
		b.WriteString(truncate(string(body), 1500))
		b.WriteString("\n")
	} else {
		b.WriteString("(no go.mod present)\n")
	}

	// Top-level layout (depth 2)
	b.WriteString("\nproject layout (depth 2):\n")
	listEntries(&b, moduleRoot, 2)

	b.WriteString("--- END CONTEXT ---\n")
	return b.String()
}

func listEntries(b *strings.Builder, root string, maxDepth int) {
	walk(b, root, "", maxDepth)
}

func walk(b *strings.Builder, dir, prefix string, depth int) {
	if depth <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if name == "node_modules" || name == "vendor" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(name)
		if e.IsDir() {
			b.WriteString("/")
		}
		b.WriteString("\n")
		if e.IsDir() {
			walk(b, filepath.Join(dir, name), prefix+"  ", depth-1)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
