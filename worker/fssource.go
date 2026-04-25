package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FSEvent is a debounced filesystem event. We collapse bursts of raw events
// (editors often fire 10+ per save) into one event per quiet period.
//
// ChangedFiles lists the absolute paths of files that triggered the burst —
// downstream agents use this to filter by file extension or by ownership.
type FSEvent struct {
	Dir          string
	ChangedAt    time.Time
	RawCount     int      // how many raw events collapsed into this one
	ChangedFiles []string // distinct absolute paths that triggered events
}

// FSConfig configures FSSource.
type FSConfig struct {
	Dir        string                    // directory to watch
	Debounce   time.Duration             // quiet period before emitting; default 200ms
	Extensions []string                  // file extensions to consider, e.g. [".go"]; nil = all
	IgnoreFile func(absPath string) bool // optional: return true to ignore an event for this path
	Recursive  bool                      // if true, watch all subdirectories (and new ones as they're created)
}

// FSSource returns a source function suitable for ReactiveWorker.Source.
//
// By default it watches only cfg.Dir (non-recursive — each agent scoped to
// one directory, which is the right default for Go package agents).
//
// When cfg.Recursive is true, FSSource walks the tree at startup and adds
// every subdirectory, then watches for newly-created subdirectories at
// runtime and adds them too. This is required for nested layouts like
// proto trees where source files live in subdirectories of the agent's
// configured root.
//
// On macOS, fsnotify uses kqueue, which only delivers events for paths
// that have been explicitly Add()'d. cfg.Dir is also resolved through
// EvalSymlinks so that events delivered with the canonical path (e.g.
// /private/tmp/... on macOS where /tmp is a symlink) match the watcher's
// internal bookkeeping.
func FSSource(cfg FSConfig) func(ctx context.Context) (<-chan FSEvent, error) {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 200 * time.Millisecond
	}
	// Resolve symlinks so the watcher's path matches what fsnotify reports.
	if resolved, err := filepath.EvalSymlinks(cfg.Dir); err == nil {
		cfg.Dir = resolved
	}
	// Normalize extensions for case-insensitive comparison and ensure leading dot.
	exts := make([]string, 0, len(cfg.Extensions))
	for _, e := range cfg.Extensions {
		e = strings.ToLower(e)
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		exts = append(exts, e)
	}

	return func(ctx context.Context) (<-chan FSEvent, error) {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, err
		}
		if err := watcher.Add(cfg.Dir); err != nil {
			watcher.Close()
			return nil, err
		}
		if cfg.Recursive {
			if err := addRecursive(watcher, cfg.Dir); err != nil {
				watcher.Close()
				return nil, err
			}
		}

		out := make(chan FSEvent, 4)
		go debounceLoop(ctx, cfg.Dir, watcher, out, cfg.Debounce, exts, cfg.IgnoreFile, cfg.Recursive)
		return out, nil
	}
}

// addRecursive walks root and adds every subdirectory to watcher. Best-effort:
// errors on individual subdirs (e.g. permission denied) are skipped, not fatal.
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable nodes; they'll just be invisible to events
		}
		if !info.IsDir() || path == root {
			return nil
		}
		_ = w.Add(path) // best-effort
		return nil
	})
}

// FSSourceLegacy is a backward-compatible shim used by the existing
// PackageAgent: watches dir for .go file changes only, no ignore filter.
func FSSourceLegacy(dir string, debounce time.Duration) func(ctx context.Context) (<-chan FSEvent, error) {
	return FSSource(FSConfig{
		Dir:        dir,
		Debounce:   debounce,
		Extensions: []string{".go"},
	})
}

func debounceLoop(
	ctx context.Context,
	dir string,
	watcher *fsnotify.Watcher,
	out chan<- FSEvent,
	debounce time.Duration,
	exts []string,
	ignore func(string) bool,
	recursive bool,
) {
	defer watcher.Close()
	defer close(out)

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
		timer = time.NewTimer(debounce)
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
		ev := FSEvent{
			Dir:          dir,
			ChangedAt:    time.Now(),
			RawCount:     count,
			ChangedFiles: paths,
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
		pending = false
		count = 0
		changed = make(map[string]struct{})
		timerC = nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				flush()
				return
			}
			// If a new directory appeared and we're recursive, watch it. Do
			// this BEFORE the relevance filter — directory creation isn't a
			// relevant content event, but it IS something we need to watch.
			if recursive && ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = watcher.Add(ev.Name)
					_ = addRecursive(watcher, ev.Name)
				}
			}
			if !isRelevantEvent(ev, exts) {
				continue
			}
			if ignore != nil && ignore(ev.Name) {
				continue
			}
			pending = true
			count++
			changed[ev.Name] = struct{}{}
			resetTimer()
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		case <-timerC:
			flush()
		}
	}
}

// isRelevantEvent: only requested extensions, only write/create/remove/rename
// ops. CHMOD and editor temp files are ignored.
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
