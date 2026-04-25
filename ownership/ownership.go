// Package ownership tracks which agent owns each file. Agents that watch
// directories use this to filter out events for files written by other
// agents (e.g., a PackageAgent ignoring .pb.go files written by ProtoGenAgent).
//
// Design: Ownership is a capability — many agents query it, multiple
// implementations are plausible (in-memory now, persistent later). It
// satisfies our rule that interfaces are reserved for capability contracts,
// not for sum types or behavior erasure. The contract is small (3 methods),
// behavior-only, and any implementation is interchangeable for callers.
package ownership

import (
	"path/filepath"
	"sync"
)

// AgentID identifies an owning agent. Mirrors supervisor.AgentID-style strings.
type AgentID string

// Registry is the capability contract.
type Registry interface {
	// Claim records that agent owns path. If path was previously owned by a
	// different agent, that ownership is overwritten and the previous owner
	// is returned in `prev` (zero-value if none).
	Claim(path string, agent AgentID) (prev AgentID, replaced bool)

	// Release removes ownership. Returns false if the path wasn't owned.
	Release(path string) bool

	// Owner returns the current owner of path, or zero/false if unowned.
	Owner(path string) (AgentID, bool)

	// Owned returns all paths currently owned by agent.
	Owned(agent AgentID) []string
}

// InMemory is the default implementation. Safe for concurrent use.
type InMemory struct {
	mu     sync.RWMutex
	owners map[string]AgentID
}

// New constructs an InMemory registry.
func New() *InMemory {
	return &InMemory{owners: make(map[string]AgentID)}
}

// canonicalize returns the path with symlinks resolved when possible.
// On macOS /tmp -> /private/tmp and /var -> /private/var; fsnotify events
// arrive with the resolved path while callers often hold the unresolved
// form. Storing resolved-only keys removes that asymmetry.
//
// If EvalSymlinks fails (e.g. a path that doesn't yet exist on disk —
// which happens when an agent claims a generated file before writing it
// in some flows), the input path is returned unchanged. The asymmetry
// in that case is acceptable: a not-yet-existing file can't have an
// fsnotify event delivered for it anyway.
func canonicalize(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	// Try to canonicalize the parent directory and rejoin — covers the
	// "claim before write" case where the file doesn't exist yet but its
	// directory does.
	dir, base := filepath.Split(path)
	if dir != "" {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, base)
		}
	}
	return path
}

func (r *InMemory) Claim(path string, agent AgentID) (AgentID, bool) {
	path = canonicalize(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, existed := r.owners[path]
	r.owners[path] = agent
	return prev, existed && prev != agent
}

func (r *InMemory) Release(path string) bool {
	path = canonicalize(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.owners[path]; !ok {
		return false
	}
	delete(r.owners, path)
	return true
}

func (r *InMemory) Owner(path string) (AgentID, bool) {
	path = canonicalize(path)
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.owners[path]
	return a, ok
}

func (r *InMemory) Owned(agent AgentID) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for p, a := range r.owners {
		if a == agent {
			out = append(out, p)
		}
	}
	return out
}

// IgnoreForeignFiles returns an FSConfig.IgnoreFile hook that returns true
// whenever the path is owned by an agent other than `self`. The hook is
// safe to call concurrently with Claim/Release.
//
// This is the function-typed seam: agents wire it into their FSConfig with
// no awareness of the registry's concrete type — only that it satisfies
// Registry. The closure captures registry + self at construction time.
func IgnoreForeignFiles(reg Registry, self AgentID) func(path string) bool {
	return func(path string) bool {
		owner, owned := reg.Owner(path)
		if !owned {
			return false // unowned files are fair game for any watcher
		}
		return owner != self
	}
}
