// Package fsmonitor is the central filesystem watcher. Exactly one
// instance runs in the process; every other agent reacts to its
// FileChangeFact rather than running its own fsnotify.
//
// The point of centralizing fsnotify:
//   - one watcher to configure, debug, and tune (we hit recursive-watch
//     and symlink bugs in the per-agent model)
//   - causal coherence: every reaction follows from one fact stream,
//     not from independent races between watchers
//   - downstream agents are stateless and pure: given (current files,
//     fact stream) they produce (their own outputs), no event ordering
//     subtleties
//
// fsmonitor itself is not an Agent in the supervisor sense. It is a
// Source[FileChangeFact] published to a blackboard. BuildHealthAgent
// runs the watcher; everything else subscribes.
package fsmonitor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vinodhalaharvi/coven/algebra/blackboard"
)

// FileChangeFact is published to the fs blackboard for every debounced
// burst of filesystem events under the watched root.
type FileChangeFact struct {
	Root         string    `json:"root"`
	ChangedFiles []string  `json:"changed_files"`
	ChangedAt    time.Time `json:"changed_at"`
	RawCount     int       `json:"raw_count"`
}

// Config configures the central watcher.
type Config struct {
	Root       string                            // module root; watched recursively
	Debounce   time.Duration                     // burst settle window; default 200ms
	Extensions []string                          // optional extension filter (".go", ".proto", ...); nil = all
	IgnoreFile func(absPath string) bool         // optional per-event filter
	Board      *blackboard.Board[FileChangeFact] // where to publish
	Author     string                            // posting author; default "fsmonitor"
}

// Run starts the watcher. It blocks until ctx is cancelled or fsnotify
// errors out. Returns the underlying setup error if the watcher fails to
// attach. After Run returns nil, the watcher has been cleanly torn down.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 200 * time.Millisecond
	}
	if cfg.Author == "" {
		cfg.Author = "fsmonitor"
	}

	root := cfg.Root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	exts := normalizeExts(cfg.Extensions)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(root); err != nil {
		return err
	}
	if err := addRecursive(watcher, root); err != nil {
		return err
	}

	var (
		pending bool
		count   int
		changed = make(map[string]struct{})
		timer   *time.Timer
		timerC  <-chan time.Time
	)
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(cfg.Debounce)
		timerC = timer.C
	}
	flush := func() {
		if !pending {
			return
		}
		paths := make([]string, 0, len(changed))
		for p := range changed {
			paths = append(paths, p)
		}
		now := time.Now()
		cfg.Board.Post(
			keyForBurst(now),
			FileChangeFact{
				Root:         root,
				ChangedFiles: paths,
				ChangedAt:    now,
				RawCount:     count,
			},
			cfg.Author,
		)
		pending = false
		count = 0
		changed = make(map[string]struct{})
		timerC = nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				flush()
				return nil
			}
			// New directory appeared: watch it (recursive mode).
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = watcher.Add(ev.Name)
					_ = addRecursive(watcher, ev.Name)
				}
			}
			if !isRelevantEvent(ev, exts) {
				continue
			}
			if cfg.IgnoreFile != nil && cfg.IgnoreFile(ev.Name) {
				continue
			}
			pending = true
			count++
			changed[ev.Name] = struct{}{}
			resetTimer()
		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		case <-timerC:
			flush()
		}
	}
}

// addRecursive walks root and adds every subdirectory to watcher.
// Best-effort: errors on individual subdirs are skipped.
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() || path == root {
			return nil
		}
		// Skip directories that aren't worth watching.
		base := filepath.Base(path)
		if base == ".git" || base == "node_modules" || base == "vendor" {
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})
}

// normalizeExts lowercases and ensures leading dot on each extension.
func normalizeExts(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(e)
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

// isRelevantEvent: filters out hidden files, editor temp files, and
// non-content events (chmod). If exts is non-empty, the file extension
// must match one of them.
func isRelevantEvent(ev fsnotify.Event, exts []string) bool {
	name := filepath.Base(ev.Name)
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
		return false
	}
	if len(exts) > 0 {
		nameLower := strings.ToLower(name)
		matched := false
		for _, e := range exts {
			if strings.HasSuffix(nameLower, e) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	opMask := fsnotify.Write | fsnotify.Create | fsnotify.Remove | fsnotify.Rename
	return ev.Op&opMask != 0
}

// Subscribe returns a Source[FileChangeFact] suitable for plugging into
// any Worker that wants to react to filesystem changes. Optional filter
// returns true to keep the fact, false to drop it. A nil filter passes
// everything.
func Subscribe(
	board *blackboard.Board[FileChangeFact],
	filter func(FileChangeFact) bool,
) func(ctx context.Context) (<-chan FileChangeFact, error) {
	return func(ctx context.Context) (<-chan FileChangeFact, error) {
		raw, _ := board.Subscribe(ctx, "*", 32)
		out := make(chan FileChangeFact, 8)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case f, ok := <-raw:
					if !ok {
						return
					}
					if filter != nil && !filter(f.Value) {
						continue
					}
					select {
					case out <- f.Value:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}
}

// FilesUnder returns a filter accepting facts whose ChangedFiles include
// at least one path under dir. Useful for scoping an agent to a
// directory while still consuming the central fact stream.
func FilesUnder(dir string) func(FileChangeFact) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	return func(f FileChangeFact) bool {
		for _, p := range f.ChangedFiles {
			if isUnder(p, abs) {
				return true
			}
		}
		return false
	}
}

// FilesWithExtension returns a filter accepting facts whose ChangedFiles
// include at least one with the given extension (e.g. ".proto").
func FilesWithExtension(ext string) func(FileChangeFact) bool {
	return func(f FileChangeFact) bool {
		for _, p := range f.ChangedFiles {
			if filepath.Ext(p) == ext {
				return true
			}
		}
		return false
	}
}

// AnyFiles is a filter accepting any fact with at least one changed file.
func AnyFiles(f FileChangeFact) bool { return len(f.ChangedFiles) > 0 }

func keyForBurst(t time.Time) string {
	// Per-burst key — facts are append-only, never overwritten, so keys
	// must be unique. Nanosecond timestamp plus counter would be even
	// safer, but in practice debounce ensures bursts are at least
	// debounce-apart, so nanosecond resolution suffices.
	return "fs:" + t.UTC().Format("20060102T150405.000000000")
}

func isUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." || rel == "" {
		return true
	}
	if len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return false
	}
	return true
}
