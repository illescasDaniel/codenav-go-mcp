package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// Round 14 (item 10): a malformed line or a bare Infinity on stdin is answered with a JSON-RPC parse error
// (id null) and the server keeps serving the requests that follow, instead of exiting.
func TestStdioSurvivesMalformedLines(t *testing.T) {
	t.Setenv(EnvWorkspace, t.TempDir())
	srv, mgr := New("test")
	defer mgr.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, NewStdioTransport(inR, outW)) }()
	out := bufio.NewReader(outR)
	read := func() map[string]any {
		t.Helper()
		type res struct {
			line string
			err  error
		}
		ch := make(chan res, 1)
		go func() { l, err := out.ReadString('\n'); ch <- res{l, err} }()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("server output ended: %v", r.err)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(r.line), &m); err != nil {
				t.Fatalf("bad reply %q: %v", r.line, err)
			}
			return m
		case <-time.After(20 * time.Second):
			t.Fatal("no reply")
			return nil
		}
	}
	send := func(s string) {
		t.Helper()
		if _, err := inW.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if m := read(); m["id"] != float64(1) || m["error"] != nil {
		t.Fatalf("initialize: %v", m)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	for line, code := range map[string]float64{
		`{"jsonrpc":"2.0","id":2,"method":"ping"`:    -32700,
		`{"jsonrpc":"2.0","id":2,"method":Infinity}`: -32700,
		`Infinity`: -32700,
		`not json`: -32700,
		`42`:       -32600,
	} {
		send(line)
		m := read()
		e, _ := m["error"].(map[string]any)
		if e == nil || e["code"] != code || m["id"] != nil {
			t.Errorf("%q: want error %v with id null, got %v", line, code, m)
		}
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	m := read()
	res, _ := m["result"].(map[string]any)
	if m["id"] != float64(3) || res == nil || !strings.Contains(toJSON(res), "symbol_info") {
		t.Errorf("server stopped serving after the bad lines: %v", m)
	}
	select {
	case err := <-done:
		t.Fatalf("server exited: %v", err)
	default:
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
