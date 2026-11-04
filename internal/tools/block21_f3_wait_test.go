package tools

import (
	"context"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// A retry waits for the failed client after a lost request, but not after an exited server (it has stopped already).
func TestBlock21F3_RetryWaitsForLostClientOnly(t *testing.T) {
	old := awaitStoppedFn
	t.Cleanup(func() { awaitStoppedFn = old })
	h := &b21fHost{}
	var atAcquire []int
	awaitStoppedFn = func(context.Context, *Workspace) { atAcquire = append(atAcquire, h.acquires) }

	fn, _ := b21fReads(&lsp.RequestError{Code: -1, Message: "stopped"})
	if _, _, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(atAcquire) != 1 || atAcquire[0] != 1 {
		t.Fatalf("waits at acquire counts %v, want one wait after the first acquire", atAcquire)
	}

	atAcquire = nil
	h = &b21fHost{}
	fn, _ = b21fReads(&lsp.ExitedError{Msg: "gopls exited"})
	if _, _, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(atAcquire) != 0 {
		t.Fatalf("waited after an exited server (%v), want no wait", atAcquire)
	}
}

// awaitGone returns as soon as the client reports that it is not alive.
func TestBlock21F3_AwaitGoneStopsWhenClientStops(t *testing.T) {
	calls := 0
	alive := func() bool { calls++; return calls < 5 }
	start := time.Now()
	awaitGone(context.Background(), alive, 5*time.Second)
	if calls != 5 {
		t.Errorf("alive asked %d times, want 5", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s for a client that stopped at once", elapsed)
	}
}

// awaitGone gives up after its limit when the client stays alive, and when the context ends.
func TestBlock21F3_AwaitGoneBounded(t *testing.T) {
	start := time.Now()
	awaitGone(context.Background(), func() bool { return true }, 60*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("waited %s, want about the 60ms limit", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	awaitGone(ctx, func() bool { return true }, 10*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a cancelled context still waited %s", elapsed)
	}
}

// A workspace without a client has nothing to wait for.
func TestBlock21F3_AwaitStoppedWithoutClient(t *testing.T) {
	start := time.Now()
	awaitStopped(context.Background(), nil)
	awaitStopped(context.Background(), &Workspace{Root: "/x"})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s without a client", elapsed)
	}
}
