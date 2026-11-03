package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxFrame bounds one inbound line, like the SDK's own default.
const maxFrame = mcp.DefaultMaxLineLength

// NewStdioTransport is the stdio transport with a guard in front of the SDK's: a line that is not JSON
// (a truncated message, a bare Infinity or NaN) would make the SDK's decoder fail and the server exit.
// Here such a line is dropped and answered with a JSON-RPC -32700 parse error (id null), a line that is
// JSON but no object or array with -32600, and the server keeps serving the lines that follow. An object or a
// JSON-RPC batch the SDK cannot take (see objectProblem and frameReader.batchProblem) is answered with -32600
// the same way, since the SDK would end the session on it.
func NewStdioTransport(in io.Reader, out io.Writer) *mcp.IOTransport {
	w := &lockedWriter{w: out}
	return &mcp.IOTransport{
		Reader:        io.NopCloser(&frameReader{r: bufio.NewReader(in), reply: w}),
		Writer:        w,
		MaxLineLength: -1, // frameReader already caps a line
	}
}

// lockedWriter serialises the SDK's writes with the parse-error replies; each is one Write call.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *lockedWriter) Close() error { return nil }

type frameReader struct {
	r       *bufio.Reader
	reply   io.Writer
	pending []byte
	err     error
	// noBatch is set once an initialize request names a protocol version without JSON-RPC batches. Until then
	// the SDK's own default (batches allowed) applies.
	noBatch bool
	// notifBatched is set once a forwarded batch held a notification. The SDK never answers a notification, so
	// it keeps the zero id of notifications in its batch table and refuses the next batch that holds one.
	notifBatched bool
}

func (f *frameReader) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		line, err := f.readLine()
		f.err = err
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if code, msg, id := f.frameProblem(line); code != 0 {
			f.replyError(code, msg, id)
			continue
		}
		f.noteInitialize(line)
		f.pending = append(line, '\n')
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

// readLine returns the next line without its newline; a line over maxFrame is consumed and returned as
// nil with errTooLong folded into a parse problem by the caller.
func (f *frameReader) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := f.r.ReadSlice('\n')
		if len(line)+len(chunk) > maxFrame {
			line = []byte("\x00too long")
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = f.r.ReadSlice('\n')
			}
			return line, nonBufferErr(err)
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(line, "\r\n"), nonBufferErr(err)
	}
}

func nonBufferErr(err error) error {
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil
	}
	return err
}

// JSON-RPC error codes of the replies.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
)

// frameProblem classifies a line the SDK could not handle: 0 when the SDK may read it. id is the id the reply
// carries (nil for null).
func (f *frameReader) frameProblem(line []byte) (code int, msg string, id json.RawMessage) {
	if bytes.Equal(line, []byte("\x00too long")) {
		return codeParseError, "Parse error: message is too large", nil
	}
	if !json.Valid(line) {
		return codeParseError, "Parse error: the message is not valid JSON", nil
	}
	switch bytes.TrimSpace(line)[0] {
	case '{':
		return objectProblem(line)
	case '[':
		c, m := f.batchProblem(line)
		return c, m, nil
	}
	return codeInvalidRequest, "Invalid Request: a JSON-RPC message is a JSON object", nil
}

// objectProblem refuses an object the SDK cannot decode as a JSON-RPC message (no "jsonrpc": "2.0", a bad id, no
// id and no method): its decoder fails and the server exits, as it would on a batch element. The reply echoes the
// object's id when that is a valid one.
func objectProblem(line []byte) (int, string, json.RawMessage) {
	if _, err := jsonrpc.DecodeMessage(line); err == nil {
		return 0, "", nil
	}
	const msg = "Invalid Request: a JSON-RPC message is not valid"
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(line, &req)
	var v any
	if json.Unmarshal(req.ID, &v) == nil && v != nil {
		if _, err := jsonrpc.MakeID(v); err == nil {
			return codeInvalidRequest, msg, req.ID
		}
	}
	return codeInvalidRequest, msg, nil
}

// batchProblem refuses a JSON-RPC batch the SDK would fail on: its failure ends Run, so the server exits. Once
// initialize named a version without batches (2025-06-18 and later, where the spec removed them) every array
// is refused. Under the older versions an empty array, an element the SDK cannot decode, a repeated request id
// or a notification after an earlier batch held one is refused; any other batch is forwarded and answered as
// a batch. The checks follow the SDK's own batch reader (its ioConn.Read and addBatch).
func (f *frameReader) batchProblem(line []byte) (int, string) {
	if f.noBatch {
		return codeInvalidRequest, "Invalid Request: JSON-RPC batching is not supported in protocol 2025-06-18 and later"
	}
	var elems []json.RawMessage
	_ = json.Unmarshal(line, &elems) // line is valid JSON and starts with '['
	if len(elems) == 0 {
		return codeInvalidRequest, "Invalid Request: an empty batch"
	}
	seen := map[jsonrpc.ID]bool{}
	notif := false
	for _, e := range elems {
		if bytes.TrimSpace(e)[0] != '{' {
			return codeInvalidRequest, "Invalid Request: a batch element is not a JSON object"
		}
		msg, err := jsonrpc.DecodeMessage(e)
		if err != nil {
			return codeInvalidRequest, "Invalid Request: a batch element is not a JSON-RPC message"
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok {
			continue
		}
		// A notification has no id, so the SDK keys it by the zero id, and two of them clash in one batch.
		if seen[req.ID] || (!req.ID.IsValid() && f.notifBatched) {
			return codeInvalidRequest, "Invalid Request: a batch repeats a request id"
		}
		seen[req.ID] = true
		notif = notif || !req.ID.IsValid()
	}
	f.notifBatched = f.notifBatched || notif
	return 0, ""
}

// noteInitialize records the protocol version an initialize request names. The SDK negotiates a version it does
// not know, or a newer one, down to 2025-11-25, which has no batches either, so only 2024-11-05 and 2025-03-26
// keep them. Any such initialize turns batches off for good; that also covers a retry the SDK accepts after a
// failed initialize, at the cost of refusing a batch the SDK could have taken.
func (f *frameReader) noteInitialize(line []byte) {
	if f.noBatch {
		return
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil || req.Method != "initialize" {
		return
	}
	if v := req.Params.ProtocolVersion; v != "2024-11-05" && v != "2025-03-26" {
		f.noBatch = true
	}
}

func (f *frameReader) replyError(code int, msg string, id json.RawMessage) {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   map[string]any  `json:"error"`
	}{"2.0", id, map[string]any{"code": code, "message": msg}})
	_, _ = f.reply.Write(append(b, '\n'))
}
