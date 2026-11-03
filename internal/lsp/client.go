package lsp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
)

// PushDiagnosticsTimeout bounds the wait for a push-only publishDiagnostics.
const PushDiagnosticsTimeout = 5 * time.Second

const stderrTailLines = 10

// stderrLineKeep is how much of a stderr line is kept; the rest of an over-long line is read and dropped.
const stderrLineKeep = 1024

// Stop waits this long for the shutdown reply, then for the exit, before the server is killed; a write
// to its stdin that does not complete within stdinWriteTimeout means it stopped reading (wedged).
const (
	stopRequestTimeout = 2 * time.Second
	stopExitWait       = 2 * time.Second
	stdinWriteTimeout  = 3 * time.Second
)

// maxScanFiles caps the watched files a disk scan lists, so a huge tree cannot stall a call.
const maxScanFiles = 50000

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
	sum     string // content hash of the text last sent to the server
}

// fileStamp is what a disk scan remembers about a file: its mtime and size, and for the module files a
// content hash, since a rewrite can keep both the size and the mtime.
type fileStamp struct {
	mtimeNs int64
	size    int64
	sum     string
}

// Options configure a Client.
type Options struct {
	Root     string
	Command  []string
	Language string
	// InitOptions is sent as initializationOptions (gopls settings).
	InitOptions map[string]any
	// OnNotice receives a line for each server restart the client makes on its own.
	OnNotice func(string)
	// WatchSuffix (e.g. ".go") and SkipDir enable the disk scan in Refresh: files with the suffix
	// that appeared, changed or vanished since the last call are reported to the server.
	WatchSuffix string
	SkipDir     func(name string) bool
	// ConfigNames are the base names of the module files (go.mod, go.sum, go.work, go.work.sum) below
	// Root whose content is hashed by the scan. ExtraConfigFiles lists more of them (absolute paths,
	// e.g. the go.mod above Root), read on each scan.
	ConfigNames      []string
	ExtraConfigFiles func() []string
	// ExtraFolders are directories below Root sent to the server as further workspace folders at
	// start (modules the root's go.mod/go.work does not cover, which gopls would not index).
	ExtraFolders []string
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
	stopping bool // Stop was asked for: its exit is expected and not logged
	stderr   []string
	exited   chan struct{}
	snapshot map[string]fileStamp // watched files on disk at the last Refresh
	configs  map[string]fileStamp // module files on disk at the last Refresh
	extra    map[string]bool      // workspace folders besides the root that the server knows

	writeMu sync.Mutex
	// opMu serialises overlay() against refresh()/EnsureOpen from other tool calls.
	opMu sync.Mutex
}

func New(opts Options) *Client {
	c := &Client{opts: opts, extra: map[string]bool{}}
	for _, f := range opts.ExtraFolders {
		c.extra[filepath.Clean(f)] = true
	}
	return c
}

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
	c.stopping = false
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
		readStderr(stderr, func(line string) {
			c.mu.Lock()
			c.stderr = append(c.stderr, line)
			if len(c.stderr) > stderrTailLines {
				c.stderr = c.stderr[len(c.stderr)-stderrTailLines:]
			}
			c.mu.Unlock()
		})
	}()
	go func() {
		c.readLoop(bufio.NewReader(stdout))
		select {
		case <-stderrDone:
		case <-time.After(500 * time.Millisecond):
		}
		c.failPending() // before exited closes: a Stop that returns has seen everything it logs
		close(exited)
		_ = cmd.Wait()
	}()

	root := PathToURI(c.opts.Root)
	params := map[string]any{
		"processId": nil,
		"rootUri":   root,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{"relatedInformation": true},
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
		"workspaceFolders": c.folders(),
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

// folders lists the workspace folders sent at start: the root, then the extra ones.
func (c *Client) folders() []map[string]any {
	out := []map[string]any{{"uri": PathToURI(c.opts.Root), "name": filepath.Base(c.opts.Root)}}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range sortedKeys(c.extra) {
		out = append(out, map[string]any{"uri": PathToURI(f), "name": filepath.Base(f)})
	}
	return out
}

// readStderr reads the server's stderr to EOF and passes each line to emit. A line over stderrLineKeep
// bytes is cut to its prefix and marked " ...". The stream is always read to the end: a server whose
// stderr nobody drains blocks on its next write and answers nothing.
func readStderr(r io.Reader, emit func(string)) {
	br := bufio.NewReader(r)
	var line []byte
	cut := false
	for {
		part, isPrefix, err := br.ReadLine()
		if err != nil {
			if len(line) > 0 || cut {
				emit(string(line) + cutMark(cut))
			}
			return
		}
		if room := stderrLineKeep - len(line); room > 0 {
			n := min(room, len(part))
			line = append(line, part[:n]...)
			cut = cut || n < len(part)
		} else if len(part) > 0 {
			cut = true
		}
		if isPrefix {
			continue
		}
		emit(string(line) + cutMark(cut))
		line, cut = line[:0], false
	}
}

func cutMark(cut bool) string {
	if cut {
		return " ..."
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SyncFolders makes the extra workspace folders (besides the root) exactly dirs, telling the server
// with workspace/didChangeWorkspaceFolders. gopls builds a view for each folder, so the packages of a
// module the root does not cover show up in workspace/symbol without a file of it being opened first.
func (c *Client) SyncFolders(dirs []string) error {
	want := map[string]bool{}
	for _, d := range dirs {
		want[filepath.Clean(d)] = true
	}
	c.mu.Lock()
	var added, removed []map[string]any
	for _, d := range sortedKeys(want) {
		if !c.extra[d] {
			added = append(added, map[string]any{"uri": PathToURI(d), "name": filepath.Base(d)})
		}
	}
	for _, d := range sortedKeys(c.extra) {
		if !want[d] {
			removed = append(removed, map[string]any{"uri": PathToURI(d), "name": filepath.Base(d)})
		}
	}
	started := c.started
	c.extra = want
	c.mu.Unlock()
	if !started || len(added)+len(removed) == 0 {
		return nil
	}
	if added == nil {
		added = []map[string]any{}
	}
	if removed == nil {
		removed = []map[string]any{}
	}
	return c.Notify("workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"added": added, "removed": removed}})
}

// Stop shuts the server down.
func (c *Client) Stop() error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil
	}
	cmd, stdin, exited := c.cmd, c.stdin, c.exited
	c.stopping = true
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), stopRequestTimeout)
	if _, err := c.requestOnce(ctx, "shutdown", nil); err == nil {
		_ = c.Notify("exit", nil)
	}
	cancel()
	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(stopExitWait):
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
	tail := append([]string(nil), c.stderr...)
	unexpected := !c.stopping
	c.mu.Unlock()
	// The stderr tail goes to the log (it may name the binary; agents never see it). A stop that Stop asked
	// for is expected and stays quiet.
	if unexpected {
		msg := "language server exited unexpectedly"
		if len(tail) > 0 {
			msg += "; its last stderr output:\n" + strings.Join(tail, "\n")
		}
		log.Print(msg)
	}
	// The message names no binary path: the agent sees it, and the next call starts a new server (Acquire).
	const msg = "the language server stopped and is restarting; retry"
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
	if d, ok := stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(stdinWriteTimeout))
	}
	_, err = stdin.Write(append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...))
	if errors.Is(err, os.ErrDeadlineExceeded) {
		c.kill() // the server stopped reading its input: end it, so the next call starts a new one
	}
	return err
}

// kill ends the server process (its exit is then seen by Alive and the readLoop).
func (c *Client) kill() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
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
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("%s: %w", method, ctx.Err())
		}
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
	var sum string
	if known != nil {
		sum = known.sum
	}
	c.mu.Unlock()
	// The content decides, not the mtime and size: a same-length rewrite that keeps the mtime is a change.
	text, err := readDocumentText(abs)
	if err != nil {
		return "", err
	}
	if known != nil && contentSum(text) == sum {
		return uri, nil
	}
	return uri, c.syncText(uri, text)
}

// contentSum is the content hash of a document or module file.
func contentSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (c *Client) syncText(uri, text string) error {
	c.mu.Lock()
	known := c.open[uri]
	if known == nil {
		known = &openFile{}
		c.open[uri] = known
		known.version = 1
	} else {
		known.version++
	}
	known.sum = contentSum(text)
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
			text, rerr := readDocumentText(path)
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && rerr == nil {
				_ = c.syncText(uri, text)
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
		if err := c.syncText(uri, text); err != nil {
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
	before := make(map[string]fileStamp, len(c.snapshot))
	for k, v := range c.snapshot {
		before[k] = v
	}
	c.mu.Unlock()
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.opts.Root, p)
		}
		p = filepath.Clean(p)
		uri := PathToURI(c.docPath(p))
		st, err := os.Stat(p)
		regular := err == nil && st.Mode().IsRegular()
		isConfig := c.isConfigName(filepath.Base(p))
		c.mu.Lock()
		typ := c.record(p, st, regular, isConfig)
		_, isOpen := c.open[uri]
		c.mu.Unlock()
		if isOpen {
			if !regular {
				_ = c.CloseDocument(uri)
			} else if text, rerr := readDocumentText(p); rerr == nil {
				_ = c.syncText(uri, text)
			}
		}
		changes = append(changes, map[string]any{"uri": uri, "type": typ})
	}
	if err := c.Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": changes}); err != nil {
		return err
	}
	c.mu.Lock()
	now := make(map[string]fileStamp, len(c.snapshot))
	for k, v := range c.snapshot {
		now[k] = v
	}
	c.mu.Unlock()
	c.loadNewDirs(newDirFiles(before, now))
	return nil
}

// record stamps p in the scan that holds it (module files in configs, the rest in snapshot) and returns the
// didChangeWatchedFiles type for it. A path that is not a regular file (missing, or a directory now) counts as
// deleted. The caller holds c.mu.
func (c *Client) record(p string, st os.FileInfo, regular, isConfig bool) int {
	table := &c.snapshot
	if isConfig {
		table = &c.configs
	}
	if *table == nil {
		*table = map[string]fileStamp{}
	}
	_, was := (*table)[p]
	if !regular {
		delete(*table, p)
		return 3
	}
	(*table)[p] = stampOf(p, st, isConfig)
	if was {
		return 2
	}
	return 1
}

// isConfigName reports whether a base name is one of the module files whose content is hashed.
func (c *Client) isConfigName(name string) bool {
	for _, n := range c.opts.ConfigNames {
		if n == name {
			return true
		}
	}
	return false
}

// stampOf stamps the file at p from its info. Module files also get a content hash.
func stampOf(p string, info os.FileInfo, hashed bool) fileStamp {
	s := fileStamp{mtimeNs: info.ModTime().UnixNano(), size: info.Size()}
	if hashed {
		if b, err := os.ReadFile(p); err == nil {
			s.sum = contentSum(string(b))
		}
	}
	return s
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

// lineCount words a line count for an out-of-range message: "1 line", "27 lines".
func lineCount(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

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
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1] // the empty element after a final newline is not a line
	}
	if line < 1 || line > len(lines) {
		return &InvalidPositionError{fmt.Sprintf("line %d is outside the file (it has %s)", line, lineCount(len(lines)))}
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
	locs, err := locations(c.Request(ctx, method, p))
	if method == "textDocument/definition" || method == "textDocument/typeDefinition" {
		return locs, err // callers explain a declaration in cgo glue
	}
	return dropGenerated(locs, func(l Location) string { return l.URI }), err
}

// dropGenerated removes the entries in cgo's generated glue (go build cache), which no results should list.
func dropGenerated[T any](xs []T, uri func(T) string) []T {
	out := xs[:0:0]
	for _, x := range xs {
		if p := URIToPath(uri(x)); p != "" && exclude.IsCgoGenerated(p) {
			continue
		}
		out = append(out, x)
	}
	return out
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

// WorkspaceSymbolCap is gopls's limit on a workspace/symbol answer: a list this long may be cut short.
const WorkspaceSymbolCap = 100

func (c *Client) WorkspaceSymbol(ctx context.Context, query string) ([]Symbol, error) {
	syms, _, err := c.WorkspaceSymbolCapped(ctx, query)
	return syms, err
}

// WorkspaceSymbolCapped is WorkspaceSymbol and whether gopls answered with its full cap. The cap is judged
// on gopls's raw list, before the generated cgo files are dropped from it.
func (c *Client) WorkspaceSymbolCapped(ctx context.Context, query string) ([]Symbol, bool, error) {
	raw, err := decode[[]Symbol](c.Request(ctx, "workspace/symbol", map[string]any{"query": query}))
	capped := len(raw) >= WorkspaceSymbolCap
	return dropGenerated(raw, func(s Symbol) string {
		if s.Location == nil {
			return ""
		}
		return s.Location.URI
	}), capped, err
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
	return c.pushedDiagnostics(ctx, uri)
}

// PublishedCovering returns the diagnostics gopls has published for the file once they include every one of
// want (a pull of the same file). gopls matches a codeAction context diagnostic only against its published set,
// and it publishes the analysis results (the simplify hints fix-all needs) after an earlier, smaller publish.
// It waits up to PushDiagnosticsTimeout, then returns what it has.
func (c *Client) PublishedCovering(ctx context.Context, path string, want []Diagnostic) ([]Diagnostic, error) {
	uri, err := c.EnsureOpen(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(PushDiagnosticsTimeout)
	for {
		got, err := c.pushedDiagnostics(ctx, uri)
		if err != nil {
			return nil, err
		}
		if coversDiagnostics(got, want) || time.Now().After(deadline) {
			return got, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// coversDiagnostics says every diagnostic of want is in got (same message, range and source).
func coversDiagnostics(got, want []Diagnostic) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Message == w.Message && g.Range == w.Range && g.Source == w.Source {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// pushedDiagnostics is the push cache of uri (waiting up to PushDiagnosticsTimeout for a fresh one).
func (c *Client) pushedDiagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
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

// scanDisk lists the watched files below the root, capped so a huge tree cannot stall a call, and the
// module files: the ConfigNames below the root and the ExtraConfigFiles. Module files carry content hashes.
func (c *Client) scanDisk() (files, configs map[string]fileStamp) {
	files, configs = map[string]fileStamp{}, map[string]fileStamp{}
	if c.opts.WatchSuffix != "" || len(c.opts.ConfigNames) > 0 {
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
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			switch {
			case c.isConfigName(d.Name()):
				configs[p] = stampOf(p, info, true)
			case c.opts.WatchSuffix != "" && strings.HasSuffix(p, c.opts.WatchSuffix):
				files[p] = stampOf(p, info, false)
				if len(files) >= maxScanFiles {
					return filepath.SkipAll
				}
			}
			return nil
		})
	}
	if c.opts.ExtraConfigFiles != nil {
		for _, p := range c.opts.ExtraConfigFiles() {
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				configs[p] = stampOf(p, info, true)
			}
		}
	}
	return files, configs
}

// ConfigFingerprint is a stable digest of the module files gopls loads the workspace from (the ConfigNames
// below the root and the ExtraConfigFiles), read from the disk now: sorted root-relative paths with content
// hashes and no mtimes. It changes exactly when one of those files changes content, appears or disappears.
// The environment (GOWORK, GOFLAGS, ...) is not part of it.
func (c *Client) ConfigFingerprint() string {
	_, configs := c.scanDisk()
	h := sha256.New()
	for _, p := range sortedKeys(configs) {
		rel, err := filepath.Rel(c.opts.Root, p)
		if err != nil {
			rel = p
		}
		fmt.Fprintf(h, "%s\x00%s\n", filepath.ToSlash(rel), configs[p].sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Refresh brings the server's view up to date with the disk: open files that changed are re-synced, and
// files created, changed or deleted behind its back are reported via didChangeWatchedFiles. An open file
// that was replaced by a directory (or removed) is closed. A module file whose content changed while its
// mtime did not, or that was removed, is not reported: gopls keys its view of such a file on the mtime and
// would ignore the event, so the server is restarted instead.
func (c *Client) Refresh() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	now, configs := c.scanDisk()
	c.mu.Lock()
	prev, prevConfigs := c.snapshot, c.configs
	c.snapshot, c.configs = now, configs
	var stale []string
	var changes []map[string]any
	seen := map[string]bool{}
	for uri, f := range c.open {
		if _, ok := c.overlays[uri]; ok {
			continue
		}
		p := URIToPath(uri)
		seen[p] = true
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			stale = append(stale, uri)
			changes = append(changes, map[string]any{"uri": uri, "type": 3})
			continue
		}
		if text, err := readDocumentText(p); err == nil && contentSum(text) != f.sum {
			changes = append(changes, map[string]any{"uri": uri, "type": 2})
		}
	}
	c.mu.Unlock()
	restart := false
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
		for p, v := range configs {
			switch old, ok := prevConfigs[p]; {
			case !ok:
				changes = append(changes, map[string]any{"uri": PathToURI(p), "type": 1})
			case old.sum != v.sum:
				restart = restart || old.mtimeNs == v.mtimeNs
				changes = append(changes, map[string]any{"uri": PathToURI(p), "type": 2})
			}
		}
		for p := range prevConfigs {
			if _, ok := configs[p]; !ok {
				restart = true
				changes = append(changes, map[string]any{"uri": PathToURI(p), "type": 3})
			}
		}
	}
	if restart {
		return c.restartForModuleFiles()
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
				if text, err := readDocumentText(p); err == nil {
					_ = c.syncText(uri, text)
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

// restartForModuleFiles restarts a started server so it loads the module files that changed in place. The
// open documents are gone with the old process; the next call opens them again.
func (c *Client) restartForModuleFiles() error {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if !started {
		return nil
	}
	if c.opts.OnNotice != nil {
		c.opts.OnNotice("go.mod, go.sum or go.work changed on disk; restarting gopls")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return c.Restart(ctx)
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
func newDirFiles(prev, now map[string]fileStamp) []string {
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
