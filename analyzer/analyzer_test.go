package analyzer

import (
	"context"
	"testing"

	"github.com/vinodhalaharvi/coven/freeap"
)

func mkOp(name string, kind freeap.OpKind) freeap.Op[int] {
	return freeap.Op[int]{
		Name: name,
		Kind: kind,
		Run: func(ctx context.Context, w freeap.World) (int, error) {
			return 1, nil
		},
	}
}

func TestAnalyze_Empty(t *testing.T) {
	s := Analyze(freeap.Pure(42))
	if s.TotalOps != 0 {
		t.Errorf("TotalOps = %d, want 0", s.TotalOps)
	}
}

func TestAnalyze_SingleLift(t *testing.T) {
	p := freeap.Lift(mkOp("llm-call", freeap.KindLLM))
	s := Analyze(p)
	if s.TotalOps != 1 {
		t.Errorf("TotalOps = %d, want 1", s.TotalOps)
	}
	if s.LLMOps != 1 {
		t.Errorf("LLMOps = %d, want 1", s.LLMOps)
	}
	if len(s.OpNames) != 1 || s.OpNames[0] != "llm-call" {
		t.Errorf("OpNames = %v", s.OpNames)
	}
}

func TestAnalyze_Parallelism(t *testing.T) {
	// Ap(Map(Lift(a), f), Lift(b)) — two independent lifts = parallelism 2.
	a := freeap.Lift(mkOp("a", freeap.KindCompute))
	b := freeap.Lift(mkOp("b", freeap.KindCompute))
	combine := func(x int) func(int) int { return func(y int) int { return x + y } }
	p := freeap.Ap(freeap.Map(a, combine), b)

	s := Analyze(p)
	if s.TotalOps != 2 {
		t.Errorf("TotalOps = %d, want 2", s.TotalOps)
	}
	if s.MaxParallelism < 2 {
		t.Errorf("MaxParallelism = %d, want >= 2", s.MaxParallelism)
	}
}

func TestAnalyze_FlatMapDetected(t *testing.T) {
	first := freeap.Lift(mkOp("first", freeap.KindCompute))
	p := freeap.FlatMap(first, func(n int) freeap.Program[int] {
		return freeap.Pure(n + 1)
	})
	s := Analyze(p)
	if !s.HasFlatMap {
		t.Error("HasFlatMap should be true")
	}
}

func TestAnalyze_KindCounts(t *testing.T) {
	p := freeap.Map(
		freeap.Lift(mkOp("compute-op", freeap.KindCompute)),
		func(n int) int { return n },
	)
	// Add an LLM op in parallel.
	llm := freeap.Lift(mkOp("llm-op", freeap.KindLLM))
	combine := func(x int) func(int) int { return func(y int) int { return x + y } }
	p2 := freeap.Ap(freeap.Map(p, combine), llm)

	s := Analyze(p2)
	if s.ComputeOps != 1 {
		t.Errorf("ComputeOps = %d, want 1", s.ComputeOps)
	}
	if s.LLMOps != 1 {
		t.Errorf("LLMOps = %d, want 1", s.LLMOps)
	}
}

func TestValidate_TooManyLLM(t *testing.T) {
	llm1 := freeap.Lift(mkOp("llm1", freeap.KindLLM))
	llm2 := freeap.Lift(mkOp("llm2", freeap.KindLLM))
	combine := func(x int) func(int) int { return func(y int) int { return x + y } }
	p := freeap.Ap(freeap.Map(llm1, combine), llm2)

	violations := Validate(p, Budget{MaxLLMOps: 1})
	if len(violations) == 0 {
		t.Fatal("expected violation for too many LLM ops")
	}
}

func TestValidate_FlatMapForbidden(t *testing.T) {
	first := freeap.Lift(mkOp("first", freeap.KindCompute))
	p := freeap.FlatMap(first, func(n int) freeap.Program[int] { return freeap.Pure(n) })
	violations := Validate(p, Budget{MaxLLMOps: -1, MaxFlatMap: false})
	if len(violations) != 1 {
		t.Fatalf("want 1 violation, got %d: %v", len(violations), violations)
	}
}

func TestValidate_DisallowedKind(t *testing.T) {
	p := freeap.Lift(mkOp("io", freeap.KindIO))
	violations := Validate(p, Budget{
		MaxLLMOps:    -1,
		MaxFlatMap:   true,
		AllowedKinds: []freeap.OpKind{freeap.KindCompute},
	})
	if len(violations) != 1 {
		t.Fatalf("want 1 violation, got %d: %v", len(violations), violations)
	}
}

func TestValidate_Clean(t *testing.T) {
	p := freeap.Lift(mkOp("ok", freeap.KindCompute))
	violations := Validate(p, Budget{
		MaxLLMOps:    0,
		MaxTotalOps:  5,
		MaxFlatMap:   true,
		AllowedKinds: []freeap.OpKind{freeap.KindCompute},
	})
	if len(violations) != 0 {
		t.Fatalf("want no violations, got %v", violations)
	}
}
