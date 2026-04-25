// Package freeap implements a Free applicative (with a bounded monadic
// escape hatch via FlatMap) for describing agent programs as inspectable
// data. The structure is visible to analyzers and the interpreter, which
// lets us run Ap branches in parallel, checkpoint at step boundaries, and
// reject malformed programs before execution.
package freeap

import (
	"context"
	"fmt"
	"sync"
)

// World is the execution environment passed to ops. Algebras extend it via
// context values; the freeap layer itself stays agnostic.
type World struct {
	Ctx context.Context
}

// OpKind categorizes ops for analyzers and budgeting.
type OpKind string

const (
	KindCompute OpKind = "compute"
	KindLLM     OpKind = "llm"
	KindIO      OpKind = "io"
	KindEvent   OpKind = "event"
	KindBridge  OpKind = "bridge"
)

// Op is a single typed operation producing A.
type Op[A any] struct {
	Name string
	Kind OpKind
	Run  func(ctx context.Context, w World) (A, error)
}

// Program is the Free applicative AST. Sealed — only the constructors below
// produce inhabitants. The isProgram method makes it a closed sum.
type Program[A any] interface {
	isProgram(A)
	Describe() NodeDesc
}

// NodeDesc is a structural description used by analyzers. Kept concrete so
// inspection doesn't require reflection.
type NodeDesc struct {
	Kind     string // "pure" | "lift" | "ap" | "map" | "flatmap"
	OpName   string
	OpKind   OpKind
	Children []NodeDesc
}

// ─── Pure ────────────────────────────────────────────────────────────────

type pure[A any] struct{ v A }

func (pure[A]) isProgram(A)          {}
func (p pure[A]) Describe() NodeDesc { return NodeDesc{Kind: "pure"} }

func Pure[A any](v A) Program[A] { return pure[A]{v: v} }

// ─── Lift ────────────────────────────────────────────────────────────────

type lift[A any] struct{ op Op[A] }

func (lift[A]) isProgram(A) {}
func (l lift[A]) Describe() NodeDesc {
	return NodeDesc{Kind: "lift", OpName: l.op.Name, OpKind: l.op.Kind}
}

func Lift[A any](op Op[A]) Program[A] { return lift[A]{op: op} }

// ─── Ap (applicative combine) ────────────────────────────────────────────

type apNode[A, B any] struct {
	pf Program[func(B) A]
	pb Program[B]
}

func (apNode[A, B]) isProgram(A) {}
func (n apNode[A, B]) Describe() NodeDesc {
	return NodeDesc{
		Kind:     "ap",
		Children: []NodeDesc{n.pf.Describe(), n.pb.Describe()},
	}
}

// Ap: Program[func(B) A] × Program[B] → Program[A]. Branches are independent
// and may be run concurrently.
func Ap[A, B any](pf Program[func(B) A], pb Program[B]) Program[A] {
	return apNode[A, B]{pf: pf, pb: pb}
}

// Map: (B → A) × Program[B] → Program[A]. Sugar for Ap with a Pure function.
func Map[A, B any](pb Program[B], f func(B) A) Program[A] {
	return Ap[A, B](Pure[func(B) A](f), pb)
}

// ─── FlatMap (bounded monadic escape) ────────────────────────────────────

type flatMap[A, B any] struct {
	pb Program[B]
	f  func(B) Program[A]
}

func (flatMap[A, B]) isProgram(A) {}
func (n flatMap[A, B]) Describe() NodeDesc {
	return NodeDesc{
		Kind:     "flatmap",
		Children: []NodeDesc{n.pb.Describe()},
	}
}

// FlatMap: Program[B] × (B → Program[A]) → Program[A]. The post-bind branch
// is not statically visible; analyzers treat flatmap as an opaque boundary.
func FlatMap[A, B any](pb Program[B], f func(B) Program[A]) Program[A] {
	return flatMap[A, B]{pb: pb, f: f}
}

// ─── Interpretation ──────────────────────────────────────────────────────

// Run interprets a Program. Ap branches execute concurrently; the first
// error cancels the sibling.
func Run[A any](ctx context.Context, p Program[A]) (A, error) {
	var zero A
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	w := World{Ctx: ctx}

	switch n := p.(type) {
	case pure[A]:
		return n.v, nil
	case lift[A]:
		return n.op.Run(ctx, w)
	default:
		if r, ok := any(p).(runner[A]); ok {
			return r.run(ctx, w)
		}
		return zero, fmt.Errorf("freeap: unknown node %T", p)
	}
}

// runner lets ap/flatMap implement their own execution with their B
// parameter captured — otherwise B would need to escape Run's signature.
type runner[A any] interface {
	run(ctx context.Context, w World) (A, error)
}

func (n apNode[A, B]) run(ctx context.Context, w World) (A, error) {
	var zero A
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		f       func(B) A
		b       B
		fErr    error
		bErr    error
		wg      sync.WaitGroup
		errOnce sync.Once
		firstErr error
	)

	report := func(err error) {
		if err != nil {
			errOnce.Do(func() { firstErr = err; cancel() })
		}
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		f, fErr = Run[func(B) A](cctx, n.pf)
		report(fErr)
	}()
	go func() {
		defer wg.Done()
		b, bErr = Run[B](cctx, n.pb)
		report(bErr)
	}()
	wg.Wait()

	if firstErr != nil {
		return zero, firstErr
	}
	return f(b), nil
}

func (n flatMap[A, B]) run(ctx context.Context, w World) (A, error) {
	var zero A
	b, err := Run[B](ctx, n.pb)
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return Run[A](ctx, n.f(b))
}
