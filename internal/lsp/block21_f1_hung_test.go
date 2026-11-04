package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// b21f1HelperEnv selects the mode of the helper server below (empty: the helper test is skipped).
const b21f1HelperEnv = "CODENAV_B21F1_HELPER"

// TestBlock21F1Helper is the language server the hung-server tests start. Modes: "silent" answers initialize and
// then nothing; "chatty" also sends $/progress every 20 ms and answers nothing; "chatty-symbol" is chatty and
// answers workspace/symbol with null.
func TestBlock21F1Helper(t *testing.T) {
	mode := os.Getenv(b21f1HelperEnv)
	if mode == "" {
		t.Skip("helper process for the hung-server tests")
	}
	b21fServe(mode)
	os.Exit(0)
}

func b21fServe(mode string) {
	var mu sync.Mutex
	write := func(body string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	in := bufio.NewReader(os.Stdin)
	started := false
	for {
		body, err := readFrame(in)
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		switch {
		case msg.Method == "exit":
			os.Exit(0)
		case msg.Method == "initialize":
			write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"capabilities":{}}}`, msg.ID))
			if strings.HasPrefix(mode, "chatty") && !started {
				started = true
				go func() {
					for {
						write(`{"jsonrpc":"2.0","method":"$/progress","params":{}}`)
						time.Sleep(20 * time.Millisecond)
					}
				}()
			}
		case msg.Method == "workspace/symbol" && mode == "chatty-symbol":
			write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, msg.ID))
		}
	}
}

// b21fClient starts the helper server in mode with a shortened request timeout and collects its notices.
func b21fClient(t *testing.T, mode string) (*Client, func() []string) {
	t.Helper()
	old := requestTimeout
	requestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { requestTimeout = old })
	t.Setenv(b21f1HelperEnv, mode)
	var mu sync.Mutex
	var notes []string
	c := New(Options{Root: t.TempDir(), Command: []string{os.Args[0], "-test.run=^TestBlock21F1Helper$"}, Language: "go",
		OnNotice: func(s string) {
			mu.Lock()
			defer mu.Unlock()
			notes = append(notes, s)
		}})
	startClient(t, c)
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notes...)
	}
}

// b21fRequest is a request with no deadline of its own, so the client's timer applies.
func b21fRequest(c *Client, method string) error {
	_, err := c.Request(context.Background(), method, map[string]any{})
	return err
}

// A silent server (no message since the request) is ended by the first timeout: the caller gets the Code -1
// error, Alive is already false when the error arrives, and one notice is posted.
func TestBlock21F1_SilentServerEndedAtFirstTimeout(t *testing.T) {
	c, notes := b21fClient(t, "silent")
	began := time.Now()
	err := b21fRequest(c, "workspace/symbol")
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -1 {
		t.Fatalf("want a Code -1 RequestError after the timeout, got %v", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("request on a silent server took %v", took)
	}
	if c.Alive() {
		t.Error("Alive is true right after the hung-server error: the next call would reuse the dead server")
	}
	if strings.Contains(re.Message, os.Args[0]) || !strings.Contains(re.Message, "restarted; retry") {
		t.Errorf("message %q must name no binary and advise a retry", re.Message)
	}
	if got := notes(); len(got) != 1 || !strings.Contains(got[0], "did not answer") || !strings.Contains(got[0], "restarted") {
		t.Errorf("want one restart notice, got %q", got)
	}
}

// A server that keeps sending messages but never answers is busy, not stuck: the first two timeouts only
// return the timeout error and leave the process alone; the third ends it.
func TestBlock21F1_ChattyServerEndedAtThirdTimeout(t *testing.T) {
	c, notes := b21fClient(t, "chatty")
	for i := 1; i <= busyTimeouts-1; i++ {
		err := b21fRequest(c, "textDocument/hover")
		var re *RequestError
		if err == nil || errors.As(err, &re) || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("timeout %d: want the plain timeout error, got %v", i, err)
		}
		if !c.Alive() {
			t.Fatalf("timeout %d: a server that still talks was ended", i)
		}
	}
	if got := notes(); len(got) != 0 {
		t.Fatalf("no notice before the third timeout, got %q", got)
	}
	err := b21fRequest(c, "textDocument/hover")
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -1 {
		t.Fatalf("third timeout: want a Code -1 RequestError, got %v", err)
	}
	if c.Alive() {
		t.Error("Alive is true after the third timeout on a chatty server")
	}
	if got := notes(); len(got) != 1 {
		t.Errorf("want exactly one restart notice, got %q", got)
	}
}

// Three requests that time out together on a server that keeps talking are one stall, not three: the server
// stays up and no notice is posted. Later stalls count again, so the third separate one ends it.
func TestBlock21F1_ConcurrentTimeoutsCountAsOneStall(t *testing.T) {
	c, notes := b21fClient(t, "chatty")
	const together = busyTimeouts
	errs := make([]error, together)
	var wg sync.WaitGroup
	for i := 0; i < together; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = b21fRequest(c, "textDocument/hover")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		var re *RequestError
		if err == nil || errors.As(err, &re) || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("concurrent request %d: want the plain timeout error, got %v", i, err)
		}
	}
	if !c.Alive() {
		t.Fatal("three timeouts that fell in one stall ended a server that still talks")
	}
	if got := notes(); len(got) != 0 {
		t.Fatalf("no notice for one stall, got %q", got)
	}
	if err := b21fRequest(c, "textDocument/hover"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("second stall: want the plain timeout error, got %v", err)
	}
	if !c.Alive() {
		t.Fatal("the second stall ended the server; it should wait for the third")
	}
	err := b21fRequest(c, "textDocument/hover")
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -1 {
		t.Fatalf("third stall: want a Code -1 RequestError, got %v", err)
	}
	if c.Alive() {
		t.Error("Alive is true after the third stall on a chatty server")
	}
}

// An answer resets the count of timeouts in a row: two timeouts, an answered request, then two more timeouts
// leave the server running. Without the reset the fourth timeout overall would end it.
func TestBlock21F1_AnswerResetsTimeoutCount(t *testing.T) {
	c, _ := b21fClient(t, "chatty-symbol")
	for i := 0; i < busyTimeouts-1; i++ {
		_ = b21fRequest(c, "textDocument/hover")
	}
	if err := b21fRequest(c, "workspace/symbol"); err != nil {
		t.Fatalf("answered request failed: %v", err)
	}
	for i := 0; i < busyTimeouts-1; i++ {
		_ = b21fRequest(c, "textDocument/hover")
	}
	if !c.Alive() {
		t.Fatal("the server was ended although an answer reset the timeouts in a row")
	}
	err := b21fRequest(c, "textDocument/hover")
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -1 {
		t.Fatalf("want the Code -1 error at the third timeout after the answer, got %v", err)
	}
}
