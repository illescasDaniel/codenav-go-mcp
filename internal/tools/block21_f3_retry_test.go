package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/notices"
)

// b21fHost counts the Acquire calls; the n-th workspace is named /ws<n>, and acquire number failAt (1-based) fails.
type b21fHost struct {
	acquires int
	failAt   int
}

func (h *b21fHost) Acquire(context.Context, *mcp.ServerSession) (*Workspace, error) {
	h.acquires++
	if h.acquires == h.failAt {
		return nil, errors.New("cannot start the language server")
	}
	return &Workspace{Root: fmt.Sprintf("/ws%d", h.acquires)}, nil
}
func (h *b21fHost) Notes() *notices.Board        { return nil }
func (h *b21fHost) Explain(source string) string { return "" }

// b21fReads returns a read whose n-th attempt fails with errs[n] (nil: succeeds with "ok<n>"); it records the roots it saw.
func b21fReads(errs ...error) (fn func(context.Context, *Workspace, struct{}) (string, error), seen *[]string) {
	var roots []string
	fn = func(_ context.Context, w *Workspace, _ struct{}) (string, error) {
		roots = append(roots, w.Root)
		n := len(roots) - 1
		if n < len(errs) && errs[n] != nil {
			return "", errs[n]
		}
		return fmt.Sprintf("ok%d", n), nil
	}
	return fn, &roots
}

// ENV-005 part 2: a read that lost its language server is answered by a fresh one.
func TestBlock21F3_RetryAfterRestartAnswersFromFreshServer(t *testing.T) {
	h := &b21fHost{}
	fn, seen := b21fReads(&lsp.RequestError{Method: "textDocument/hover", Code: -1, Message: "server stopped"})
	out, w, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn)
	if err != nil || out != "ok1" {
		t.Fatalf("out=%q err=%v, want the second attempt's answer", out, err)
	}
	if h.acquires != 2 {
		t.Errorf("Acquire called %d times, want 2", h.acquires)
	}
	if len(*seen) != 2 || (*seen)[0] != "/ws1" || (*seen)[1] != "/ws2" {
		t.Errorf("roots tried = %v, want the old and the fresh workspace", *seen)
	}
	if w == nil || w.Root != "/ws2" {
		t.Errorf("returned workspace = %+v, want the fresh one", w)
	}
}

// A gopls that exited while the request was pending is restart-class too.
func TestBlock21F3_RetryAfterExitedServer(t *testing.T) {
	h := &b21fHost{}
	fn, _ := b21fReads(&lsp.ExitedError{Msg: "gopls exited while a request was pending"})
	out, _, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn)
	if err != nil || out != "ok1" || h.acquires != 2 {
		t.Fatalf("out=%q err=%v acquires=%d, want a retry", out, err, h.acquires)
	}
}

// Any other error is the answer, after one attempt and one Acquire.
func TestBlock21F3_NoRetryOnOtherErrors(t *testing.T) {
	refusal := &lsp.RequestError{Method: "textDocument/hover", Code: -32602, Message: "invalid position"}
	h := &b21fHost{}
	fn, seen := b21fReads(refusal)
	_, _, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn)
	if !errors.Is(err, refusal) && err != error(refusal) {
		t.Fatalf("err = %v, want the refusal as it is", err)
	}
	if h.acquires != 1 || len(*seen) != 1 {
		t.Errorf("acquires=%d attempts=%d, want 1 and 1", h.acquires, len(*seen))
	}
}

// Two restart-class errors in a row: exactly two attempts, and the second error is reported.
func TestBlock21F3_TwoRestartErrorsStopAfterTwoAttempts(t *testing.T) {
	first := &lsp.RequestError{Code: -1, Message: "first"}
	second := &lsp.ExitedError{Msg: "second"}
	h := &b21fHost{}
	fn, seen := b21fReads(first, second, nil)
	_, _, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn)
	if err == nil || err.Error() != "second" {
		t.Fatalf("err = %v, want the second error", err)
	}
	if h.acquires != 2 || len(*seen) != 2 {
		t.Errorf("acquires=%d attempts=%d, want 2 and 2 (no third attempt)", h.acquires, len(*seen))
	}
}

// A cancelled context is not retried.
func TestBlock21F3_CancelledContextNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &b21fHost{}
	attempts := 0
	fn := func(context.Context, *Workspace, struct{}) (string, error) {
		attempts++
		cancel()
		return "", &lsp.RequestError{Code: -1, Message: "stopped"}
	}
	_, _, err := retryAfterRestart(ctx, h, nil, struct{}{}, fn)
	if err == nil || attempts != 1 || h.acquires != 1 {
		t.Fatalf("err=%v attempts=%d acquires=%d, want one attempt and no retry", err, attempts, h.acquires)
	}
}

// When the fresh server cannot be started, that failure is the answer and the read does not run again.
func TestBlock21F3_RetryAcquireFailureReported(t *testing.T) {
	h := &b21fHost{failAt: 2}
	fn, seen := b21fReads(&lsp.RequestError{Code: -1, Message: "stopped"})
	_, w, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn)
	if err == nil || err.Error() != "cannot start the language server" || w != nil {
		t.Fatalf("err=%v w=%v, want the acquire failure", err, w)
	}
	if len(*seen) != 1 {
		t.Errorf("attempts = %d, want 1", len(*seen))
	}
}

// A failure of the first Acquire is returned as it is, with no read.
func TestBlock21F3_FirstAcquireFailureNoRead(t *testing.T) {
	h := &b21fHost{failAt: 1}
	fn, seen := b21fReads()
	_, _, err := retryAfterRestart(context.Background(), h, nil, struct{}{}, fn)
	if err == nil || len(*seen) != 0 || h.acquires != 1 {
		t.Fatalf("err=%v attempts=%d acquires=%d", err, len(*seen), h.acquires)
	}
}
