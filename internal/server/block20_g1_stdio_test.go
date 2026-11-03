package server

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const g1Req = `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`

// PROTO-004: a second request whose id is still in flight is refused with -32600 and never forwarded.
func TestStdioDuplicateInFlightIDRefused(t *testing.T) {
	var out bytes.Buffer
	tr := NewStdioTransport(strings.NewReader(g1Req+"\n"+g1Req+"\n"), &out)
	buf := make([]byte, 1<<16)
	if n, err := tr.Reader.Read(buf); err != nil || !strings.Contains(string(buf[:n]), `"id":5`) {
		t.Fatalf("first request not forwarded: n=%d err=%v", n, err)
	}
	if n, err := tr.Reader.Read(buf); err != io.EOF || n != 0 {
		t.Fatalf("duplicate was forwarded: n=%d err=%v data=%q", n, err, buf[:n])
	}
	reply := out.String()
	if !strings.Contains(reply, `"code":-32600`) || !strings.Contains(reply, "duplicate request id") || !strings.Contains(reply, `"id":5`) {
		t.Fatalf("no -32600 duplicate reply, got %q", reply)
	}
}

// PROTO-004: once the first request is answered its id is free again.
func TestStdioIDReusableAfterAnswer(t *testing.T) {
	var out bytes.Buffer
	tr := NewStdioTransport(strings.NewReader(g1Req+"\n"+g1Req+"\n"), &out)
	buf := make([]byte, 1<<16)
	if _, err := tr.Reader.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Writer.Write([]byte(`{"jsonrpc":"2.0","id":5,"result":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if n, err := tr.Reader.Read(buf); err != nil || !strings.Contains(string(buf[:n]), `"id":5`) {
		t.Fatalf("reused id not forwarded after its answer: n=%d err=%v", n, err)
	}
	if strings.Contains(out.String(), "duplicate") {
		t.Fatalf("unexpected duplicate reply: %q", out.String())
	}
}

// PROTO-005: a frame that is not valid UTF-8 gets -32700 with id null and is not processed; the frames
// around it still are.
func TestStdioRejectsInvalidUTF8Frame(t *testing.T) {
	const ping1 = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	const ping3 = `{"jsonrpc":"2.0","id":3,"method":"ping"}`
	bad := "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"edit\",\"arguments\":{\"s\":\"h\xffi\"}}}"
	var out bytes.Buffer
	tr := NewStdioTransport(strings.NewReader(ping1+"\n"+bad+"\n"+ping3+"\n"), &out)
	got, err := io.ReadAll(tr.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "\"id\":2") || strings.Contains(string(got), "�") {
		t.Fatalf("invalid frame was processed: %q", got)
	}
	if !strings.Contains(string(got), `"id":1`) || !strings.Contains(string(got), `"id":3`) {
		t.Fatalf("frames around the bad one were not forwarded: %q", got)
	}
	reply := out.String()
	if !strings.Contains(reply, `"code":-32700`) || !strings.Contains(reply, "parse error: invalid UTF-8") || !strings.Contains(reply, `"id":null`) {
		t.Fatalf("no -32700 invalid UTF-8 reply: %q", reply)
	}
}
