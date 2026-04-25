// Package ensemble composes peer algebras and evaluates convergence across
// them. For the MVP we compose one supervisor algebra and one blackboard
// algebra; the pattern generalizes to the full four-algebra design.
//
// Each algebra keeps its typed witness internally. The ensemble layer sees
// an erased AnyWitness struct (tagged union) — the one place we allow a
// heterogeneous collection, because the ensemble is by definition the place
// where heterogeneous things compose.
package ensemble

import (
	"context"
	"fmt"
	"time"

	"github.com/vinodhalaharvi/coven/algebra/blackboard"
	"github.com/vinodhalaharvi/coven/algebra/supervisor"
)

// WitnessKind discriminates entries in AnyWitness.
type WitnessKind int

const (
	KindGoal WitnessKind = iota
	KindEquilibrium
)

// AnyWitness is the tagged-union erasure used only at the ensemble boundary.
// Exactly one of the pointer fields is non-nil, per Kind.
type AnyWitness struct {
	Algebra     string
	Kind        WitnessKind
	Goal        *supervisor.GoalWitness
	Equilibrium *blackboard.EquilibriumWitness
}

// Converged returns whether this single witness is itself converged.
func (w AnyWitness) Converged() bool {
	switch w.Kind {
	case KindGoal:
		return w.Goal != nil && w.Goal.AllHealthy
	case KindEquilibrium:
		return w.Equilibrium != nil && w.Equilibrium.Stable
	}
	return false
}

func (w AnyWitness) String() string {
	switch w.Kind {
	case KindGoal:
		if w.Goal == nil {
			return "goal<nil>"
		}
		return fmt.Sprintf("goal{healthy=%d/%d quarantined=%d}",
			w.Goal.Healthy, w.Goal.Total, w.Goal.Quarantined)
	case KindEquilibrium:
		if w.Equilibrium == nil {
			return "equilibrium<nil>"
		}
		return fmt.Sprintf("equilibrium{stable=%v rounds=%d facts=%d}",
			w.Equilibrium.Stable, w.Equilibrium.Rounds, w.Equilibrium.LastFacts)
	}
	return "unknown"
}

// Probe is a function that produces one AnyWitness on demand. Each algebra
// is adapted into a Probe at Attach time — the adapter captures the typed
// witness and erases at the boundary.
type Probe func() AnyWitness

// Combinator decides whether the ensemble has converged given current
// witnesses. Return (result, true) to declare converged.
type Combinator func(witnesses []AnyWitness) (string, bool)

// All requires every named algebra to be converged.
func All() Combinator {
	return func(ws []AnyWitness) (string, bool) {
	   for _, w := range ws {
		   if !w.Converged() {
			   return "", false
		   }
	   }
	   return "all converged", true
	}
}

// Ensemble composes probes and evaluates convergence.
type Ensemble struct {
	probes map[string]Probe
	conv   Combinator
	tick   time.Duration
}

// Config configures the ensemble.
type Config struct {
	Convergence Combinator
	Tick        time.Duration // how often to poll witnesses; default 200ms
}

func New(cfg Config) *Ensemble {
	if cfg.Tick <= 0 {
		cfg.Tick = 200 * time.Millisecond
	}
	if cfg.Convergence == nil {
		cfg.Convergence = All()
	}
	return &Ensemble{
		probes: make(map[string]Probe),
		conv:   cfg.Convergence,
		tick:   cfg.Tick,
	}
}

// AttachSupervisor registers a supervisor algebra. Generic on the event and
// result types so the caller's Supervisor[E,A] type parameters don't escape.
func AttachSupervisor[E any, A any](e *Ensemble, name string, sup *supervisor.Supervisor[E, A]) {
	e.probes[name] = func() AnyWitness {
		w := sup.Witness()
		return AnyWitness{Algebra: name, Kind: KindGoal, Goal: &w}
	}
}

// AttachBlackboard registers a blackboard algebra.
func AttachBlackboard[F any](e *Ensemble, name string, b *blackboard.Board[F]) {
	e.probes[name] = func() AnyWitness {
		w := b.Equilibrium()
		return AnyWitness{Algebra: name, Kind: KindEquilibrium, Equilibrium: &w}
	}
}

// Snapshot captures all probes once.
func (e *Ensemble) Snapshot() []AnyWitness {
	out := make([]AnyWitness, 0, len(e.probes))
	for _, p := range e.probes {
		out = append(out, p())
	}
	return out
}

// Await polls witnesses until the combinator declares convergence, or ctx is
// done. Returns the final snapshot and the combinator's message.
func (e *Ensemble) Await(ctx context.Context) ([]AnyWitness, string, error) {
	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()
	for {
		snap := e.Snapshot()
		if msg, ok := e.conv(snap); ok {
			return snap, msg, nil
		}
		select {
		case <-ctx.Done():
			return snap, "", ctx.Err()
		case <-ticker.C:
		}
	}
}
