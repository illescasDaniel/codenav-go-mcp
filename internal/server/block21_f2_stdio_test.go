package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Regression (PROTO-004 follow-up): a batch that repeats a request id is refused as one frame with -32600 and id
// null, and none of its requests is forwarded.
func TestBlock21F2_BatchRepeatedIDRefused(t *testing.T) {
	var out bytes.Buffer
	dup := `[{"jsonrpc":"2.0","id":7,"method":"tools/list"},{"jsonrpc":"2.0","id":7,"method":"tools/list"}]`
	tr := NewStdioTransport(strings.NewReader(dup+"\n"), &out)
	buf := make([]byte, 1<<16)
	if n, err := tr.Reader.Read(buf); err != io.EOF || n != 0 {
		t.Fatalf("a batch with a repeated id was forwarded: n=%d err=%v data=%q", n, err, buf[:n])
	}
	m := parseReply(t, out.String())
	e, _ := m["error"].(map[string]any)
	if m["id"] != nil || e == nil || e["code"] != float64(-32600) || e["message"] != "duplicate request id" {
		t.Fatalf("want -32600 \"duplicate request id\" with id null, got %q", out.String())
	}
}

// Regression (PROTO-004 follow-up): a batch that reuses an id still in flight is refused whole. The ids it does
// not repeat are not registered by the refusal, so a later request with one of them is forwarded.
func TestBlock21F2_BatchReusingInFlightIDRefused(t *testing.T) {
	var out bytes.Buffer
	in := g1Req + "\n" +
		`[{"jsonrpc":"2.0","id":5,"method":"tools/list"},{"jsonrpc":"2.0","id":6,"method":"tools/list"}]` + "\n" +
		`{"jsonrpc":"2.0","id":6,"method":"tools/list"}` + "\n"
	tr := NewStdioTransport(strings.NewReader(in), &out)
	buf := make([]byte, 1<<16)
	if n, err := tr.Reader.Read(buf); err != nil || !strings.Contains(string(buf[:n]), `"id":5`) {
		t.Fatalf("first request not forwarded: n=%d err=%v", n, err)
	}
	n, err := tr.Reader.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), `{"jsonrpc":"2.0","id":6,`) {
		t.Fatalf("after the refused batch, id 6 must be forwarded alone; got n=%d err=%v data=%q", n, err, buf[:n])
	}
	m := parseReply(t, out.String())
	e, _ := m["error"].(map[string]any)
	if m["id"] != nil || e == nil || e["code"] != float64(-32600) {
		t.Fatalf("want -32600 with id null for the batch, got %q", out.String())
	}
}

// Regression (PROTO-004 follow-up): the SDK answers a batch as one array once every request in it is answered; the
// ids of that batch are then free again. Before the array was released, a later request reusing id 1 was refused
// as a duplicate for good.
func TestBlock21F2_BatchIDsReleasedByArrayAnswer(t *testing.T) {
	h := startStdio(t)
	h.send(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`)
	var replies []map[string]any
	if err := json.Unmarshal([]byte(h.reply(t)), &replies); err != nil || len(replies) != 2 {
		t.Fatalf("want a 2-reply batch array: %v", err)
	}
	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if m := parseReply(t, h.reply(t)); m["id"] != float64(1) || m["error"] != nil {
		t.Fatalf("id 1 was not free after the batch was answered: %v", m)
	}
}

// Regression (PROTO-004 follow-up, review): a batch refused for an in-flight id never reaches the SDK, so it must
// not mark the session as having held a notification. A later batch that holds a notification and a fresh id is
// forwarded; before the fix the refused batch's notification refused it too.
func TestBlock21F2_RefusedBatchWithNotificationNotNoted(t *testing.T) {
	var out bytes.Buffer
	notif := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":77}}`
	in := g1Req + "\n" +
		"[" + notif + `,{"jsonrpc":"2.0","id":5,"method":"tools/list"}]` + "\n" +
		"[" + notif + `,{"jsonrpc":"2.0","id":9,"method":"tools/list"}]` + "\n"
	tr := NewStdioTransport(strings.NewReader(in), &out)
	buf := make([]byte, 1<<16)
	if n, err := tr.Reader.Read(buf); err != nil || !strings.Contains(string(buf[:n]), `"id":5`) {
		t.Fatalf("first request not forwarded: n=%d err=%v", n, err)
	}
	n, err := tr.Reader.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "[") || !strings.Contains(string(buf[:n]), `"id":9`) {
		t.Fatalf("the batch with a notification and id 9 must be forwarded after a refused batch; got n=%d err=%v data=%q", n, err, buf[:n])
	}
	m := parseReply(t, out.String())
	e, _ := m["error"].(map[string]any)
	if m["id"] != nil || e == nil || e["message"] != "duplicate request id" {
		t.Fatalf("the refused batch must be answered with duplicate request id, got %q", out.String())
	}
}
