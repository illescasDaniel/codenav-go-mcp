package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"unicode/utf8"

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
	fr := &frameReader{r: bufio.NewReader(in), reply: w, inflight: &inflightSet{}}
	return &mcp.IOTransport{
		Reader:        io.NopCloser(fr),
		Writer:        &answerWriter{w: w, inflight: fr.inflight},
		MaxLineLength: -1, // frameReader already caps a line
	}
}

// inflightSet holds the ids of the requests read and not yet answered. The SDK keeps requests by id, so a
// second request with an id still in use would replace the first and lose its answer.
type inflightSet struct {
	mu  sync.Mutex
	ids map[jsonrpc.ID]bool
}

// add records id and reports false when it is already in flight.
func (s *inflightSet) add(id jsonrpc.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[id] {
		return false
	}
	if s.ids == nil {
		s.ids = map[jsonrpc.ID]bool{}
	}
	s.ids[id] = true
	return true
}

// addAll records the ids of one batch, or none of them: it reports false when any id is already in flight or
// repeats within the batch.
func (s *inflightSet) addAll(ids []jsonrpc.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[jsonrpc.ID]bool{}
	for _, id := range ids {
		if s.ids[id] || seen[id] {
			return false
		}
		seen[id] = true
	}
	if s.ids == nil && len(seen) > 0 {
		s.ids = map[jsonrpc.ID]bool{}
	}
	for id := range seen {
		s.ids[id] = true
	}
	return true
}

func (s *inflightSet) done(id jsonrpc.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

// answerWriter is the SDK's writer: a response it writes ends the request with that id.
type answerWriter struct {
	w        io.Writer
	inflight *inflightSet
}

func (a *answerWriter) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(p, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if bytes.TrimSpace(line)[0] == '[' {
			// A batch answer: the SDK writes the responses of a batch as one array, once all its requests are answered.
			var resps []json.RawMessage
			if json.Unmarshal(bytes.TrimSpace(line), &resps) == nil {
				for _, r := range resps {
					if msg, err := jsonrpc.DecodeMessage(r); err == nil {
						if resp, ok := msg.(*jsonrpc.Response); ok && resp.ID.IsValid() {
							a.inflight.done(resp.ID)
						}
					}
				}
			}
			continue
		}
		if msg, err := jsonrpc.DecodeMessage(line); err == nil {
			if resp, ok := msg.(*jsonrpc.Response); ok && resp.ID.IsValid() {
				a.inflight.done(resp.ID)
			}
		}
	}
	return a.w.Write(p)
}

func (a *answerWriter) Close() error { return nil }

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
	// inflight is shared with answerWriter; a request whose id is still in it is refused.
	inflight *inflightSet
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
		if id, ok := requestID(line); ok && !f.inflight.add(id) {
			f.replyError(codeInvalidRequest, "duplicate request id", rawID(line))
			continue
		}
		if bytes.TrimSpace(line)[0] == '[' {
			if !f.inflight.addAll(batchRequestIDs(line)) {
				// One -32600 for the whole batch: an id in it is still in flight (or repeats in it, which frameProblem refused).
				f.replyError(codeInvalidRequest, "duplicate request id", nil)
				continue
			}
			// Only a batch that is forwarded counts: the SDK answers nothing for a notification, so it is noted here.
			f.notifBatched = f.notifBatched || batchHasNotification(line)
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

// requestID returns the id of a request object (a method and a non-null id); other lines are not tracked.
func requestID(line []byte) (jsonrpc.ID, bool) {
	if len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
		return jsonrpc.ID{}, false
	}
	msg, err := jsonrpc.DecodeMessage(line)
	if err != nil {
		return jsonrpc.ID{}, false
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok || !req.ID.IsValid() {
		return jsonrpc.ID{}, false
	}
	return req.ID, true
}

// batchRequestIDs returns the ids of the request elements of a batch line (requestID's objects); the batch was
// checked by frameProblem, so it is a JSON array of valid JSON-RPC elements.
func batchRequestIDs(line []byte) []jsonrpc.ID {
	var elems []json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(line), &elems) != nil {
		return nil
	}
	var ids []jsonrpc.ID
	for _, e := range elems {
		if id, ok := requestID(e); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// rawID returns the id member of a JSON object as written, for a reply to echo.
func rawID(line []byte) json.RawMessage {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(line, &req)
	return req.ID
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
	if !utf8.Valid(line) {
		// Refused, not repaired: a string with the bytes replaced by U+FFFD would be processed silently.
		return codeParseError, "parse error: invalid UTF-8", nil
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
		switch {
		case req.ID.IsValid() && seen[req.ID]:
			return codeInvalidRequest, "duplicate request id"
		case !req.ID.IsValid() && (seen[req.ID] || f.notifBatched):
			// A notification has no id, so the SDK keys it by the zero id, and two of them clash in one batch.
			return codeInvalidRequest, "Invalid Request: a batch repeats a request id"
		}
		seen[req.ID] = true
	}
	return 0, ""
}

// batchHasNotification reports whether a batch line holds a notification (a request without an id, which the SDK
// keys by the zero id). Read calls it only for a batch it forwards, so a refused batch never sets notifBatched.
func batchHasNotification(line []byte) bool {
	var elems []json.RawMessage
	_ = json.Unmarshal(line, &elems)
	for _, e := range elems {
		if msg, err := jsonrpc.DecodeMessage(e); err == nil {
			if req, ok := msg.(*jsonrpc.Request); ok && !req.ID.IsValid() {
				return true
			}
		}
	}
	return false
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
