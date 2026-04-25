# coven — multi-agent Go package monitor
![CI](https://github.com/vinodhalaharvi/coven/actions/workflows/ci.yml/badge.svg)

A category-theoretic multi-agent system for live Go projects. Each agent
watches a slice of the codebase and reacts independently; a blackboard
mediates cross-cutting effects. The architecture combines:

- **Free applicative DSL** (with bounded monadic FlatMap) for agent programs
- **Hierarchical supervision** for lifecycle, restart, quarantine
- **Blackboard algebra** for shared facts + equilibrium detection
- **Function-typed seams** (LLM, Runner, Scatter) for substitutable behavior
- **Generic types end-to-end**, no `interface{}` erasure
- **Capability interfaces** only for genuine contracts (Ownership)

Three agent kinds, all reactive but with different sources:

| Agent | Source | Scope | Owner of files |
|---|---|---|---|
| **PackageAgent** | fsnotify on its dir | one Go package | hand-written Go in dir |
| **ProtoGenAgent** | fsnotify on proto root | one proto tree | generated files (anywhere) |
| **ToolAgent** (e.g. golangci-lint) | blackboard subscription + settle | configured (often module-wide) | nothing |

## Build & test

Requires Go 1.22+.

```
go test ./... -race
go build ./cmd/demo
```

**94 tests, all pass under `-race`. `go vet ./...` clean.**

## Run

Watch a Go module's packages:

```
./demo -root /path/to/your/go/module
```

With a proto root and buf code generation:

```
./demo -root . -proto-root proto -gen-root gen -buf buf
```

Edit any .proto and the chain fires:

1. ProtoGenAgent runs `buf generate`
2. Diff-writes outputs (skip if unchanged → no spurious cascades)
3. Claims ownership of every output
4. Posts ProtoGenFact to the gen blackboard
5. Each PackageAgent whose directory received a generated file (via blackboard
   subscription) re-analyzes and rebuilds
6. PackageAgents that import from the regenerated package see the upstream
   PackageFact change and re-analyze in turn

With a linter:

```
./demo -root . -lint -lint-settle 3s
```

The lint tool agent waits for package builds to settle, runs golangci-lint
once, and scatters per-package ToolFacts onto the lint blackboard.

With Claude review on every package:

```
ANTHROPIC_API_KEY=sk-... ./demo -root . -llm haiku -review 30s
```

All flags compose. You can run protogen + lint + Claude review simultaneously.

## Architecture

```
freeap/                 Free applicative + bounded monadic FlatMap
                        Pure, Lift, Ap (parallel), Map, FlatMap
                        NodeDesc for static analysis

algebra/
  supervisor/           Hierarchical lifecycle algebra
                        Restart policies, quarantine, panic recovery
                        Witness: GoalWitness

  blackboard/           Shared-state algebra
                        Pattern subscriptions, equilibrium detection
                        Witness: EquilibriumWitness

ownership/              Capability interface (Registry) + InMemory impl
                        IgnoreForeignFiles function-typed seam used by FSConfig

sources/                Reusable source combinators
                        BlackboardSource, MergeSources, AggregatedSource, MapSource

llm/                    Function-typed Claude client + Static/Scripted fakes
                        Structured[T] generic wrapper

analyzer/               Static AST analysis of freeap.Programs

worker/                 PackageAgent — Go package watcher
                        Per-event Program: checksum → (ExtractAll || Build || Vet)
                        → optional LLM review → post to pkg blackboard

protogen/               ProtoGenAgent — proto watcher
                        Runner is function-typed (FakeRunner / ExecRunner / your own)
                        DiffWriter avoids spurious cascades
                        Claims file ownership

toolagent/              Generic cross-cutting tool agent
                        Driven by AggregatedSource over upstream blackboard
                        Specialization: GolangciLintRunner + PathPrefixScatter
                        Easy to add: staticcheck, go test, vuln check

ensemble/               Composition layer
                        AnyWitness tagged-union erasure at boundary
                        Combinators: All, ...

cmd/demo/               CLI: discover, spawn agents, run ensemble

integration/            End-to-end tests:
                        - multi-package convergence
                        - broken package detection + recovery
                        - proto change cascade to consumer
                        - ownership prevents feedback loop
                        - lint fires after package activity settles
```

## Key design decisions

**Generic types over interfaces.** `Program[A]`, `Worker[E,A]`, `Source[E]`,
`Op[A]`, `Board[F]` all preserve types end-to-end. Interfaces appear only
for capability contracts (`ownership.Registry`).

**Function-typed seams.** `LLM`, `Runner`, `Scatter`, `DiffWriter`,
`Combinator`, `IgnoreFile` are all function values. Substituting a fake in
tests or swapping a real backend in production is a one-line change with no
type-system contortion.

**Blackboard semantics under concurrency.** Subscription cleanup marks
subscriptions as closed under the same lock Post uses to snapshot
subscribers. Channels are intentionally NOT closed (would race with
concurrent sends). This was caught by `-race` during cascade integration
testing.

**Per-package fsnotify scope.** Each agent watches only its own directory,
non-recursively. Cross-package signals flow through the blackboard, not
through transitively-watched filesystem events.

**Ownership-aware fsnotify.** The `IgnoreFile` hook on FSConfig consults the
ownership registry and ignores events for files claimed by other agents.
This breaks the feedback loop where a generated file lands inside a
PackageAgent's watched directory.

**Diff-write to break cascades.** `buf generate` rewrites all outputs even
when only one proto changed. The DiffWriter compares bytes against disk
before writing, so unchanged content produces no fsnotify event downstream.
Test `TestProtoGen_DiffWriter_SkipsUnchanged` enforces this.

**Reactive primitive.** ReactiveWorker separates the framework-owned
infinite loop from user-owned per-event finite Programs. The Free
applicative's static-analysis property is preserved for the user program.

**Convergence via post-activity equilibrium.** EquilibriumWitness reports
Stable=true only after at least one Post — empty boards are trivially idle,
not converged. The ensemble's `All()` combinator is composable; specialized
combinators (e.g. "lint caught up to builds") slot in.

## What's deliberately not in this slice

- NATS transport (in-memory channels only)
- Actor and gossip algebras
- Cross-package change-proposal protocol (would use actor algebra)
- Per-algebra witness unions enforced via Go generic constraints
- Compensation / rewind on divergence
- Real `buf` or `golangci-lint` binaries are not bundled — the function
  typed Runners take a binary path, defaulting to `buf` and `golangci-lint`
  on PATH. All tests use injected fakes; no external tools required.

## Testing philosophy

- Every package has its own unit tests (`-race` clean)
- Function-typed seams enable hermetic integration tests with no external
  binaries (FakeRunner, FakeLinter, Static/Scripted LLM)
- The cascade integration test exercises three agent kinds, two boards,
  ownership, and self-healing in a real temp Go module running real
  `go build` / `go vet`
- Race detection found a real concurrency bug in the blackboard during
  development; the fix and a regression test are in place
