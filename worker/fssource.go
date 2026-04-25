package worker

import (
	"context"
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
	Dir        string        // directory to watch (non-recursive)
	Debounce   time.Duration // quiet period before emitting; default 200ms
	Extensions []string      // file extensions to consider, e.g. [".go"]; nil = all
	IgnoreFile func(absPath string) bool // optional: return true to ignore an event for this path
}

// FSSource returns a source function suitable for ReactiveWorker.Source. It
// watches exactly cfg.Dir (non-recursive — each agent is scoped to one
// directory) and emits a debounced FSEvent per burst of relevant changes.
func FSSource(cfg FSConfig) func(ctx context.Context) (<-chan FSEvent, error) {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 200 * time.Millisecond
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

		out := make(chan FSEvent, 4)
		go debounceLoop(ctx, cfg.Dir, watcher, out, cfg.Debounce, exts, cfg.IgnoreFile)
		return out, nil
	}
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
