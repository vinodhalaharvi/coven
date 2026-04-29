package controlplane

import (
	"context"
	"testing"
	"time"
)

// TestStubReturnsCleanly verifies the placeholder ControlPlane shuts down
// gracefully when its context is cancelled. Real ControlPlane impls must
// preserve this property.
func TestStubReturnsCleanly(t *testing.T) {
	cp := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- cp.Run(ctx)
	}()

	// Give Run a moment to start, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error after cancel: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestNewReturnsNonNil ensures the constructor never returns a nil
// ControlPlane. Subsequent component-fill-in commits should preserve this.
func TestNewReturnsNonNil(t *testing.T) {
	cp := New(Config{})
	if cp == nil {
		t.Fatal("New returned nil ControlPlane")
	}
}
