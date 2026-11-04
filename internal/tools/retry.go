package tools

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// restartWait bounds how long a retry waits for the failed client to report that it is gone. The client fails its
// pending requests before it marks itself exited, so an Acquire right after the error could still hand back that
// client; the wait closes that window. restartPoll is how often the wait looks.
const (
	restartWait = 200 * time.Millisecond
	restartPoll = 5 * time.Millisecond
)

// awaitStoppedFn is awaitStopped; a test replaces it to see when a retry waits.
var awaitStoppedFn = awaitStopped

// awaitStopped waits until the client of w is no longer alive, for at most restartWait. A workspace without a client
// returns at once, and so does a cancelled context.
func awaitStopped(ctx context.Context, w *Workspace) {
	if w == nil || w.Client == nil {
		return
	}
	awaitGone(ctx, w.Client.Alive, restartWait)
}

// awaitGone polls alive every restartPoll until it reports false, the context ends, or limit passes.
func awaitGone(ctx context.Context, alive func() bool, limit time.Duration) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(restartPoll)
	defer tick.Stop()
	for alive() {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

// restartClass reports whether err says the language server died or stopped answering (the request was lost, not
// refused): a fresh server, started by acquiring the workspace again, may answer the same read.
func restartClass(err error) bool {
	var re *lsp.RequestError
	if errors.As(err, &re) && re.Code == -1 {
		return true
	}
	var ee *lsp.ExitedError
	return errors.As(err, &ee)
}

// retryAfterRestart runs a read on the workspace of the call. When the attempt fails with a restart-class error
// (restartClass), the workspace is acquired again, which starts a fresh server when the old one is dead, and the read
// runs once more; that second outcome is the answer, and there is no third attempt. A cancelled context is not retried.
// After a lost request the old client is given a moment to stop (awaitStopped), except after an exited server, which has
// stopped already. It returns the answer and the workspace that produced it (nil when none could be acquired).
func retryAfterRestart[In any](ctx context.Context, h Host, sess *mcp.ServerSession, in In, fn func(context.Context, *Workspace, In) (string, error)) (string, *Workspace, error) {
	w, err := h.Acquire(ctx, sess)
	if err != nil {
		return "", w, err
	}
	out, err := fn(ctx, w, in)
	if err == nil || !restartClass(err) || ctx.Err() != nil {
		return out, w, err
	}
	var ee *lsp.ExitedError
	if !errors.As(err, &ee) {
		awaitStoppedFn(ctx, w)
	}
	w, err = h.Acquire(ctx, sess)
	if err != nil {
		return "", w, err
	}
	out, err = fn(ctx, w, in)
	return out, w, err
}
