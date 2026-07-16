package handler

import (
	"context"
	"testing"
	"time"
)

// A 1-second timeout that is never disarmed must cancel the context.
func TestRequestTimeoutFires(t *testing.T) {
	ctx, cancel, _ := requestTimeout(context.Background(), 1)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("context not cancelled after timeout elapsed")
	}
}

// Disarming before the deadline must leave the context alive past it —
// this is the streaming case: a generation may outlive any wall-clock
// budget once SSE has started.
func TestRequestTimeoutDisarm(t *testing.T) {
	ctx, cancel, disarm := requestTimeout(context.Background(), 1)
	defer cancel()
	disarm()
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled despite disarm: %v", context.Cause(ctx))
	case <-time.After(1500 * time.Millisecond):
	}
}

// A disarmed exchange must still die with its parent (client
// disconnect) — disarm removes the wall-clock only, not liveness.
func TestRequestTimeoutParentCancelAfterDisarm(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel, disarm := requestTimeout(parent, 3600)
	defer cancel()
	disarm()
	parentCancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("parent cancellation did not propagate after disarm")
	}
}

// The explicit cancel func must work regardless of disarm state.
func TestRequestTimeoutExplicitCancel(t *testing.T) {
	ctx, cancel, disarm := requestTimeout(context.Background(), 3600)
	disarm()
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("explicit cancel did not cancel the context")
	}
}
