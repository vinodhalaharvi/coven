package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/freeap"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A simple worker that handles int events by doubling them.
func doublerWorker(id string, source func(ctx context.Context) (<-chan int, error)) ReactiveWorker[int, int] {
	return ReactiveWorker[int, int]{
		ID:     id,
		Source: source,
		Handle: func(ev int) freeap.Program[int] {
			return freeap.Lift(freeap.Op[int]{
				Name: "double",
				Run: func(ctx context.Context, w freeap.World) (int, error) {
					return ev * 2, nil
				},
			})
		},
		Report: func(result int, err error) Report {
			r := Report{WorkerID: id, At: time.Now()}
			if err != nil {
				r.OK = false
				r.Detail = err.Error()
			} else {
				r.OK = true
				r.Detail = "doubled"
			}
			return r
		},
	}
}

func TestSupervisor_BasicRun(t *testing.T) {
	events := make(chan int, 3)
	events <- 1
	events <- 2
	events <- 3
	close(events)

	source := func(ctx context.Context) (<-chan int, error) {
		return events, nil
	}

	sup := New[int, int](Config{Name: "test", Logger: quietLogger()})
	sup.Attach(doublerWorker("w1", source))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	// Drain 3 reports.
	var got int
	for got < 3 {
		select {
		case r := <-sup.Reports():
			if !r.OK {
				t.Errorf("report not ok: %+v", r)
			}
			got++
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("only got %d reports", got)
		}
	}
	cancel()
	<-done
}

func TestSupervisor_RestartOnSourceError(t *testing.T) {
	var attempts atomic.Int32
	source := func(ctx context.Context) (<-chan int, error) {
		n := attempts.Add(1)
		if n <= 2 {
			return nil, errors.New("source failed")
		}
		ch := make(chan int, 1)
		ch <- 42
		close(ch)
		return ch, nil
	}

	sup := New[int, int](Config{
		Name:   "test",
		Logger: quietLogger(),
		Restart: RestartPolicy{
			MaxRestarts: 5,
			Window:      time.Minute,
			Backoff:     func(n int) time.Duration { return 10 * time.Millisecond },
		},
	})
	sup.Attach(doublerWorker("w1", source))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	select {
	case r := <-sup.Reports():
		if !r.OK {
			t.Fatalf("expected success after restart, got %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatalf("no report received; attempts=%d", attempts.Load())
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts (2 fail + 1 succeed), got %d", attempts.Load())
	}
	cancel()
	<-done
}

func TestSupervisor_QuarantineOnExhaustion(t *testing.T) {
	var attempts atomic.Int32
	source := func(ctx context.Context) (<-chan int, error) {
		attempts.Add(1)
		return nil, errors.New("always fails")
	}

	sup := New[int, int](Config{
		Name:   "test",
		Logger: quietLogger(),
		Restart: RestartPolicy{
			MaxRestarts: 2,
			Window:      time.Minute,
			Backoff:     func(n int) time.Duration { return time.Millisecond },
		},
	})
	sup.Attach(doublerWorker("w1", source))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	// Expect a quarantine report.
	deadline := time.After(500 * time.Millisecond)
	var saw bool
	for !saw {
		select {
		case r := <-sup.Reports():
			if !r.OK && r.Detail != "" {
				saw = true
			}
		case <-deadline:
			t.Fatal("no quarantine report")
		}
	}

	// Worker should be quarantined now.
	w := sup.Witness()
	if w.Quarantined != 1 {
		t.Errorf("Quarantined = %d, want 1", w.Quarantined)
	}
	if w.AllHealthy {
		t.Error("AllHealthy should be false")
	}
	cancel()
	<-done
}

func TestSupervisor_PanicRecovery(t *testing.T) {
	var attempts atomic.Int32
	source := func(ctx context.Context) (<-chan int, error) {
		n := attempts.Add(1)
		if n == 1 {
			panic("first time is a panic")
		}
		ch := make(chan int, 1)
		ch <- 1
		close(ch)
		return ch, nil
	}

	sup := New[int, int](Config{
		Name:   "test",
		Logger: quietLogger(),
		Restart: RestartPolicy{
			MaxRestarts: 3,
			Window:      time.Minute,
			Backoff:     func(n int) time.Duration { return 5 * time.Millisecond },
		},
	})
	sup.Attach(doublerWorker("w1", source))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	select {
	case r := <-sup.Reports():
		if !r.OK {
			t.Fatalf("want success after panic recovery, got %+v", r)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no report after panic recovery")
	}
	cancel()
	<-done
}

func TestSupervisor_Witness(t *testing.T) {
	sup := New[int, int](Config{Name: "test", Logger: quietLogger()})
	for _, id := range []string{"a", "b", "c"} {
		src := func(ctx context.Context) (<-chan int, error) {
			ch := make(chan int)
			return ch, nil
		}
		sup.Attach(doublerWorker(id, src))
	}
	w := sup.Witness()
	if w.Total != 3 {
		t.Errorf("Total = %d, want 3", w.Total)
	}
	if w.Healthy != 3 {
		t.Errorf("Healthy = %d, want 3", w.Healthy)
	}
	if !w.AllHealthy {
		t.Error("AllHealthy should be true")
	}
}
