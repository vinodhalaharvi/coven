// Package analyzer walks a Program's structural description (NodeDesc) to
// extract information useful for budgeting, validation, and debugging.
//
// The crucial property: everything here is pure static analysis on data. No
// side effects, no execution. This is what the Free applicative buys us.
package analyzer

import (
	"fmt"

	"github.com/vinodhalaharvi/coven/freeap"
)

// Summary is what analysis produces for a Program.
type Summary struct {
	TotalOps        int
	LLMOps          int
	ComputeOps      int
	IOOps           int
	EventOps        int
	BridgeOps       int
	HasFlatMap      bool // whether the program contains a monadic boundary
	MaxParallelism  int  // independent Ap branches visible statically
	OpNames         []string
}

// Analyze walks the program structure.
func Analyze[A any](p freeap.Program[A]) Summary {
	s := Summary{}
	walk(p.Describe(), &s)
	// After walk, MaxParallelism is stored in s.MaxParallelism.
	return s
}

func walk(n freeap.NodeDesc, s *Summary) (par int) {
	switch n.Kind {
	case "pure":
		return 1
	case "lift":
		s.TotalOps++
		s.OpNames = append(s.OpNames, n.OpName)
		switch n.OpKind {
		case freeap.KindLLM:
			s.LLMOps++
		case freeap.KindCompute:
			s.ComputeOps++
		case freeap.KindIO:
			s.IOOps++
		case freeap.KindEvent:
			s.EventOps++
		case freeap.KindBridge:
			s.BridgeOps++
		}
		return 1
	case "ap":
		// Parallelism is sum of children — Ap means both branches run together.
		p := 0
		for _, c := range n.Children {
			p += walk(c, s)
		}
		if p > s.MaxParallelism {
			s.MaxParallelism = p
		}
		return p
	case "flatmap":
		s.HasFlatMap = true
		// Only the pre-bind branch is statically visible. Conservative: 1.
		for _, c := range n.Children {
			walk(c, s)
		}
		return 1
	default:
		return 1
	}
}

// Budget is a set of limits against which a Program can be validated.
type Budget struct {
	MaxLLMOps     int // -1 for unlimited
	MaxTotalOps   int
	MaxFlatMap    bool // if false, FlatMap is forbidden
	AllowedKinds  []freeap.OpKind
}

// Validate checks a Program against a Budget. Returns a list of violations.
func Validate[A any](p freeap.Program[A], b Budget) []string {
	s := Analyze(p)
	var violations []string

	if b.MaxLLMOps >= 0 && s.LLMOps > b.MaxLLMOps {
		violations = append(violations,
			fmt.Sprintf("too many LLM ops: %d > %d", s.LLMOps, b.MaxLLMOps))
	}
	if b.MaxTotalOps > 0 && s.TotalOps > b.MaxTotalOps {
		violations = append(violations,
			fmt.Sprintf("too many total ops: %d > %d", s.TotalOps, b.MaxTotalOps))
	}
	if !b.MaxFlatMap && s.HasFlatMap {
		violations = append(violations, "FlatMap not allowed in this budget")
	}
	if len(b.AllowedKinds) > 0 {
		allowed := make(map[freeap.OpKind]bool)
		for _, k := range b.AllowedKinds {
			allowed[k] = true
		}
		walkCheck(p.Describe(), allowed, &violations)
	}
	return violations
}

func walkCheck(n freeap.NodeDesc, allowed map[freeap.OpKind]bool, violations *[]string) {
	if n.Kind == "lift" {
		if !allowed[n.OpKind] {
			*violations = append(*violations,
				fmt.Sprintf("op %q has disallowed kind %q", n.OpName, n.OpKind))
		}
	}
	for _, c := range n.Children {
		walkCheck(c, allowed, violations)
	}
}
