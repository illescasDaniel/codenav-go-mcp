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

// stdioHarness runs the server over pipes and reads its replies one line at a time.
type stdioHarness struct {
	in  io.WriteCloser
	out *bufio.Reader
}

func startStdio(t *testing.T) *stdioHarness {
	t.Helper()
	t.Setenv(EnvWorkspace, t.TempDir())
	srv, mgr := New("test")
	t.Cleanup(func() { mgr.Close() })
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { inW.Close() })
	go func() { _ = srv.Run(ctx, NewStdioTransport(inR, outW)) }()
	return &stdioHarness{in: inW, out: bufio.NewReader(outR)}
}

func (h *stdioHarness) send(t *testing.T, line string) {
	t.Helper()
	if _, err := h.in.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
}

func (h *stdioHarness) reply(t *testing.T) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		l, err := h.out.ReadString('\n')
		if err != nil {
			l = "read error: " + err.Error()
		}
		ch <- l
	}()
	select {
	case l := <-ch:
		return l
	case <-time.After(20 * time.Second):
		t.Fatal("no reply")
		return ""
	}
}

// BUG-PROTO-001: JSON-RPC batching was removed in 2025-06-18 and the SDK ends the session on a batch it refuses,
// so from then on every array is answered with -32600 (id null) and not forwarded. Under 2025-03-26 a valid batch
// is forwarded and answered as an array. An empty array or one with a non-object element is -32600 under every
// version. In each case the server keeps serving afterwards.
func TestStdioBatchFrames(t *testing.T) {
	ping1 := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	pingBoth := ping1 + `,{"jsonrpc":"2.0","id":2,"method":"ping"}`
	notif := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":77}}`
	for _, tc := range []struct {
		name, version, batch string
		forwarded            bool // answered as a batch array rather than refused
	}{
		{"empty array, 2025-06-18", "2025-06-18", `[]`, false},
		{"non-object element, 2025-06-18", "2025-06-18", `[1]`, false},
		{"one ping, 2025-06-18", "2025-06-18", "[" + ping1 + "]", false},
		{"two pings, 2025-06-18", "2025-06-18", "[" + pingBoth + "]", false},
		{"two pings, unknown future version", "2099-01-01", "[" + pingBoth + "]", false},
		{"empty array, 2025-03-26", "2025-03-26", `[]`, false},
		{"array element, 2025-03-26", "2025-03-26", `[[]]`, false},
		{"non-object element, 2025-03-26", "2025-03-26", `[1]`, false},
		{"two pings, 2025-03-26", "2025-03-26", "[" + pingBoth + "]", true},
		{"element without jsonrpc, 2025-03-26", "2025-03-26", `[{"id":1,"method":"ping"}]`, false},
		{"bad id type, 2025-03-26", "2025-03-26", `[{"jsonrpc":"2.0","id":[1],"method":"ping"}]`, false},
		{"duplicate request ids, 2025-03-26", "2025-03-26", "[" + ping1 + "," + ping1 + "]", false},
		{"two notifications, 2025-03-26", "2025-03-26", "[" + notif + "," + notif + "]", false},
		{"empty array, before initialize", "", `[]`, false},
		{"two pings, before initialize", "", "[" + pingBoth + "]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startStdio(t)
			if tc.version != "" { // "" leaves the session before initialize
				h.send(t, `{"jsonrpc":"2.0","id":100,"method":"initialize","params":{"protocolVersion":"`+tc.version+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
				if m := parseReply(t, h.reply(t)); m["id"] != float64(100) || m["error"] != nil {
					t.Fatalf("initialize: %v", m)
				}
				h.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			}
			h.send(t, tc.batch)
			raw := h.reply(t)
			if tc.forwarded {
				var replies []map[string]any
				if err := json.Unmarshal([]byte(raw), &replies); err != nil || len(replies) != 2 {
					t.Fatalf("want a 2-reply batch array, got %q (%v)", raw, err)
				}
				for i, r := range replies {
					if r["id"] != float64(i+1) || r["result"] == nil {
						t.Errorf("batch reply %d: %v", i, r)
					}
				}
			} else {
				m := parseReply(t, raw)
				e, _ := m["error"].(map[string]any)
				if m["id"] != nil || e == nil || e["code"] != float64(-32600) {
					t.Fatalf("want -32600 with id null, got %q", raw)
				}
			}
			h.send(t, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
			if m := parseReply(t, h.reply(t)); m["id"] != float64(9) || m["error"] != nil {
				t.Fatalf("server stopped serving after the batch: %v", m)
			}
		})
	}
}

// BUG-PROTO-001: the SDK never answers a notification, so after a forwarded batch held one it keeps the notification's
// zero id and fails the next batch that holds one, which ends the server. That batch gets -32600 instead.
func TestStdioBatchSecondNotification(t *testing.T) {
	notif := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":77}}`
	h := startStdio(t)
	h.send(t, `{"jsonrpc":"2.0","id":100,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if m := parseReply(t, h.reply(t)); m["id"] != float64(100) || m["error"] != nil {
		t.Fatalf("initialize: %v", m)
	}
	h.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(t, "["+notif+"]") // forwarded; the SDK answers nothing
	h.send(t, "["+notif+"]")
	m := parseReply(t, h.reply(t))
	e, _ := m["error"].(map[string]any)
	if m["id"] != nil || e == nil || e["code"] != float64(-32600) {
		t.Fatalf("second notification batch: want -32600 with id null, got %v", m)
	}
	h.send(t, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if m := parseReply(t, h.reply(t)); m["id"] != float64(9) || m["error"] != nil {
		t.Fatalf("server stopped serving after the batch: %v", m)
	}
}

// BUG-PROTO-001 (single objects): an object the SDK cannot decode as a JSON-RPC message ends the session as a batch
// element would. It is answered with -32600, echoing its id when that is a valid id, and the server keeps serving.
func TestStdioRefusesUndecodableObjects(t *testing.T) {
	for _, version := range []string{"2025-06-18", "2025-03-26"} {
		for _, tc := range []struct {
			name, line string
			wantID     any // the echoed id; nil for null
		}{
			{"no jsonrpc", `{"id":1,"method":"ping"}`, float64(1)},
			{"jsonrpc 1.0", `{"jsonrpc":"1.0","id":2,"method":"ping"}`, float64(2)},
			{"bad id type", `{"jsonrpc":"2.0","id":[1],"method":"ping"}`, nil},
			{"no id and no method", `{"jsonrpc":"2.0"}`, nil},
		} {
			t.Run(version+" "+tc.name, func(t *testing.T) {
				h := startStdio(t)
				h.send(t, `{"jsonrpc":"2.0","id":100,"method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
				if m := parseReply(t, h.reply(t)); m["id"] != float64(100) || m["error"] != nil {
					t.Fatalf("initialize: %v", m)
				}
				h.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
				h.send(t, tc.line)
				m := parseReply(t, h.reply(t))
				e, _ := m["error"].(map[string]any)
				if m["id"] != tc.wantID || e == nil || e["code"] != float64(-32600) {
					t.Fatalf("want -32600 with id %v, got %v", tc.wantID, m)
				}
				h.send(t, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
				if m := parseReply(t, h.reply(t)); m["id"] != float64(9) || m["error"] != nil {
					t.Fatalf("server stopped serving after the object: %v", m)
				}
			})
		}
	}
}

func parseReply(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("bad reply %q: %v", line, err)
	}
	return m
}
