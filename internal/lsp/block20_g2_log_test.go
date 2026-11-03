package lsp

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a log sink safe for the exit goroutine to write while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

// LEAK-009 follow-up: an unexpected exit logs the server's stderr tail (the agent-facing text does not).
func TestBlock20G2UnexpectedExitLogsStderrTail(t *testing.T) {
	buf := captureLog(t)
	c, _, _ := newFakeClient(t, "longline,wedge") // writes a 100 KB stderr line, answers initialize, then hangs
	startClient(t, c)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.stderr)
		c.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.kill()
	select {
	case <-c.exitedChan():
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit after kill")
	}
	got := buf.String()
	if !strings.Contains(got, "language server exited unexpectedly") || !strings.Contains(got, strings.Repeat("x", 100)) {
		t.Errorf("unexpected exit did not log the stderr tail:\n%.300s", got)
	}
}

// A stop that Stop asked for is expected: nothing is logged.
func TestBlock20G2RequestedStopIsNotLogged(t *testing.T) {
	buf := captureLog(t)
	c, _, _ := newFakeClient(t, "longline")
	startClient(t, c)
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Contains(got, "exited unexpectedly") {
		t.Errorf("a requested stop was logged as unexpected:\n%.300s", got)
	}
}

// exitedChan reads the exit channel under the client lock.
func (c *Client) exitedChan() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exited
}
