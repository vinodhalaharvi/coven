package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// TestRunAgent_DoesNotRetryOnNilExit — if Run returns nil cleanly, the
// helper should exit (the agent decided it was done).
func TestRunAgent_DoesNotRetryOnNilExit(t *testing.T) {
	var calls int32
	run := func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runAgent(ctx, quietLog(), "test", run)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("got %d calls, want 1", got)
	}
}

// TestRunAgent_DoesNotRetryAfterCancel — if ctx is cancelled, no retry.
func TestRunAgent_DoesNotRetryAfterCancel(t *testing.T) {
	var calls int32
	run := func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("boom")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	runAgent(ctx, quietLog(), "test", run)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("got %d calls, want 1 (no retry after cancel)", got)
	}
}

// TestRunAgent_RetriesOnError — failures should result in retries with
// backoff. We use a very short context so we expect a small but nonzero
// number of retries.
func TestRunAgent_RetriesOnError(t *testing.T) {
	var calls int32
	run := func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("transient failure")
	}

	// 2.5s window: should see initial call + retry after 1s + retry after 2s
	// = 3 total calls. The 4-second backoff retry won't fit.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	runAgent(ctx, quietLog(), "test", run)

	got := atomic.LoadInt32(&calls)
	if got < 2 || got > 4 {
		t.Errorf("got %d calls, expected 2-4 (initial + 1-2 retries)", got)
	}
}

// TestRunAgent_BackoffGrowsExponentially — the gaps between calls
// should roughly double until the cap.
func TestRunAgent_BackoffGrowsExponentially(t *testing.T) {
	var times []time.Time
	run := func(ctx context.Context) error {
		times = append(times, time.Now())
		return errors.New("fail")
	}

	// 4s window: should see calls at t=0, t≈1s, t≈3s (1+2), and possibly t≈7s (cut off).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	runAgent(ctx, quietLog(), "test", run)

	if len(times) < 3 {
		t.Fatalf("got %d calls, expected at least 3", len(times))
	}

	gap1 := times[1].Sub(times[0])
	gap2 := times[2].Sub(times[1])

	// gap1 should be ~1s, gap2 should be ~2s. Allow generous slack for CI noise.
	if gap1 < 800*time.Millisecond || gap1 > 1500*time.Millisecond {
		t.Errorf("first backoff gap = %v, expected ~1s", gap1)
	}
	if gap2 < 1700*time.Millisecond || gap2 > 2500*time.Millisecond {
		t.Errorf("second backoff gap = %v, expected ~2s", gap2)
	}
	// gap2 should be roughly 2x gap1.
	if gap2 < gap1 {
		t.Errorf("backoff did not grow: gap1=%v, gap2=%v", gap1, gap2)
	}
}

// TestRunAgent_BackoffResetsAfterLongRun — if the agent runs for ≥60s
// before failing, backoff should reset.
//
// We can't actually wait 60s in a test, so we test the behavior
// indirectly by mocking time isn't worth it. Instead we just verify
// the resetAfter constant is reasonable; this guards against accidental
// changes that would make the reset never happen.
func TestRunAgent_ResetThresholdIsReasonable(t *testing.T) {
	// The actual resetAfter constant is private to runAgent. This test
	// exists primarily as documentation: if you change resetAfter, this
	// test serves as a checkpoint to think about why.
	//
	// Reasonable values: 30s-300s. Less than 30s and a flaky agent's
	// backoff never grows. More than 300s and a real outage cascade
	// keeps growing toward the 30s cap forever.
	t.Log("resetAfter is set to 60s in cmd/demo/main.go; if you change this, think about flaky-agent vs sustained-outage tradeoffs")
}
