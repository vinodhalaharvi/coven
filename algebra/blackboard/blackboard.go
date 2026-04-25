// Package blackboard implements the shared-state coordination algebra.
// Agents post facts keyed by a domain key; other agents subscribe to patterns
// and are notified when matching facts change. Equilibrium is detected when
// no fact changes for a configured quiet period.
package blackboard

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Fact is the canonical envelope for anything posted to the blackboard.
// F is the domain payload type (e.g., PackageFact).
type Fact[F any] struct {
	Key       string    // domain key, e.g., "pkg:github.com/foo/bar"
	Value     F         // the payload
	Author    string    // who posted it
	Timestamp time.Time // when it was posted
	Version   uint64    // monotonic per-board version number
}

// EquilibriumWitness is the convergence witness produced by the blackboard.
// It records that no updates occurred for QuietFor, over Rounds checks.
type EquilibriumWitness struct {
	Stable    bool
	QuietFor  time.Duration
	Rounds    int
	LastFacts int // count of facts at equilibrium
}

// subscription is an internal record of one subscribe call.
type subscription[F any] struct {
	id      uint64
	pattern string
	ch      chan Fact[F]
	cancel  context.CancelFunc
	closed  bool // protected by Board.mu; Post checks under lock
}

// Board is an in-memory, thread-safe blackboard for facts of type F.
type Board[F any] struct {
	mu       sync.RWMutex
	facts    map[string]Fact[F]
	version  uint64
	lastChg  time.Time
	subs     map[uint64]*subscription[F]
	nextSub  uint64

	// equilibrium detection config + state
	quietFor   time.Duration
	rounds     int
	quietCount int
}

// Config configures a Board.
type Config struct {
	// QuietFor is how long the board must be idle before a single equilibrium
	// check passes.
	QuietFor time.Duration
	// Rounds is how many consecutive quiet checks are required for the
	// witness to report Stable=true.
	Rounds int
}

// New creates a Board.
func New[F any](cfg Config) *Board[F] {
	if cfg.QuietFor <= 0 {
		cfg.QuietFor = 500 * time.Millisecond
	}
	if cfg.Rounds <= 0 {
		cfg.Rounds = 3
	}
	return &Board[F]{
		facts:    make(map[string]Fact[F]),
		subs:     make(map[uint64]*subscription[F]),
		lastChg:  time.Now(),
		quietFor: cfg.QuietFor,
		rounds:   cfg.Rounds,
	}
}

// Post writes or overwrites a fact. Returns the assigned version.
func (b *Board[F]) Post(key string, value F, author string) uint64 {
	b.mu.Lock()
	b.version++
	f := Fact[F]{
		Key:       key,
		Value:     value,
		Author:    author,
		Timestamp: time.Now(),
		Version:   b.version,
	}
	b.facts[key] = f
	b.lastChg = f.Timestamp
	b.quietCount = 0 // any change restarts the equilibrium counter
	// Snapshot subscribers to notify without holding the write lock.
	// We skip subscriptions whose context has been cancelled (closed flag
	// set under the same mutex by the cleanup goroutine).
	toNotify := make([]*subscription[F], 0, len(b.subs))
	for _, s := range b.subs {
		if s.closed {
			continue
		}
		if matches(s.pattern, key) {
			toNotify = append(toNotify, s)
		}
	}
	v := b.version
	b.mu.Unlock()

	for _, s := range toNotify {
		// Non-blocking send; if the subscriber is slow, drop. The subscriber
		// can always Get the latest value on-demand.
		select {
		case s.ch <- f:
		default:
		}
	}
	return v
}

// Get returns a fact by key.
func (b *Board[F]) Get(key string) (Fact[F], bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	f, ok := b.facts[key]
	return f, ok
}

// List returns all facts whose key matches pattern (prefix or "*").
func (b *Board[F]) List(pattern string) []Fact[F] {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Fact[F], 0, len(b.facts))
	for k, f := range b.facts {
		if matches(pattern, k) {
			out = append(out, f)
		}
	}
	return out
}

// Subscribe returns a channel of facts matching pattern. The caller must call
// the returned cancel function (or cancel ctx) to release resources.
func (b *Board[F]) Subscribe(ctx context.Context, pattern string, buffer int) (<-chan Fact[F], context.CancelFunc) {
	if buffer <= 0 {
		buffer = 16
	}
	subCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Fact[F], buffer)

	b.mu.Lock()
	b.nextSub++
	id := b.nextSub
	s := &subscription[F]{
		id:      id,
		pattern: pattern,
		ch:      ch,
		cancel:  cancel,
	}
	b.subs[id] = s
	b.mu.Unlock()

	// Cleanup when ctx is cancelled. We do NOT close ch here — closing it
	// would race with concurrent Post sends. Instead we mark the
	// subscription as closed under the same lock Post uses to snapshot
	// subscribers; Post will skip closed subs. The channel is then
	// garbage-collected once the subscriber stops reading.
	go func() {
		<-subCtx.Done()
		b.mu.Lock()
		s.closed = true
		delete(b.subs, id)
		b.mu.Unlock()
	}()

	return ch, cancel
}

// Equilibrium performs one equilibrium check. Returns a witness with
// Stable=true only after `Rounds` consecutive calls where no facts changed
// within `QuietFor` of the call time. Intended to be called periodically by
// the ensemble runner.
//
// State is kept internal to Board via a counter that resets on any change.
func (b *Board[F]) Equilibrium() EquilibriumWitness {
	b.mu.Lock()
	defer b.mu.Unlock()

	quiet := time.Since(b.lastChg) >= b.quietFor
	if quiet {
		b.quietCount++
	} else {
		b.quietCount = 0
	}
	stable := b.quietCount >= b.rounds && b.version > 0
	return EquilibriumWitness{
		Stable:    stable,
		QuietFor:  time.Since(b.lastChg),
		Rounds:    b.quietCount,
		LastFacts: len(b.facts),
	}
}

// quietCount is incremented on each Equilibrium call where the board has
// been idle for QuietFor; reset on any Post.
// (declared here via a separate field to keep the Config struct clean.)

// matches implements the minimal pattern language: "*" matches anything,
// otherwise exact-prefix match with the pattern ending in "*", or full
// equality otherwise.
func matches(pattern, key string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(key, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == key
	}
}
