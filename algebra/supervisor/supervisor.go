// Package supervisor implements the hierarchical coordination algebra.
// A supervisor owns a set of reactive workers and is responsible for their
// lifecycle: starting them, restarting on crash with a bounded policy, and
// reporting aggregate goal-completion status.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/freeap"
)

// ReactiveWorker describes a long-lived worker driven by an event source.
// Each event produces a fresh, finite Program[A] that the framework runs to
// completion before waiting for the next event. This is the split that keeps
// the Free applicative's nice properties: per-event programs are finite and
// analyzable; the reactive loop lives in the framework.
type ReactiveWorker[E any, A any] struct {
	ID     string
	Source func(ctx context.Context) (<-chan E, error)
	Handle func(ev E) freeap.Program[A]
	Report func(result A, err error) Report
}

// Report is the homogeneous projection of a worker's result.
type Report struct {
	WorkerID string
	OK       bool
	Detail   string
	At       time.Time
}

// RestartPolicy bounds how often a worker may be restarted.
type RestartPolicy struct {
	MaxRestarts int           // in a rolling window
	Window      time.Duration // window size
	Backoff     BackoffFunc   // how long to wait before restart
	OnExhausted Action        // what to do when MaxRestarts reached
}

// Action taken when restart budget is exhausted.
type Action int

const (
	Escalate  Action = iota // propagate failure up (default)
	Quarantine               // stop trying, mark worker as failed
)

// BackoffFunc computes wait duration for the n-th restart (n starts at 1).
type BackoffFunc func(n int) time.Duration

// ExponentialJitter returns a backoff function with exponential growth
// capped at maxWait, with ±25% jitter.
func ExponentialJitter(base, maxWait time.Duration) BackoffFunc {
	return func(n int) time.Duration {
		d := base << (n - 1)
		if d > maxWait || d <= 0 {
			d = maxWait
		}
		// Deterministic-ish "jitter" based on n — cheap and sufficient here.
		jitter := time.Duration(int64(d) / 4 * int64(n%3-1))
		return d + jitter
	}
}

// Config configures a Supervisor.
type Config struct {
	Name    string
	Restart RestartPolicy
	Logger  *slog.Logger
}

// GoalWitness is the convergence witness for the supervisor algebra: which
// workers are live/healthy/quarantined.
type GoalWitness struct {
	Healthy     int
	Quarantined int
	Total       int
	AllHealthy  bool
}

// Supervisor manages a set of reactive workers.
// A is the worker's per-event result type; for heterogeneous workers we'd
// need the existential-projection pattern, but for the MVP all our workers
// return PackageFact, so a single A suffices.
type Supervisor[E any, A any] struct {
	cfg     Config
	mu      sync.Mutex
	workers map[string]*workerState[E, A]
	reports chan Report
}

type workerState[E any, A any] struct {
	w            ReactiveWorker[E, A]
	restartTimes []time.Time
	quarantined  bool
	cancel       context.CancelFunc
	done         chan struct{}
}

// New creates a Supervisor.
func New[E any, A any](cfg Config) *Supervisor[E, A] {
	if cfg.Restart.MaxRestarts == 0 {
		cfg.Restart.MaxRestarts = 3
	}
	if cfg.Restart.Window == 0 {
		cfg.Restart.Window = time.Minute
	}
	if cfg.Restart.Backoff == nil {
		cfg.Restart.Backoff = ExponentialJitter(100*time.Millisecond, 5*time.Second)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Supervisor[E, A]{
		cfg:     cfg,
		workers: make(map[string]*workerState[E, A]),
		reports: make(chan Report, 64),
	}
}

// Reports returns a channel of worker reports.
func (s *Supervisor[E, A]) Reports() <-chan Report { return s.reports }

// Attach registers a worker. It's not started until Run.
func (s *Supervisor[E, A]) Attach(w ReactiveWorker[E, A]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers[w.ID] = &workerState[E, A]{w: w}
}

// Run starts all attached workers and blocks until ctx is cancelled.
func (s *Supervisor[E, A]) Run(ctx context.Context) error {
	s.mu.Lock()
	workers := make([]*workerState[E, A], 0, len(s.workers))
	for _, ws := range s.workers {
		workers = append(workers, ws)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, ws := range workers {
		wg.Add(1)
		go func(ws *workerState[E, A]) {
			defer wg.Done()
			s.superviseWorker(ctx, ws)
		}(ws)
	}
	wg.Wait()
	return ctx.Err()
}

// Witness computes the current GoalWitness.
func (s *Supervisor[E, A]) Witness() GoalWitness {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := GoalWitness{Total: len(s.workers)}
	for _, ws := range s.workers {
		if ws.quarantined {
			w.Quarantined++
		} else {
			w.Healthy++
		}
	}
	w.AllHealthy = w.Quarantined == 0 && w.Healthy == w.Total
	return w
}

// superviseWorker runs one worker with restart-on-panic-or-error semantics.
func (s *Supervisor[E, A]) superviseWorker(ctx context.Context, ws *workerState[E, A]) {
	log := s.cfg.Logger.With("worker", ws.w.ID)
	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if ws.quarantined {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		err := s.runOneLifetime(ctx, ws)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Worker's source closed cleanly — treat as normal termination.
			log.Info("worker ended normally")
			return
		}

		log.Warn("worker failed, considering restart", "err", err)
		if !s.shouldRestart(ws) {
			log.Error("restart budget exhausted", "action", s.cfg.Restart.OnExhausted)
			s.mu.Lock()
			ws.quarantined = true
			s.mu.Unlock()
			s.emitReport(Report{
				WorkerID: ws.w.ID, OK: false,
				Detail: "quarantined: " + err.Error(), At: time.Now(),
			})
			return
		}

		n := len(ws.restartTimes)
		wait := s.cfg.Restart.Backoff(n)
		log.Info("restarting worker", "after", wait, "attempt", n)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// runOneLifetime runs the worker's source loop once. Returns nil if the
// source channel closed (normal end) or an error on panic/source error.
func (s *Supervisor[E, A]) runOneLifetime(ctx context.Context, ws *workerState[E, A]) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	lifeCtx, cancel := context.WithCancel(ctx)
	ws.cancel = cancel
	defer cancel()

	events, srcErr := ws.w.Source(lifeCtx)
	if srcErr != nil {
		return fmt.Errorf("source: %w", srcErr)
	}

	for {
		select {
		case <-lifeCtx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			prog := ws.w.Handle(ev)
			result, runErr := freeap.Run(lifeCtx, prog)
			// Per-event errors do NOT restart the worker by default — the
			// worker-lifetime error is reserved for source failures / panics.
			// Per-event errors surface via the Report.
			s.emitReport(ws.w.Report(result, runErr))
		}
	}
}

func (s *Supervisor[E, A]) shouldRestart(ws *workerState[E, A]) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-s.cfg.Restart.Window)
	// Drop old restart records.
	filtered := ws.restartTimes[:0]
	for _, t := range ws.restartTimes {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	ws.restartTimes = filtered
	if len(ws.restartTimes) >= s.cfg.Restart.MaxRestarts {
		return false
	}
	ws.restartTimes = append(ws.restartTimes, now)
	return true
}

func (s *Supervisor[E, A]) emitReport(r Report) {
	select {
	case s.reports <- r:
	default:
		// Drop if no one is reading — supervisor shouldn't block on reporting.
	}
}
