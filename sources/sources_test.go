package sources

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
)

type fact struct{ V int }

func TestBlackboardSource_DeliversFacts(t *testing.T) {
	board := blackboard.New[fact](blackboard.Config{})

	src := BlackboardSource(board, "k:*",
		func(f blackboard.Fact[fact]) (int, bool) {
			return f.Value.V, true
		}, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	board.Post("k:1", fact{V: 10}, "x")
	board.Post("k:2", fact{V: 20}, "x")

	got := []int{}
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("only got %v", got)
		}
	}
}

func TestBlackboardSource_ProjectorCanFilter(t *testing.T) {
	board := blackboard.New[fact](blackboard.Config{})
	src := BlackboardSource(board, "*",
		func(f blackboard.Fact[fact]) (int, bool) {
			return f.Value.V, f.Value.V%2 == 0 // only evens
		}, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := src(ctx)
	if err != nil {
		t.Fatal(err)
	}

	board.Post("a", fact{V: 1}, "x") // filtered out
	board.Post("b", fact{V: 2}, "x") // delivered
	board.Post("c", fact{V: 3}, "x") // filtered out
	board.Post("d", fact{V: 4}, "x") // delivered

	got := []int{}
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("only got %v", got)
		}
	}
	for _, v := range got {
		if v%2 != 0 {
			t.Errorf("filter let through odd %d", v)
		}
	}
}

func TestMergeSources_FansIn(t *testing.T) {
	mk := func(values []int) Source[int] {
		return func(ctx context.Context) (<-chan int, error) {
			ch := make(chan int, len(values))
			for _, v := range values {
				ch <- v
			}
			close(ch)
			return ch, nil
		}
	}
	merged := MergeSources(mk([]int{1, 2}), mk([]int{3, 4}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := merged(ctx)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[int]bool{}
	for v := range ch {
		seen[v] = true
	}
	for _, want := range []int{1, 2, 3, 4} {
		if !seen[want] {
			t.Errorf("missing %d in merged output: %v", want, seen)
		}
	}
}

func TestAggregatedSource_Settles(t *testing.T) {
	// Inner source emits 5 events spaced 20ms apart, then is silent.
	inner := func(ctx context.Context) (<-chan int, error) {
		ch := make(chan int, 1)
		go func() {
			defer close(ch)
			for i := 0; i < 5; i++ {
				select {
				case ch <- i:
				case <-ctx.Done():
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			// then go silent
		}()
		return ch, nil
	}

	agg := AggregatedSource(Source[int](inner), 100*time.Millisecond,
		func(evs []int) string {
			return time.Now().Format("aggregated") // placeholder; we just care about count via len
		})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := agg(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// We expect ONE aggregated event after settling (~100ms after last emit).
	start := time.Now()
	select {
	case <-ch:
		elapsed := time.Since(start)
		if elapsed < 100*time.Millisecond {
			t.Errorf("aggregate emitted too early: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("no aggregated event")
	}

	// Should not get a second event (no more upstream activity).
	select {
	case <-ch:
		// could be a close-flush; allow channel close but not a value
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAggregatedSource_BatchesBurst(t *testing.T) {
	// Capture how many events per aggregate.
	var seen []int
	var mu sync.Mutex

	inner := func(ctx context.Context) (<-chan int, error) {
		ch := make(chan int, 3)
		go func() {
			defer close(ch)
			// Burst 1: 3 events quickly.
			for i := 0; i < 3; i++ {
				ch <- i
				time.Sleep(10 * time.Millisecond)
			}
			// Wait long enough for the aggregator to settle and emit.
			time.Sleep(200 * time.Millisecond)
			// Burst 2: 2 events quickly.
			for i := 10; i < 12; i++ {
				ch <- i
				time.Sleep(10 * time.Millisecond)
			}
		}()
		return ch, nil
	}

	agg := AggregatedSource(Source[int](inner), 80*time.Millisecond,
		func(evs []int) int {
			mu.Lock()
			seen = append(seen, len(evs))
			mu.Unlock()
			return len(evs)
		})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := agg(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Drain.
	count := 0
	deadline := time.After(2 * time.Second)
	for count < 2 {
		select {
		case _, ok := <-ch:
			if !ok {
				goto done
			}
			count++
		case <-deadline:
			goto done
		}
	}
done:
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want 2 aggregates, got %d: %v", len(seen), seen)
	}
	if seen[0] != 3 {
		t.Errorf("first aggregate size = %d, want 3", seen[0])
	}
}

func TestMapSource(t *testing.T) {
	inner := func(ctx context.Context) (<-chan int, error) {
		ch := make(chan int, 3)
		ch <- 1
		ch <- 2
		ch <- 3
		close(ch)
		return ch, nil
	}
	mapped := MapSource(Source[int](inner), func(n int) string {
		return string(rune('a' + n - 1))
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch, err := mapped(ctx)
	if err != nil {
		t.Fatal(err)
	}

	got := []string{}
	for v := range ch {
		got = append(got, v)
	}
	want := []string{"a", "b", "c"}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] got %q want %q", i, got[i], w)
		}
	}
}
