package ensemble

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
)

type fact struct{ V int }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAnyWitness_ConvergedGoal(t *testing.T) {
	w := AnyWitness{Kind: KindGoal, Goal: &supervisor.GoalWitness{
		Healthy: 3, Total: 3, AllHealthy: true,
	}}
	if !w.Converged() {
		t.Error("should be converged")
	}
}

func TestAnyWitness_ConvergedEquilibrium(t *testing.T) {
	w := AnyWitness{Kind: KindEquilibrium, Equilibrium: &blackboard.EquilibriumWitness{
		Stable: true,
	}}
	if !w.Converged() {
		t.Error("should be converged")
	}
}

func TestAll_Combinator(t *testing.T) {
	conv := All()

	ws := []AnyWitness{
		{Kind: KindGoal, Goal: &supervisor.GoalWitness{AllHealthy: true}},
		{Kind: KindEquilibrium, Equilibrium: &blackboard.EquilibriumWitness{Stable: false}},
	}
	if _, ok := conv(ws); ok {
		t.Error("should not converge when one witness not converged")
	}

	ws[1].Equilibrium.Stable = true
	if _, ok := conv(ws); !ok {
		t.Error("should converge when all witnesses converged")
	}
}

func TestEnsemble_Snapshot(t *testing.T) {
	sup := supervisor.New[int, int](supervisor.Config{Logger: quietLogger()})
	board := blackboard.New[fact](blackboard.Config{})

	e := New(Config{})
	AttachSupervisor(e, "sup", sup)
	AttachBlackboard(e, "board", board)

	snap := e.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot length = %d, want 2", len(snap))
	}

	// Verify both witness kinds present.
	kinds := map[WitnessKind]bool{}
	for _, w := range snap {
		kinds[w.Kind] = true
	}
	if !kinds[KindGoal] || !kinds[KindEquilibrium] {
		t.Errorf("missing witness kind: %+v", kinds)
	}
}

func TestEnsemble_Await_Converges(t *testing.T) {
	sup := supervisor.New[int, int](supervisor.Config{Logger: quietLogger()})
	// No workers attached → goal witness is trivially AllHealthy (0/0).
	board := blackboard.New[fact](blackboard.Config{
		QuietFor: 30 * time.Millisecond,
		Rounds:   2,
	})
	// Post one fact so the blackboard has activity to become equilibrium-quiet
	// after. (Empty boards are never "converged" — they're trivially idle.)
	board.Post("init", fact{V: 1}, "test")

	e := New(Config{Tick: 20 * time.Millisecond})
	AttachSupervisor(e, "sup", sup)
	AttachBlackboard(e, "board", board)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	snap, msg, err := e.Await(ctx)
	if err != nil {
		t.Fatalf("await error: %v", err)
	}
	if msg == "" {
		t.Error("expected convergence message")
	}
	if len(snap) != 2 {
		t.Errorf("snap length = %d, want 2", len(snap))
	}
}

func TestEnsemble_Await_Timeout(t *testing.T) {
	sup := supervisor.New[int, int](supervisor.Config{Logger: quietLogger()})
	board := blackboard.New[fact](blackboard.Config{
		QuietFor: 10 * time.Second, // never within test budget
		Rounds:   10,
	})
	// Post a fact so equilibrium never reaches stability within the timeout.
	board.Post("k", fact{V: 1}, "test")

	e := New(Config{Tick: 20 * time.Millisecond})
	AttachSupervisor(e, "sup", sup)
	AttachBlackboard(e, "board", board)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := e.Await(ctx)
	if err == nil {
		t.Error("expected context error on timeout")
	}
}
