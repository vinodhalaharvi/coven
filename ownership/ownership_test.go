package ownership

import (
	"sync"
	"testing"
)

func TestClaim_Owner(t *testing.T) {
	r := New()
	prev, replaced := r.Claim("a.go", "agent1")
	if replaced {
		t.Errorf("first claim shouldn't be a replacement")
	}
	if prev != "" {
		t.Errorf("prev should be empty, got %q", prev)
	}
	owner, ok := r.Owner("a.go")
	if !ok || owner != "agent1" {
		t.Errorf("Owner = %q,%v", owner, ok)
	}
}

func TestClaim_Replaces(t *testing.T) {
	r := New()
	r.Claim("a.go", "agent1")
	prev, replaced := r.Claim("a.go", "agent2")
	if !replaced {
		t.Error("should have been replaced")
	}
	if prev != "agent1" {
		t.Errorf("prev = %q, want agent1", prev)
	}
}

func TestClaim_SameAgentNotReplaced(t *testing.T) {
	r := New()
	r.Claim("a.go", "agent1")
	_, replaced := r.Claim("a.go", "agent1")
	if replaced {
		t.Error("re-claim by same agent shouldn't count as replacement")
	}
}

func TestRelease(t *testing.T) {
	r := New()
	r.Claim("a.go", "agent1")
	if !r.Release("a.go") {
		t.Error("release should return true for known path")
	}
	if _, ok := r.Owner("a.go"); ok {
		t.Error("should not have owner after release")
	}
	if r.Release("nonexistent") {
		t.Error("release of unknown path should return false")
	}
}

func TestOwned(t *testing.T) {
	r := New()
	r.Claim("a.go", "agent1")
	r.Claim("b.go", "agent1")
	r.Claim("c.go", "agent2")

	owned := r.Owned("agent1")
	if len(owned) != 2 {
		t.Errorf("agent1 owns %d, want 2", len(owned))
	}
	owned2 := r.Owned("agent2")
	if len(owned2) != 1 || owned2[0] != "c.go" {
		t.Errorf("agent2 owned = %v, want [c.go]", owned2)
	}
}

func TestIgnoreForeignFiles(t *testing.T) {
	r := New()
	r.Claim("/proj/auth/auth.go", "auth-agent")
	r.Claim("/proj/auth/auth.pb.go", "protogen")

	hook := IgnoreForeignFiles(r, "auth-agent")

	// Owned-by-self: should NOT be ignored.
	if hook("/proj/auth/auth.go") {
		t.Error("self-owned file should not be ignored")
	}
	// Owned-by-other: should be ignored.
	if !hook("/proj/auth/auth.pb.go") {
		t.Error("foreign file should be ignored")
	}
	// Unowned: should NOT be ignored (everyone has equal claim until someone calls Claim).
	if hook("/proj/auth/new.go") {
		t.Error("unowned file should not be ignored")
	}
}

func TestConcurrent(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.Claim("file", "a")
		}(i)
		go func(i int) {
			defer wg.Done()
			r.Owner("file")
		}(i)
	}
	wg.Wait()
	owner, _ := r.Owner("file")
	if owner != "a" {
		t.Errorf("owner = %q, want a", owner)
	}
}
