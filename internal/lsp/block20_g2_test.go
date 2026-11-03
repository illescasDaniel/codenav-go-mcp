package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// LEAK-009: a request in flight when the server dies gets the isError-worthy exit message, and no binary path.
func TestBlock20G2InFlightRequestSeesRestartMessage(t *testing.T) {
	c, _, _ := newFakeClient(t, "wedge") // answers initialize, then never reads again: the request stays pending
	startClient(t, c)
	done := make(chan error, 1)
	go func() {
		_, err := c.Request(context.Background(), "workspace/symbol", map[string]any{"query": "A"})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.kill()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request was not answered after the server was killed")
	}
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -1 {
		t.Fatalf("want the exit RequestError, got %v", err)
	}
	if strings.Contains(re.Message, os.Args[0]) || strings.Contains(re.Message, "language server exited") {
		t.Errorf("exit message names the binary or the old wording: %q", re.Message)
	}
	if !strings.Contains(re.Message, "restarting; retry") {
		t.Errorf("exit message lacks the restart advice: %q", re.Message)
	}
}

// PROTO-007: a request whose context is cancelled reports the cancel, not a timeout.
func TestBlock20G2CancelledRequestIsNotATimeout(t *testing.T) {
	c, _, _ := newFakeClient(t, "wedge")
	startClient(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := c.Request(ctx, "textDocument/references", map[string]any{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancel reported as a timeout: %v", err)
	}
}

// NAVP-006: the out-of-range message counts lines, not the empty element after a final newline.
func TestBlock20G2LineCountIgnoresPhantomLine(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := New(Options{Root: dir})
	body := strings.Repeat("x\n", 26) + "x" // 27 lines
	cases := map[string]struct {
		text string
		line int
		want string // "" means the position is accepted
	}{
		"final newline, line 28": {body + "\n", 28, "line 28 is outside the file (it has 27 lines); lines are 1-based, so use 1 to 27"},
		"final newline, line 0":  {body + "\n", 0, "line 0 is outside the file (it has 27 lines); lines are 1-based, so use 1 to 27"},
		"final newline, line 27": {body + "\n", 27, ""},
		"no final newline, 28":   {body, 28, "line 28 is outside the file (it has 27 lines); lines are 1-based, so use 1 to 27"},
		"one line with newline":  {"x\n", 2, "line 2 is outside the file (it has 1 line); lines are 1-based, so use 1 to 1"},
		"one line, no newline":   {"x", 2, "line 2 is outside the file (it has 1 line); lines are 1-based, so use 1 to 1"},
		"crlf final newline":     {strings.ReplaceAll(body, "\n", "\r\n") + "\r\n", 28, "line 28 is outside the file (it has 27 lines); lines are 1-based, so use 1 to 27"},
	}
	for name, tc := range cases {
		err := c.checkPosition(write(strings.ReplaceAll(name, " ", "_")+".go", tc.text), tc.line, 1)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", name, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}
