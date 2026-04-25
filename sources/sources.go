// Package sources provides composable event-source combinators for reactive
// agents. Sources are function values of shape:
//
//   type Source[E any] = func(ctx context.Context) (<-chan E, error)
//
// This is the same shape consumed by supervisor.ReactiveWorker.Source. By
// keeping it as a function type (not an interface), sources can be combined
// with closures and generic helpers without losing type information.
//
// The three primitives:
//
//   BlackboardSource — subscribe to a blackboard pattern, project facts to
//                      a typed event stream
//   MergeSources     — fan-in N sources of the same E into one stream
//   AggregatedSource — wait for upstream activity to settle, then emit one
//                      event capturing the burst (the "tool agent" pattern)
package sources

import (
	"context"
	"sync"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
)

// Source is the canonical source-function type. Defined as a named generic
// type (not an alias) for Go 1.22 compatibility — generic aliases landed in
// 1.24. Call sites still benefit from structural compatibility: any function
// of the underlying signature can be assigned to a Source[E].
type Source[E any] func(ctx context.Context) (<-chan E, error)

// BlackboardSource subscribes to a board pattern and projects each matching
// fact to an event of type E via the project function. The fact's value type
// F is preserved through the projector — no `any`, no type assertions.
//
// The buffer parameter sizes the underlying subscription channel; the output
// channel is unbuffered (the projection is cheap, so backpressure is fine).
func BlackboardSource[F any, E any](
	board *blackboard.Board[F],
	pattern string,
	project func(blackboard.Fact[F]) (E, bool),
	buffer int,
) Source[E] {
	if buffer <= 0 {
		buffer = 16
	}
	return func(ctx context.Context) (<-chan E, error) {
		sub, _ := board.Subscribe(ctx, pattern, buffer)
		out := make(chan E, 4)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case f, ok := <-sub:
					if !ok {
						return
					}
					ev, emit := project(f)
					if !emit {
						continue
					}
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}
}

// MergeSources fans in N sources of the same event type into one stream.
// All inner sources are started; the merged channel closes when all inputs
// have closed (or ctx is done).
func MergeSources[E any](inner ...Source[E]) Source[E] {
	return func(ctx context.Context) (<-chan E, error) {
		out := make(chan E, 4)
		var wg sync.WaitGroup
		for _, src := range inner {
			ch, err := src(ctx)
			if err != nil {
				return nil, err
			}
			wg.Add(1)
			go func(ch <-chan E) {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case ev, ok := <-ch:
						if !ok {
							return
						}
						select {
						case out <- ev:
						case <-ctx.Done():
							return
						}
					}
				}
			}(ch)
		}
		go func() {
			wg.Wait()
			close(out)
		}()
		return out, nil
	}
}

// AggregatedSource consumes an inner source and emits one aggregate event
// after activity has settled for `settleFor`. Useful for tool agents that
// should run once after a burst of upstream changes — e.g., lint after the
// build dust has settled.
//
// `aggregate` is called with the slice of buffered events and produces one
// output event. If the slice is empty when the timer fires (it shouldn't be,
// but defensively), no event is emitted.
func AggregatedSource[E any, A any](
	inner Source[E],
	settleFor time.Duration,
	aggregate func([]E) A,
) Source[A] {
	if settleFor <= 0 {
		settleFor = 500 * time.Millisecond
	}
	return func(ctx context.Context) (<-chan A, error) {
		ch, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan A, 4)
		go func() {
			defer close(out)
			var buf []E
			var timer *time.Timer
			var timerC <-chan time.Time
			arm := func() {
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(settleFor)
				timerC = timer.C
			}
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-ch:
					if !ok {
						// inner closed — flush any pending and return.
						if len(buf) > 0 {
							select {
							case out <- aggregate(buf):
							case <-ctx.Done():
							}
						}
						return
					}
					buf = append(buf, ev)
					arm()
				case <-timerC:
					if len(buf) > 0 {
						a := aggregate(buf)
						buf = buf[:0]
						select {
						case out <- a:
						case <-ctx.Done():
							return
						}
					}
					timerC = nil
				}
			}
		}()
		return out, nil
	}
}

// MapSource is a small helper: transform each event from inner via project.
// Useful when MergeSources requires uniform E but the upstream sources have
// different shapes that can be lifted into a common type.
func MapSource[E any, F any](inner Source[E], project func(E) F) Source[F] {
	return func(ctx context.Context) (<-chan F, error) {
		ch, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan F, 4)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- project(ev):
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	}
}
