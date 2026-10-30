package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// PushDiagnosticsTimeout bounds the wait for a push-only publishDiagnostics.
const PushDiagnosticsTimeout = 5 * time.Second

const stderrTailLines = 10

var retryDelays = []time.Duration{100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}

// RequestError is a JSON-RPC error response.
type RequestError struct {
	Method  string
	Code    int
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// ExitedError means the server process ended while a request was pending.
type ExitedError struct{ Msg string }

func (e *ExitedError) Error() string { return e.Msg }

// InvalidPositionError: the line/column does not exist in the file.
type InvalidPositionError struct{ Msg string }

func (e *InvalidPositionError) Error() string { return e.Msg }

type response struct {
	Result json.RawMessage
	Err    *RequestError
}

type openFile struct {
	version int
	mtimeNs int64
	size    int64
}

// overlayMtime never equals a real mtime, so the next EnsureOpen after an
// overlay ends re-reads the file from disk.
const overlayMtime = -2

// Options configure a Client.
type Options struct {
	Root     string
	Command  []string
	Language string
	// InitOptions is sent as initializationOptions (gopls settings).
	InitOptions map[string]any
	OnNotice    func(string)
	// WatchSuffix (e.g. ".go") and SkipDir enable the disk scan in Refresh: files with the suffix
	// that appeared, changed or vanished since the last call are reported to the server.
	WatchSuffix string
	SkipDir     func(name string) bool
}

// Client talks LSP to one server process.
type Client struct {
	opts Options

	mu       sync.Mutex // guards everything below except writeMu
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	nextID   int
	pending  map[int]chan response
	diags    map[string][]Diagnostic
	diagEv   map[string]chan struct{}
	open     map[string]*openFile
	overlays map[string]string
	started  bool
	stderr   []string
	exited   chan struct{}
	snapshot map[string][2]int64 // watched files on disk at the last Refresh (path -> mtime, size)

	writeMu sync.Mutex
	// opMu serialises overlay() against refresh()/EnsureOpen from other tool calls.
	opMu sync.Mutex
}

func New(opts Options) *Client { return &Client{opts: opts} }

func (c *Client) Root() string { return c.opts.Root }

func (c *Client) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return false
	}
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// PathToURI converts an absolute path to a file URI.
func PathToURI(p string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String()
}

// URIToPath converts a file URI to a path ("" when not a file URI).
func URIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(u.Path)
}

func (c *Client) reset() {
	c.pending = map[int]chan response{}
	c.diags = map[string][]Diagnostic{}
	c.diagEv = map[string]chan struct{}{}
	c.open = map[string]*openFile{}
	c.overlays = map[string]string{}
	c.stderr = nil
	c.nextID = 0
	c.exited = make(chan struct{})
}

// Start launches the server and performs the initialize handshake.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.reset()
	cmd := exec.Command(c.opts.Command[0], c.opts.Command[1:]...)
	cmd.Dir = c.opts.Root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := cmd.Start(); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("starting %s: %w", strings.Join(c.opts.Command, " "), err)
	}
	c.cmd, c.stdin = cmd, stdin
	exited := c.exited
	c.started = true
	c.mu.Unlock()

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			c.mu.Lock()
			c.stderr = append(c.stderr, sc.Text())
			if len(c.stderr) > stderrTailLines {
				c.stderr = c.stderr[len(c.stderr)-stderrTailLines:]
			}
			c.mu.Unlock()
		}
	}()
	go func() {
		c.readLoop(bufio.NewReader(stdout))
		select {
		case <-stderrDone:
		case <-time.After(500 * time.Millisecond):
		}
		close(exited)
		c.failPending()
		_ = cmd.Wait()
	}()

	root := PathToURI(c.opts.Root)
	params := map[string]any{
		"processId": nil,
		"rootUri":   root,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{},
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"callHierarchy":      map[string]any{},
				"typeHierarchy":      map[string]any{},
				"rename":             map[string]any{"prepareSupport": true},
				// dataSupport + resolveSupport make gopls send lazy fixes as resolvable actions
				// (edit filled in by codeAction/resolve) instead of command-only ones.
				"codeAction": map[string]any{
					"codeActionLiteralSupport": map[string]any{
						"codeActionKind": map[string]any{"valueSet": []string{"", "quickfix", "refactor", "source"}}},
					"dataSupport":    true,
					"resolveSupport": map[string]any{"properties": []string{"edit"}}},
			},
			"workspace": map[string]any{"workspaceFolders": true,
				"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true},
				"configuration":         true},
		},
		"workspaceFolders": []map[string]any{{"uri": root, "name": filepath.Base(c.opts.Root)}},
	}
	if len(c.opts.InitOptions) > 0 {
		params["initializationOptions"] = c.opts.InitOptions
	}
	if _, err := c.Request(ctx, "initialize", params); err != nil {
		_ = c.Stop()
		return err
	}
	return c.Notify("initialized", map[string]any{})
}

// Stop shuts the server down.
func (c *Client) Stop() error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil
	}
	cmd, stdin, exited := c.cmd, c.stdin, c.exited
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := c.requestOnce(ctx, "shutdown", nil); err == nil {
		_ = c.Notify("exit", nil)
	}
	cancel()
	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	c.mu.Lock()
	c.started = false
	c.mu.Unlock()
	return nil
}

// Restart stops the server and starts it afresh with no memory of the old session.
func (c *Client) Restart(ctx context.Context) error {
	_ = c.Stop()
	return c.Start(ctx)
}

func (c *Client) readLoop(r *bufio.Reader) {
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
				length, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if length < 0 {
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.ID != nil && msg.Method != "":
			c.replyToServer(msg.ID, msg.Method, msg.Params)
		case msg.ID != nil:
			id, err := strconv.Atoi(string(msg.ID))
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				resp := response{Result: msg.Result}
				if msg.Error != nil {
					resp.Err = &RequestError{Code: msg.Error.Code, Message: msg.Error.Message}
				}
				ch <- resp
			}
		case msg.Method == "textDocument/publishDiagnostics":
			var p struct {
				URI         string       `json:"uri"`
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && p.URI != "" {
				c.mu.Lock()
				c.diags[p.URI] = p.Diagnostics
				if ev := c.diagEv[p.URI]; ev != nil {
					close(ev)
					delete(c.diagEv, p.URI)
				}
				c.mu.Unlock()
			}
		}
	}
}

func (c *Client) replyToServer(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	var rpcErr any
	switch method {
	case "workspace/configuration":
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(params, &p)
		result = make([]any, len(p.Items))
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create",
		"workspace/diagnostic/refresh", "workspace/semanticTokens/refresh", "workspace/inlayHint/refresh",
		"workspace/codeLens/refresh":
		result = nil
	default:
		rpcErr = map[string]any{"code": -32601, "message": "Method not found: " + method}
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	_ = c.send(msg)
}

func (c *Client) failPending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int]chan response{}
	msg := "language server exited: " + strings.Join(c.opts.Command, " ")
	if len(c.stderr) > 0 {
		msg += "\nIts last stderr output:\n" + strings.Join(c.stderr, "\n")
	}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- response{Err: &RequestError{Code: -1, Message: msg}}
	}
}

func (c *Client) send(obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("language server not started")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = stdin.Write(append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...))
	return err
}

// Request sends a request, retrying on ContentModified / ServerCancelled.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for _, d := range retryDelays {
		res, err := c.requestOnce(ctx, method, params)
		var re *RequestError
		if errors.As(err, &re) && (re.Code == -32801 || re.Code == -32802) {
			select {
			case <-time.After(d):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return res, err
	}
	return c.requestOnce(ctx, method, params)
}

func (c *Client) requestOnce(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	if c.pending == nil {
		c.pending = map[int]chan response{}
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Err != nil {
			resp.Err.Method = method
			return nil, resp.Err
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s timed out: %w", method, ctx.Err())
	}
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// -- document sync ---------------------------------------------------------

func readDocumentText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))), nil
}

func (c *Client) absPath(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.opts.Root, p)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// EnsureOpen syncs a file from disk (didOpen, or didChange when it changed) and returns its URI.
func (c *Client) EnsureOpen(path string) (string, error) {
	abs := c.absPath(path)
	uri := PathToURI(abs)
	c.mu.Lock()
	if _, ok := c.overlays[uri]; ok {
		c.mu.Unlock()
		return uri, nil
	}
	known := c.open[uri]
	c.mu.Unlock()
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if known != nil && known.mtimeNs == st.ModTime().UnixNano() && known.size == st.Size() {
		return uri, nil
	}
	text, err := readDocumentText(abs)
	if err != nil {
		return "", err
	}
	return uri, c.syncText(uri, text, st.ModTime().UnixNano(), st.Size())
}

func (c *Client) syncText(uri, text string, mtime, size int64) error {
	c.mu.Lock()
	known := c.open[uri]
	if known == nil {
		known = &openFile{}
		c.open[uri] = known
		known.version = 1
	} else {
		known.version++
	}
	known.mtimeNs, known.size = mtime, size
	version, isNew := known.version, known.version == 1
	// A fresh sync invalidates the last pushed diagnostics: wait for the next ones.
	delete(c.diags, uri)
	if _, ok := c.diagEv[uri]; !ok {
		c.diagEv[uri] = make(chan struct{})
	}
	c.mu.Unlock()
	if isNew {
		return c.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": uri, "languageId": c.opts.Language, "version": version, "text": text}})
	}
	return c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": text}}})
}

// CloseDocument tells the server a document is gone and drops its caches.
func (c *Client) CloseDocument(uri string) error {
	c.mu.Lock()
	_, ok := c.open[uri]
	delete(c.open, uri)
	delete(c.diags, uri)
	delete(c.overlays, uri)
	c.mu.Unlock()
	if !ok {
		return nil
	}
	return c.Notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

// Overlay shows the server `texts` (path -> content) in place of the disk's while fn runs.
// Documents are restored to disk state afterwards. Files that do not exist on disk work too.
func (c *Client) Overlay(ctx context.Context, texts map[string]string, fn func() error) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	uris := make([]string, 0, len(texts))
	defer func() {
		for _, uri := range uris {
			c.mu.Lock()
			delete(c.overlays, uri)
			c.mu.Unlock()
			path := URIToPath(uri)
			if st, err := os.Stat(path); err == nil {
				if text, err := readDocumentText(path); err == nil {
					_ = c.syncText(uri, text, st.ModTime().UnixNano(), st.Size())
				}
			} else {
				_ = c.CloseDocument(uri)
			}
		}
	}()
	for p, text := range texts {
		uri := PathToURI(c.absPath(p))
		uris = append(uris, uri)
		c.mu.Lock()
		c.overlays[uri] = text
		c.mu.Unlock()
		if err := c.syncText(uri, text, overlayMtime, int64(len(text))); err != nil {
			return err
		}
	}
	return fn()
}

// NoteWritten records files this process just created, changed or deleted, so the next Refresh
// does not mistake them for outside changes and a later outside deletion is still noticed. The
// server is told once, through didChangeWatchedFiles; open documents are resynced or closed.
func (c *Client) NoteWritten(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	var changes []map[string]any
	c.mu.Lock()
	var before map[string][2]int64
	if c.snapshot != nil {
		before = make(map[string][2]int64, len(c.snapshot))
		for k, v := range c.snapshot {
			before[k] = v
		}
	}
	c.mu.Unlock()
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.opts.Root, p)
		}
		p = filepath.Clean(p)
		uri := PathToURI(c.docPath(p))
		st, err := os.Stat(p)
		c.mu.Lock()
		if c.snapshot == nil {
			c.snapshot = map[string][2]int64{}
		}
		_, was := c.snapshot[p]
		typ := 3
		if err == nil {
			c.snapshot[p] = [2]int64{st.ModTime().UnixNano(), st.Size()}
			typ = 1
			if was {
				typ = 2
			}
		} else {
			delete(c.snapshot, p)
		}
		_, isOpen := c.open[uri]
		c.mu.Unlock()
		if isOpen {
			if err != nil {
				_ = c.CloseDocument(uri)
			} else if text, rerr := readDocumentText(p); rerr == nil {
				_ = c.syncText(uri, text, st.ModTime().UnixNano(), st.Size())
			}
		}
		changes = append(changes, map[string]any{"uri": uri, "type": typ})
	}
	if err := c.Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": changes}); err != nil {
		return err
	}
	c.mu.Lock()
	now := make(map[string][2]int64, len(c.snapshot))
	for k, v := range c.snapshot {
		now[k] = v
	}
	c.mu.Unlock()
	c.loadNewDirs(newDirFiles(before, now))
	return nil
}

// docPath is absPath for a path that may not exist yet: its directory is resolved instead.
func (c *Client) docPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return filepath.Clean(p)
}

// -- position helpers --------------------------------------------------------

// Utf16Len is the length of s in UTF-16 code units.
func Utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

func (c *Client) checkPosition(abs string, line, column int) error {
	c.mu.Lock()
	text, ok := c.overlays[PathToURI(abs)]
	c.mu.Unlock()
	if !ok {
		var err error
		if text, err = readDocumentText(abs); err != nil {
			return err
		}
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if line < 1 || line > len(lines) {
		return &InvalidPositionError{fmt.Sprintf("line %d is outside the file (it has %d lines)", line, len(lines))}
	}
	if width := Utf16Len(lines[line-1]); column < 1 || column > width+1 {
		return &InvalidPositionError{fmt.Sprintf("column %d is outside line %d (it has %d UTF-16 columns)", column, line, width)}
	}
	return nil
}

func (c *Client) posParams(path string, line, column int) (map[string]any, error) {
	uri, err := c.EnsureOpen(path)
	if err != nil {
		return nil, err
	}
	if err := c.checkPosition(c.absPath(path), line, column); err != nil {
		return nil, err
	}
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     Position{Line: line - 1, Character: column - 1},
	}, nil
}

// -- typed requests ------------------------------------------------------------

func decode[T any](raw json.RawMessage, err error) (T, error) {
	var out T
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

// Hover returns the raw `contents` value of a hover result.
func (c *Client) Hover(ctx context.Context, path string, line, column int) (json.RawMessage, error) {
	p, err := c.posParams(path, line, column)
	if err != nil {
		return nil, err
	}
	res, err := decode[struct {
		Contents json.RawMessage `json:"contents"`
	}](c.Request(ctx, "textDocument/hover", p))
	return res.Contents, err
}

// locations decodes Location | Location[] | LocationLink[].
func locations(raw json.RawMessage, err error) ([]Location, error) {
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, err
	}
	var many []struct {
		Location
		TargetURI            string `json:"targetUri"`
		TargetSelectionRange Range  `json:"targetSelectionRange"`
	}
	if json.Unmarshal(raw, &many) != nil {
		var one Location
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		return []Location{one}, nil
	}
	out := make([]Location, 0, len(many))
	for _, m := range many {
		if m.TargetURI != "" {
			out = append(out, Location{URI: m.TargetURI, Range: m.TargetSelectionRange})
		} else {
			out = append(out, m.Location)
		}
	}
	return out, nil
}

func (c *Client) locationRequest(ctx context.Context, method, path string, line, column int, extra map[string]any) ([]Location, error) {
	p, err := c.posParams(path, line, column)
	if err != nil {
		return nil, err
	}
	for k, v := range extra {
		p[k] = v
	}
	return locations(c.Request(ctx, method, p))
}

func (c *Client) Definition(ctx context.Context, path string, line, column int) ([]Location, error) {
	return c.locationRequest(ctx, "textDocument/definition", path, line, column, nil)
}

func (c *Client) TypeDefinition(ctx context.Context, path string, line, column int) ([]Location, error) {
	return c.locationRequest(ctx, "textDocument/typeDefinition", path, line, column, nil)
}

func (c *Client) Implementation(ctx context.Context, path string, line, column int) ([]Location, error) {
	return c.locationRequest(ctx, "textDocument/implementation", path, line, column, nil)
}

func (c *Client) References(ctx context.Context, path string, line, column int, includeDeclaration bool) ([]Location, error) {
	return c.locationRequest(ctx, "textDocument/references", path, line, column,
		map[string]any{"context": map[string]any{"includeDeclaration": includeDeclaration}})
}

func (c *Client) WorkspaceSymbol(ctx context.Context, query string) ([]Symbol, error) {
	return decode[[]Symbol](c.Request(ctx, "workspace/symbol", map[string]any{"query": query}))
}

func (c *Client) DocumentSymbol(ctx context.Context, path string) ([]Symbol, error) {
	uri, err := c.EnsureOpen(path)
	if err != nil {
		return nil, err
	}
	return decode[[]Symbol](c.Request(ctx, "textDocument/documentSymbol",
		map[string]any{"textDocument": map[string]any{"uri": uri}}))
}

func (c *Client) PrepareCallHierarchy(ctx context.Context, path string, line, column int) ([]CallHierarchyItem, error) {
	p, err := c.posParams(path, line, column)
	if err != nil {
		return nil, err
	}
	return decode[[]CallHierarchyItem](c.Request(ctx, "textDocument/prepareCallHierarchy", p))
}

func (c *Client) IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]IncomingCall, error) {
	return decode[[]IncomingCall](c.Request(ctx, "callHierarchy/incomingCalls", map[string]any{"item": item}))
}

func (c *Client) PrepareRename(ctx context.Context, path string, line, column int) (*Range, error) {
	p, err := c.posParams(path, line, column)
	if err != nil {
		return nil, err
	}
	res, err := decode[json.RawMessage](c.Request(ctx, "textDocument/prepareRename", p))
	if err != nil || len(res) == 0 {
		return nil, err
	}
	var r struct {
		Range *Range `json:"range"`
		Start *Position
		End   *Position
	}
	if json.Unmarshal(res, &r) != nil {
		return nil, nil
	}
	if r.Range != nil {
		return r.Range, nil
	}
	var direct Range
	if json.Unmarshal(res, &direct) == nil {
		return &direct, nil
	}
	return nil, nil
}

func (c *Client) Rename(ctx context.Context, path string, line, column int, newName string) (*WorkspaceEdit, error) {
	p, err := c.posParams(path, line, column)
	if err != nil {
		return nil, err
	}
	p["newName"] = newName
	return decode[*WorkspaceEdit](c.Request(ctx, "textDocument/rename", p))
}

// Diagnostics returns current diagnostics of a file: pull when the server supports it
// (gopls does, though it does not advertise it), else the last push.
func (c *Client) Diagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	uri, err := c.EnsureOpen(path)
	if err != nil {
		return nil, err
	}
	return c.PullDiagnostics(ctx, uri)
}

// PullDiagnostics pulls diagnostics for an already-synced uri, falling back to the
// push cache (waiting up to PushDiagnosticsTimeout for a fresh one).
func (c *Client) PullDiagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
	res, err := decode[struct {
		Items []Diagnostic `json:"items"`
	}](c.Request(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": uri}}))
	if err == nil {
		return res.Items, nil
	}
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -32601 {
		return nil, err
	}
	c.mu.Lock()
	cached, have := c.diags[uri]
	ev := c.diagEv[uri]
	c.mu.Unlock()
	if !have && ev != nil {
		select {
		case <-ev:
		case <-time.After(PushDiagnosticsTimeout):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		cached = c.diags[uri]
		c.mu.Unlock()
	}
	return cached, nil
}

// DiagnosticsOf pulls diagnostics for each uri (files need not be open).
func (c *Client) DiagnosticsOf(ctx context.Context, uris []string) (map[string][]Diagnostic, error) {
	out := map[string][]Diagnostic{}
	for _, uri := range uris {
		d, err := c.PullDiagnostics(ctx, uri)
		if err != nil {
			return nil, err
		}
		out[uri] = d
	}
	return out, nil
}

func (c *Client) CodeActions(ctx context.Context, path string, rng Range, only []string, diags []Diagnostic) ([]json.RawMessage, error) {
	uri, err := c.EnsureOpen(path)
	if err != nil {
		return nil, err
	}
	ctxt := map[string]any{"diagnostics": diags}
	if diags == nil {
		ctxt["diagnostics"] = []Diagnostic{}
	}
	if len(only) > 0 {
		ctxt["only"] = only
	}
	return decode[[]json.RawMessage](c.Request(ctx, "textDocument/codeAction",
		map[string]any{"textDocument": map[string]any{"uri": uri}, "range": rng, "context": ctxt}))
}

// scanDisk lists the watched files below the root, capped so a huge tree cannot stall a call.
func (c *Client) scanDisk() map[string][2]int64 {
	out := map[string][2]int64{}
	if c.opts.WatchSuffix == "" {
		return out
	}
	_ = filepath.WalkDir(c.opts.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != c.opts.Root && c.opts.SkipDir != nil && c.opts.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, c.opts.WatchSuffix) {
			if info, ierr := d.Info(); ierr == nil {
				out[p] = [2]int64{info.ModTime().UnixNano(), info.Size()}
			}
		}
		if len(out) >= 50000 {
			return filepath.SkipAll
		}
		return nil
	})
	return out
}

// Refresh brings the server's view up to date with the disk: open files that changed are re-read,
// and files created, changed or deleted behind its back are reported via didChangeWatchedFiles.
func (c *Client) Refresh() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	now := c.scanDisk()
	c.mu.Lock()
	prev := c.snapshot
	c.snapshot = now
	var stale []string
	var changes []map[string]any
	seen := map[string]bool{}
	for uri, f := range c.open {
		if _, ok := c.overlays[uri]; ok {
			continue
		}
		seen[URIToPath(uri)] = true
		st, err := os.Stat(URIToPath(uri))
		if err != nil {
			stale = append(stale, uri)
			changes = append(changes, map[string]any{"uri": uri, "type": 3})
			continue
		}
		if f.mtimeNs != st.ModTime().UnixNano() || f.size != st.Size() {
			changes = append(changes, map[string]any{"uri": uri, "type": 2})
		}
	}
	c.mu.Unlock()
	if prev != nil {
		for p, v := range now {
			switch old, ok := prev[p]; {
			case !ok:
				changes = append(changes, map[string]any{"uri": PathToURI(p), "type": 1})
			case old != v && !seen[p]:
				changes = append(changes, map[string]any{"uri": PathToURI(p), "type": 2})
			}
		}
		for p := range prev {
			if _, ok := now[p]; !ok && !seen[p] {
				changes = append(changes, map[string]any{"uri": PathToURI(p), "type": 3})
			}
		}
	}
	for _, uri := range stale {
		_ = c.CloseDocument(uri)
	}
	if len(changes) == 0 {
		return nil
	}
	newDirs := newDirFiles(prev, now)
	for _, ch := range changes {
		if ch["type"] == 2 {
			uri := ch["uri"].(string)
			if f := c.openFileFor(uri); f {
				p := URIToPath(uri)
				if st, err := os.Stat(p); err == nil {
					if text, err := readDocumentText(p); err == nil {
						_ = c.syncText(uri, text, st.ModTime().UnixNano(), st.Size())
					}
				}
			}
		}
	}
	if err := c.Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": changes}); err != nil {
		return err
	}
	c.loadNewDirs(newDirs)
	return nil
}

// loadNewDirs makes gopls load the packages of files created in directories it did not know.
// gopls only reloads the packages of known directories on didChangeWatchedFiles, so such a file
// stays invisible to workspace/symbol (even seconds later). Opening it and pulling its diagnostics
// loads the package synchronously; the document stays open and Refresh keeps it in sync.
func (c *Client) loadNewDirs(files []string) {
	for _, p := range files {
		if uri, err := c.EnsureOpen(p); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = c.Request(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": uri}})
			cancel()
		}
	}
}

// maxNewDirOpens bounds how many new directories Refresh opens a file in (a big checkout should
// not turn into thousands of didOpens).
const maxNewDirOpens = 50

// newDirFiles picks one file in each directory that has watched files now but had none at the last
// scan. prev == nil (the first scan) yields nothing: gopls loads the initial tree itself.
func newDirFiles(prev, now map[string][2]int64) []string {
	if prev == nil {
		return nil
	}
	known := map[string]bool{}
	for p := range prev {
		known[filepath.Dir(p)] = true
	}
	pick := map[string]string{}
	for p := range now {
		d := filepath.Dir(p)
		if known[d] {
			continue
		}
		if cur, ok := pick[d]; !ok || p < cur {
			pick[d] = p
		}
	}
	out := make([]string, 0, len(pick))
	for _, p := range pick {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > maxNewDirOpens {
		out = out[:maxNewDirOpens]
	}
	return out
}

func (c *Client) openFileFor(uri string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.open[uri]
	return ok
}
