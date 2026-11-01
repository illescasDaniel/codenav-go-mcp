package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxFrame bounds one inbound line, like the SDK's own default.
const maxFrame = mcp.DefaultMaxLineLength

// NewStdioTransport is the stdio transport with a guard in front of the SDK's: a line that is not JSON
// (a truncated message, a bare Infinity or NaN) would make the SDK's decoder fail and the server exit.
// Here such a line is dropped and answered with a JSON-RPC -32700 parse error (id null), a line that is
// JSON but no object or array with -32600, and the server keeps serving the lines that follow.
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
		if code, msg := frameProblem(line); code != 0 {
			f.replyError(code, msg)
			continue
		}
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

// frameProblem classifies a line the SDK could not handle: 0 when it is a JSON object or array.
func frameProblem(line []byte) (int, string) {
	if bytes.Equal(line, []byte("\x00too long")) {
		return codeParseError, "Parse error: message is too large"
	}
	if !json.Valid(line) {
		return codeParseError, "Parse error: the message is not valid JSON"
	}
	if c := bytes.TrimSpace(line)[0]; c != '{' && c != '[' {
		return codeInvalidRequest, "Invalid Request: a JSON-RPC message is a JSON object"
	}
	return 0, ""
}

func (f *frameReader) replyError(code int, msg string) {
	b, _ := json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      *int           `json:"id"`
		Error   map[string]any `json:"error"`
	}{"2.0", nil, map[string]any{"code": code, "message": msg}})
	_, _ = f.reply.Write(append(b, '\n'))
}
