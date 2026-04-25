// Package protogen implements an agent that watches a proto source root,
// runs a code generator (typically `buf generate`) when proto files change,
// claims ownership of generated files, and posts a ProtoGenFact to the
// blackboard.
//
// Design seams:
//   - Runner is a function value (not an interface) — substitute with a
//     fake in tests, with `buf generate` in production.
//   - DiffWriter is the function that decides whether to actually write a
//     file (skip if checksum-equal). Default impl reads disk + compares.
package protogen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
	"github.com/vinodhalaharvi/coven/freeap"
	"github.com/vinodhalaharvi/coven/ownership"
	"github.com/vinodhalaharvi/coven/worker"
)

// GeneratedFile is one output from a Runner invocation.
type GeneratedFile struct {
	Path     string `json:"path"`     // absolute path
	Checksum string `json:"checksum"` // sha256 of content
	Bytes    []byte `json:"-"`        // contents (not serialized; large)
	Skipped  bool   `json:"skipped"`  // true if disk content matched (no write)
}

// ProtoGenFact is what the agent posts to the blackboard after each run.
type ProtoGenFact struct {
	AgentID       string          `json:"agent_id"`
	ProtoRoot     string          `json:"proto_root"`
	GenRoot       string          `json:"gen_root"`
	SourceProtos  []string        `json:"source_protos"`
	Generated     []GeneratedFile `json:"generated"`
	ChangedFiles  []string        `json:"changed_files"` // subset of Generated whose content changed
	OK            bool            `json:"ok"`
	Output        string          `json:"output,omitempty"`
	Duration      time.Duration   `json:"duration"`
	GeneratedAt   time.Time       `json:"generated_at"`
}

// Runner is the function-typed seam for invoking a code generator. It runs
// in protoRoot, writes outputs into genRoot (or wherever its config says),
// and returns the list of GeneratedFile (with full content) plus combined
// stdout+stderr and an error.
//
// The Runner does NOT write to disk — it returns the generated content. The
// ProtoGenAgent decides what to write based on the DiffWriter, so we can
// avoid spurious fsnotify cascades for unchanged outputs.
type Runner func(ctx context.Context, protoRoot, genRoot string) (files []GeneratedFile, output string, err error)

// DiffWriter writes a generated file only if its content differs from
// what's already on disk. Returns whether a write occurred.
type DiffWriter func(path string, content []byte) (wrote bool, err error)

// DefaultDiffWriter reads the existing file (if any), compares bytes, and
// writes only on diff. Creates parent dirs as needed.
func DefaultDiffWriter(path string, content []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && len(existing) == len(content) && bytesEqual(existing, content) {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && err != nil && len(existing) != 0 {
		// Real read error other than "not exist" — surface it.
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		return false, err
	}
	return true, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Config configures a ProtoGenAgent.
type Config struct {
	AgentID    string
	ProtoRoot  string                            // directory watched for .proto changes
	GenRoot    string                            // base for generated outputs (informational)
	Board      *blackboard.Board[ProtoGenFact]
	Ownership  ownership.Registry                // ownership claims for generated files
	Runner     Runner                            // injected; required
	WriteFile  DiffWriter                        // optional; defaults to DefaultDiffWriter
	Debounce   time.Duration                     // fsnotify debounce; default 200ms
	Extensions []string                          // proto extensions to watch; default [".proto"]
}

// BuildReactiveWorker assembles a supervisor.ReactiveWorker for the agent.
func BuildReactiveWorker(cfg Config) supervisor.ReactiveWorker[worker.FSEvent, ProtoGenFact] {
	if cfg.WriteFile == nil {
		cfg.WriteFile = DefaultDiffWriter
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 200 * time.Millisecond
	}
	if len(cfg.Extensions) == 0 {
		cfg.Extensions = []string{".proto"}
	}
	a := &agent{cfg: cfg}
	src := worker.FSSource(worker.FSConfig{
		Dir:        cfg.ProtoRoot,
		Debounce:   cfg.Debounce,
		Extensions: cfg.Extensions,
		Recursive:  true, // proto trees are conventionally nested (e.g. proto/svc/v1/...)
	})
	return supervisor.ReactiveWorker[worker.FSEvent, ProtoGenFact]{
		ID:     cfg.AgentID,
		Source: src,
		Handle: a.handle,
		Report: a.report,
	}
}

type agent struct {
	cfg Config
}

// handle: list source protos → run generator → for each output, diff-write
// and claim ownership → post fact.
func (a *agent) handle(_ worker.FSEvent) freeap.Program[ProtoGenFact] {
	listProtos := freeap.Lift(freeap.Op[[]string]{
		Name: "list-protos",
		Kind: freeap.KindCompute,
		Run: func(ctx context.Context, w freeap.World) ([]string, error) {
			return listFilesByExt(a.cfg.ProtoRoot, ".proto")
		},
	})

	return freeap.FlatMap(listProtos, func(protos []string) freeap.Program[ProtoGenFact] {
		return freeap.Lift(freeap.Op[ProtoGenFact]{
			Name: "buf-generate",
			Kind: freeap.KindIO,
			Run: func(ctx context.Context, w freeap.World) (ProtoGenFact, error) {
				start := time.Now()
				files, output, err := a.cfg.Runner(ctx, a.cfg.ProtoRoot, a.cfg.GenRoot)
				fact := ProtoGenFact{
					AgentID:      a.cfg.AgentID,
					ProtoRoot:    a.cfg.ProtoRoot,
					GenRoot:      a.cfg.GenRoot,
					SourceProtos: protos,
					Output:       output,
					Duration:     time.Since(start),
					GeneratedAt:  time.Now(),
				}
				if err != nil {
					fact.OK = false
					a.cfg.Board.Post(factKey(a.cfg.AgentID), fact, a.cfg.AgentID)
					return fact, nil // do NOT bubble up — bad gen is a fact, not a worker failure
				}

				// Diff-write each output, compute checksum, claim ownership.
				var changed []string
				written := make([]GeneratedFile, 0, len(files))
				for _, f := range files {
					sum := sha256Hex(f.Bytes)
					f.Checksum = sum
					wrote, werr := a.cfg.WriteFile(f.Path, f.Bytes)
					if werr != nil {
						return ProtoGenFact{}, fmt.Errorf("write %s: %w", f.Path, werr)
					}
					f.Skipped = !wrote
					if wrote {
						changed = append(changed, f.Path)
					}
					if a.cfg.Ownership != nil {
						a.cfg.Ownership.Claim(f.Path, ownership.AgentID(a.cfg.AgentID))
					}
					// Don't ship file bytes onto the blackboard — only metadata.
					f.Bytes = nil
					written = append(written, f)
				}
				fact.Generated = written
				fact.ChangedFiles = changed
				fact.OK = true
				a.cfg.Board.Post(factKey(a.cfg.AgentID), fact, a.cfg.AgentID)
				return fact, nil
			},
		})
	})
}

func (a *agent) report(f ProtoGenFact, err error) supervisor.Report {
	r := supervisor.Report{WorkerID: a.cfg.AgentID, At: time.Now()}
	if err != nil {
		r.OK = false
		r.Detail = "error: " + err.Error()
		return r
	}
	r.OK = f.OK
	if f.OK {
		if len(f.ChangedFiles) == 0 {
			r.Detail = "regenerated; no changes"
		} else {
			r.Detail = fmt.Sprintf("regenerated; %d file(s) changed", len(f.ChangedFiles))
		}
	} else {
		r.Detail = "generation failed: " + truncate(f.Output, 80)
	}
	return r
}

func factKey(agentID string) string {
	return "protogen:" + agentID
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func listFilesByExt(root, ext string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ext) {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
