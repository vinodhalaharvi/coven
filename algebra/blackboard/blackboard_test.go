package blackboard

import (
	"context"
	"sync"
	"testing"
	"time"
)

type testFact struct {
	Content string
}

func TestPostGet(t *testing.T) {
	b := New[testFact](Config{})
	v := b.Post("pkg:a", testFact{Content: "hello"}, "agent-a")
	if v != 1 {
		t.Fatalf("first post version should be 1, got %d", v)
	}
	f, ok := b.Get("pkg:a")
	if !ok {
		t.Fatal("fact not found")
	}
	if f.Value.Content != "hello" {
		t.Fatalf("unexpected content: %q", f.Value.Content)
	}
	if f.Author != "agent-a" {
		t.Fatalf("unexpected author: %q", f.Author)
	}
}

func TestList(t *testing.T) {
	b := New[testFact](Config{})
	b.Post("pkg:a", testFact{"A"}, "x")
	b.Post("pkg:b", testFact{"B"}, "x")
	b.Post("other:c", testFact{"C"}, "x")

	got := b.List("pkg:*")
	if len(got) != 2 {
		t.Fatalf("want 2 facts, got %d", len(got))
	}
}

func TestSubscribe_Receives(t *testing.T) {
	b := New[testFact](Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := b.Subscribe(ctx, "pkg:*", 8)

	var received []string
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		for f := range ch {
			mu.Lock()
			received = append(received, f.Value.Content)
			if len(received) == 2 {
				close(done)
				mu.Unlock()
				return
			}
			mu.Unlock()
		}
	}()

	b.Post("pkg:a", testFact{"A"}, "x")
	b.Post("pkg:b", testFact{"B"}, "x")
	b.Post("other:c", testFact{"C"}, "x") // should not be received

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("did not receive expected facts")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("got %d facts, want 2", len(received))
	}
}

func TestSubscribe_Cancel(t *testing.T) {
	b := New[testFact](Config{})
	ctx, cancel := context.WithCancel(context.Background())

	ch, _ := b.Subscribe(ctx, "*", 4)
	cancel()

	// Give cleanup goroutine a moment to mark the sub as closed.
	time.Sleep(50 * time.Millisecond)

	// Posts after cancel should NOT be delivered. The channel is intentionally
	// not closed (avoids races with concurrent Posts), but the subscription is
	// flagged inactive and Post skips it.
	for i := 0; i < 5; i++ {
		b.Post("k", testFact{"v"}, "x")
	}

	select {
	case f := <-ch:
		t.Fatalf("got fact after cancel: %+v", f)
	case <-time.After(100 * time.Millisecond):
		// Good — no delivery after cancel.
	}
}

func TestEquilibrium_DetectsQuiet(t *testing.T) {
	b := New[testFact](Config{QuietFor: 50 * time.Millisecond, Rounds: 3})
	b.Post("pkg:a", testFact{"A"}, "x")

	// Immediately: not quiet yet.
	w := b.Equilibrium()
	if w.Stable {
		t.Fatal("should not be stable immediately after post")
	}

	// Wait past quietFor, then call Equilibrium 3 times.
	time.Sleep(80 * time.Millisecond)
	for i := 1; i <= 3; i++ {
		w = b.Equilibrium()
		if i < 3 && w.Stable {
			t.Fatalf("stable too early at round %d", i)
		}
	}
	if !w.Stable {
		t.Fatalf("should be stable after 3 quiet rounds, got %+v", w)
	}
	if w.LastFacts != 1 {
		t.Fatalf("LastFacts = %d, want 1", w.LastFacts)
	}
}

func TestEquilibrium_ResetOnChange(t *testing.T) {
	b := New[testFact](Config{QuietFor: 30 * time.Millisecond, Rounds: 2})
	b.Post("pkg:a", testFact{"A"}, "x")
	time.Sleep(50 * time.Millisecond)

	b.Equilibrium() // round 1
	// Now post — should reset.
	b.Post("pkg:b", testFact{"B"}, "x")
	time.Sleep(50 * time.Millisecond)

	w := b.Equilibrium() // should be round 1 again, not round 2
	if w.Stable {
		t.Fatal("should have reset on post")
	}
	if w.Rounds != 1 {
		t.Fatalf("Rounds = %d, want 1 after reset", w.Rounds)
	}
}

func TestEquilibrium_NotStableBeforeAnyPost(t *testing.T) {
	b := New[testFact](Config{QuietFor: 10 * time.Millisecond, Rounds: 1})
	// Wait out the quiet period without posting anything.
	time.Sleep(30 * time.Millisecond)
	w := b.Equilibrium()
	if w.Stable {
		t.Fatal("empty blackboard should not report Stable=true — it's trivially quiet, not converged")
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		pattern, key string
		want         bool
	}{
		{"*", "anything", true},
		{"", "anything", true},
		{"pkg:*", "pkg:foo", true},
		{"pkg:*", "other:foo", false},
		{"pkg:foo", "pkg:foo", true},
		{"pkg:foo", "pkg:bar", false},
	}
	for _, c := range cases {
		if got := matches(c.pattern, c.key); got != c.want {
			t.Errorf("matches(%q,%q) = %v, want %v", c.pattern, c.key, got, c.want)
		}
	}
}

func TestConcurrentPosts(t *testing.T) {
	b := New[testFact](Config{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Post("pkg:x", testFact{"val"}, "author")
		}(i)
	}
	wg.Wait()

	// Version should equal 100 (one per post).
	f, _ := b.Get("pkg:x")
	if f.Version != 100 {
		t.Fatalf("version = %d, want 100", f.Version)
	}
}
