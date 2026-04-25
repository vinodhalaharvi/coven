package freeap

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPure(t *testing.T) {
	got, err := Run(context.Background(), Pure(42))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("want 42, got %d", got)
	}
}

func TestLift(t *testing.T) {
	op := Op[string]{
		Name: "hello",
		Kind: KindCompute,
		Run: func(ctx context.Context, w World) (string, error) {
			return "hi", nil
		},
	}
	got, err := Run(context.Background(), Lift(op))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hi" {
		t.Fatalf("want hi, got %q", got)
	}
}

func TestLiftError(t *testing.T) {
	boom := errors.New("boom")
	op := Op[int]{
		Name: "fail",
		Run: func(ctx context.Context, w World) (int, error) {
			return 0, boom
		},
	}
	_, err := Run(context.Background(), Lift(op))
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestMap(t *testing.T) {
	p := Map(Pure(21), func(x int) int { return x * 2 })
	got, err := Run(context.Background(), p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("want 42, got %d", got)
	}
}

func TestAp_Parallel(t *testing.T) {
	// Two ops that each sleep 100ms — if run in parallel via Ap, total
	// should be <180ms. If sequential, >=200ms.
	mkOp := func(name string, v int) Op[int] {
		return Op[int]{
			Name: name,
			Run: func(ctx context.Context, w World) (int, error) {
				select {
				case <-time.After(100 * time.Millisecond):
					return v, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			},
		}
	}
	combine := func(a int) func(int) int { return func(b int) int { return a + b } }
	p := Ap(Map(Lift(mkOp("a", 10)), combine), Lift(mkOp("b", 32)))

	start := time.Now()
	got, err := Run(context.Background(), p)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("want 42, got %d", got)
	}
	if elapsed > 180*time.Millisecond {
		t.Fatalf("Ap did not parallelize: %v", elapsed)
	}
}

func TestAp_ErrorCancelsSibling(t *testing.T) {
	var siblingStarted, siblingFinished atomic.Bool
	boom := errors.New("boom")

	failFast := Op[int]{
		Name: "fail",
		Run: func(ctx context.Context, w World) (int, error) {
			return 0, boom
		},
	}
	slowSibling := Op[int]{
		Name: "slow",
		Run: func(ctx context.Context, w World) (int, error) {
			siblingStarted.Store(true)
			select {
			case <-time.After(500 * time.Millisecond):
				siblingFinished.Store(true)
				return 1, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		},
	}
	combine := func(a int) func(int) int { return func(b int) int { return a + b } }
	p := Ap(Map(Lift(failFast), combine), Lift(slowSibling))

	start := time.Now()
	_, err := Run(context.Background(), p)
	elapsed := time.Since(start)

	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("sibling not cancelled: took %v", elapsed)
	}
	_ = siblingStarted.Load()
	if siblingFinished.Load() {
		t.Fatal("sibling should have been cancelled before finishing")
	}
}

func TestFlatMap(t *testing.T) {
	// FlatMap lets the second step depend on the first's value.
	first := Lift(Op[int]{
		Name: "first",
		Run:  func(ctx context.Context, w World) (int, error) { return 3, nil },
	})
	p := FlatMap(first, func(n int) Program[string] {
		return Lift(Op[string]{
			Name: "second",
			Run: func(ctx context.Context, w World) (string, error) {
				return string(rune('A' + n)), nil // 'A'+3 == 'D'
			},
		})
	})
	got, err := Run(context.Background(), p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "D" {
		t.Fatalf("want D, got %q", got)
	}
}

func TestFlatMap_ErrorShortCircuits(t *testing.T) {
	boom := errors.New("boom")
	first := Lift(Op[int]{
		Name: "first",
		Run:  func(ctx context.Context, w World) (int, error) { return 0, boom },
	})
	secondCalled := false
	p := FlatMap(first, func(n int) Program[string] {
		secondCalled = true
		return Pure("nope")
	})
	_, err := Run(context.Background(), p)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if secondCalled {
		t.Fatal("second stage should not run after first fails")
	}
}

func TestDescribe_StructurePreserved(t *testing.T) {
	op := Op[int]{Name: "x", Kind: KindCompute, Run: nil}
	p := Map(Lift(op), func(n int) int { return n + 1 })
	d := p.Describe()
	if d.Kind != "ap" {
		t.Fatalf("Map should describe as ap, got %s", d.Kind)
	}
	if len(d.Children) != 2 {
		t.Fatalf("ap should have 2 children, got %d", len(d.Children))
	}
	// Second child is the Lift of op 'x'.
	if d.Children[1].Kind != "lift" || d.Children[1].OpName != "x" {
		t.Fatalf("unexpected second child: %+v", d.Children[1])
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Pure(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}
