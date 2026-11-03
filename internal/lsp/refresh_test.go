package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeServerEnv makes TestFakeServer act as a language server (the test binary re-executed); fakeLogEnv names
// the file it appends every message it receives to, one JSON object per line.
const (
	fakeServerEnv = "CODENAV_FAKE_LSP"
	fakeLogEnv    = "CODENAV_FAKE_LSP_LOG"
)

var moduleFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// TestFakeServer is the language server the tests start. Its modes (comma separated) are "longline": a 100 KB
// stderr line before the first answer; "shortlines": then 300 KB of short stderr lines; "wedge": stop reading
// stdin after the initialize reply. Every request is answered with null, initialize with empty capabilities.
func TestFakeServer(t *testing.T) {
	mode := os.Getenv(fakeServerEnv)
	if mode == "" {
		t.Skip("helper process for the fake language server")
	}
	serveFake(mode)
	os.Exit(0)
}

func serveFake(mode string) {
	if strings.Contains(mode, "longline") {
		fmt.Fprintln(os.Stderr, strings.Repeat("x", 100000))
	}
	if strings.Contains(mode, "shortlines") {
		line := strings.Repeat("y", 99) + "\n"
		for range 3000 {
			_, _ = os.Stderr.WriteString(line)
		}
	}
	in := bufio.NewReader(os.Stdin)
	for {
		body, err := readFrame(in)
		if err != nil {
			return // stdin closed
		}
		logFake(body)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		switch {
		case msg.Method == "exit":
			os.Exit(0)
		case msg.Method == "initialize":
			fakeReply(msg.ID, `{"capabilities":{}}`)
			if strings.Contains(mode, "wedge") {
				time.Sleep(time.Hour)
			}
		case msg.ID != nil:
			fakeReply(msg.ID, "null")
		}
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
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
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

func fakeReply(id json.RawMessage, result string) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(body), body)
}

func logFake(body []byte) {
	path := os.Getenv(fakeLogEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(append([]byte{}, body...), '\n'))
}

type loggedMsg struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// newFakeClient returns a client for a fresh root that starts the fake server in mode; tweak adjusts the options.
func newFakeClient(t *testing.T, mode string, tweak ...func(*Options)) (*Client, string, string) {
	t.Helper()
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "fake.log")
	if mode == "" {
		mode = "plain"
	}
	t.Setenv(fakeServerEnv, mode)
	t.Setenv(fakeLogEnv, logPath)
	opts := Options{Root: root, Command: []string{os.Args[0], "-test.run=^TestFakeServer$"}, Language: "go",
		WatchSuffix: ".go", ConfigNames: moduleFiles}
	for _, f := range tweak {
		f(&opts)
	}
	return New(opts), root, logPath
}

func startClient(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop() })
}

// syncFake waits until the server has handled everything sent before it (it answers requests in order).
func syncFake(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Request(ctx, "workspace/symbol", map[string]any{"query": ""}); err != nil {
		t.Fatalf("sync request: %v", err)
	}
}

func fakeLog(t *testing.T, path string) []loggedMsg {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []loggedMsg
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var m loggedMsg
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func countMethod(msgs []loggedMsg, method string) int {
	n := 0
	for _, m := range msgs {
		if m.Method == method {
			n++
		}
	}
	return n
}

// watchedChanges returns the didChangeWatchedFiles types sent so far, by uri (the last one sent wins).
func watchedChanges(t *testing.T, path string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, m := range fakeLog(t, path) {
		if m.Method != "workspace/didChangeWatchedFiles" {
			continue
		}
		var p struct {
			Changes []struct {
				URI  string `json:"uri"`
				Type int    `json:"type"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			t.Fatal(err)
		}
		for _, ch := range p.Changes {
			out[ch.URI] = ch.Type
		}
	}
	return out
}

// sameMtime is the fixed mtime writeSame gives every file, so that rewrites keep the mtime exactly.
var sameMtime = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// writeSame writes content to path with the fixed mtime sameMtime, so a later rewrite of the same length
// keeps both size and mtime.
func writeSame(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, sameMtime, sameMtime); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// -- stderr reader and shutdown (fake server) ---------------------------------

func TestReadStderrCutsLongLinesAndDrains(t *testing.T) {
	long := strings.Repeat("x", 100000)
	input := long + "\n" + strings.Repeat("y", stderrLineKeep) + "\r\nafter\n" + "tail without newline"
	var got []string
	readStderr(strings.NewReader(input), func(s string) { got = append(got, s) })
	want := []string{long[:stderrLineKeep] + " ...", strings.Repeat("y", stderrLineKeep), "after", "tail without newline"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %d bytes %q..., want %d bytes", i, len(got[i]), got[i][:min(len(got[i]), 20)], len(want[i]))
		}
	}
}

func TestFloodedStderrDoesNotWedgeServer(t *testing.T) {
	for _, mode := range []string{"longline", "longline,shortlines"} {
		t.Run(mode, func(t *testing.T) {
			c, _, _ := newFakeClient(t, mode)
			startClient(t, c) // the server writes 100 KB (or 400 KB) to stderr before it answers initialize
			syncFake(t, c)
			start := time.Now()
			if err := c.Stop(); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("Stop took %v with a server that exits on shutdown", d)
			}
		})
	}
}

func TestStopBoundsWedgedServer(t *testing.T) {
	c, _, _ := newFakeClient(t, "wedge")
	startClient(t, c) // answers initialize, then never reads its stdin again
	start := time.Now()
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("Stop took %v with a server that ignores shutdown and stdin EOF; want about 4 s (kill)", d)
	}
}

// -- Refresh: files replaced, edited in place, module files (fake server) ----

func TestRefreshDirReplacesOpenFile(t *testing.T) {
	c, root, logPath := newFakeClient(t, "")
	startClient(t, c)
	file := filepath.Join(root, "a", "a.go")
	writeFile(t, file, "package a\n\nfunc A() {}\n")
	uri, err := c.EnsureOpen(file)
	if err != nil {
		t.Fatal(err)
	}
	// The file is replaced by a directory of the same name.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(file, "inner.txt"), "x\n")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	syncFake(t, c)
	if got, ok := watchedChanges(t, logPath)[uri]; !ok || got != 3 {
		t.Errorf("didChangeWatchedFiles for %s: type %d (sent: %v), want 3 (deleted)", uri, got, ok)
	}
	c.mu.Lock()
	_, open := c.open[uri]
	c.mu.Unlock()
	if open {
		t.Errorf("the document is still open after its file became a directory")
	}
}

func TestEnsureOpenSeesSameSizeEdit(t *testing.T) {
	c, root, logPath := newFakeClient(t, "")
	startClient(t, c)
	file := filepath.Join(root, "main.go")
	writeSame(t, file, "package main\n\nfunc main() { println(\"Hello\") }\n")
	if _, err := c.EnsureOpen(file); err != nil {
		t.Fatal(err)
	}
	// Same length and same mtime, different content: the document must still be re-sent.
	writeSame(t, file, "package main\n\nfunc main() { println(\"Hallo\") }\n")
	if _, err := c.EnsureOpen(file); err != nil {
		t.Fatal(err)
	}
	syncFake(t, c)
	changed := false
	for _, m := range fakeLog(t, logPath) {
		if m.Method == "textDocument/didChange" && strings.Contains(string(m.Params), "Hallo") {
			changed = true
		}
	}
	if !changed {
		t.Errorf("no didChange with the edited text after a same-size, same-mtime edit")
	}
}

func TestRefreshSendsModuleFileChangeWithoutRestart(t *testing.T) {
	c, root, logPath := newFakeClient(t, "")
	startClient(t, c)
	gomod := filepath.Join(root, "go.mod")
	writeSame(t, gomod, "module example.com/m\n\ngo 1.22\n")
	if err := c.Refresh(); err != nil { // baseline
		t.Fatal(err)
	}
	// A new mtime: gopls reloads on the event, so no restart.
	writeFile(t, gomod, "module example.com/n\n\ngo 1.22\n")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	syncFake(t, c)
	if got := watchedChanges(t, logPath)[PathToURI(gomod)]; got != 2 {
		t.Errorf("go.mod change sent as type %d, want 2 (changed)", got)
	}
	if n := countMethod(fakeLog(t, logPath), "initialize"); n != 1 {
		t.Errorf("server initialized %d times, want 1 (no restart)", n)
	}
}

func TestRefreshRestartsForInPlaceModuleEdit(t *testing.T) {
	var notices []string
	c, root, logPath := newFakeClient(t, "", func(o *Options) {
		o.OnNotice = func(s string) { notices = append(notices, s) }
	})
	startClient(t, c)
	gomod := filepath.Join(root, "go.mod")
	writeSame(t, gomod, "module example.com/m\n\ngo 1.22\n")
	if err := c.Refresh(); err != nil { // baseline
		t.Fatal(err)
	}
	// Same length, mtime kept: gopls keys its view on the mtime and would ignore an event, so restart.
	writeSame(t, gomod, "module example.com/q\n\ngo 1.22\n")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	syncFake(t, c)
	if n := countMethod(fakeLog(t, logPath), "initialize"); n != 2 {
		t.Errorf("server initialized %d times, want 2 (restart)", n)
	}
	if len(notices) != 1 {
		t.Errorf("notices = %q, want one line about the restart", notices)
	}
}

func TestConfigFingerprintTracksModuleFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "go.mod")
	writeFile(t, outside, "module example.com/outer\n\ngo 1.22\n")
	c := New(Options{Root: root, ConfigNames: moduleFiles, ExtraConfigFiles: func() []string { return []string{outside} }})
	gomod := filepath.Join(root, "go.mod")
	writeSame(t, gomod, "module example.com/m\n\ngo 1.22\n")
	fp0 := c.ConfigFingerprint()
	if fp1 := c.ConfigFingerprint(); fp1 != fp0 {
		t.Fatalf("fingerprint is not stable: %s then %s", fp0, fp1)
	}
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	if c.ConfigFingerprint() != fp0 {
		t.Errorf("a Go source file changed the fingerprint")
	}
	// Same size and mtime, other content.
	writeSame(t, gomod, "module example.com/q\n\ngo 1.22\n")
	fp1 := c.ConfigFingerprint()
	if fp1 == fp0 {
		t.Errorf("go.mod rewritten in place with the same size and mtime: fingerprint unchanged")
	}
	if err := os.Chtimes(gomod, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.ConfigFingerprint() != fp1 {
		t.Errorf("a touch without a content change changed the fingerprint")
	}
	writeFile(t, filepath.Join(root, "go.work"), "go 1.22\n")
	fp2 := c.ConfigFingerprint()
	if fp2 == fp1 {
		t.Errorf("go.work created: fingerprint unchanged")
	}
	writeFile(t, outside, "module example.com/outer2\n\ngo 1.22\n")
	if c.ConfigFingerprint() == fp2 {
		t.Errorf("a go.mod above the root changed: fingerprint unchanged")
	}
}

// -- gopls: does the server reload on a module file change? (skipped without gopls) --

func goplsCommand(t *testing.T) string {
	t.Helper()
	g, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not installed")
	}
	return g
}

// writeModule makes a module mod with package lib and a main.go that calls lib.L.
func writeModule(t *testing.T, root, mod string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "go.mod"), "module "+mod+"\n\ngo 1.22\n")
	writeFile(t, filepath.Join(root, "lib", "lib.go"), "package lib\n\nfunc L() int { return 1 }\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport \""+mod+"/lib\"\n\nfunc main() { _ = lib.L() }\n")
}

func startGopls(t *testing.T, root string) (*Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	c := New(Options{Root: root, Command: []string{goplsCommand(t)}, Language: "go", WatchSuffix: ".go",
		ConfigNames: moduleFiles, InitOptions: map[string]any{"symbolScope": "workspace"}})
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	return c, ctx
}

func TestGoplsReloadsInPlaceGoModEdit(t *testing.T) {
	root := t.TempDir()
	writeModule(t, root, "example.com/m")
	writeSame(t, filepath.Join(root, "go.mod"), "module example.com/m\n\ngo 1.22\n")
	c, ctx := startGopls(t, root)
	if err := c.Refresh(); err != nil { // baseline
		t.Fatal(err)
	}
	if d, err := c.Diagnostics(ctx, "main.go"); err != nil || len(d) != 0 {
		t.Fatalf("baseline diagnostics = %v, %v", d, err)
	}
	// Same length, mtime kept: the module path changes, so main.go's import no longer resolves.
	writeSame(t, filepath.Join(root, "go.mod"), "module example.com/q\n\ngo 1.22\n")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	d, err := c.Diagnostics(ctx, "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(d) == 0 {
		t.Errorf("no diagnostic after go.mod was edited in place: gopls kept the old module")
	}
}

func TestGoplsReloadsOnGoWorkCreated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/m\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n")
	writeFile(t, filepath.Join(root, "dep", "go.mod"), "module example.com/dep\n\ngo 1.22\n")
	writeFile(t, filepath.Join(root, "dep", "dep.go"), "package dep\n\nfunc Hello() string { return \"hi\" }\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport \"example.com/dep\"\n\nfunc main() { println(dep.Hello()) }\n")
	c, ctx := startGopls(t, root)
	if err := c.Refresh(); err != nil { // baseline
		t.Fatal(err)
	}
	if d, err := c.Diagnostics(ctx, "main.go"); err != nil || len(d) == 0 {
		t.Fatalf("baseline: want an unresolved import, got %v, %v", d, err)
	}
	// The go.work replace makes the import resolve: the created file must reach gopls.
	writeFile(t, filepath.Join(root, "go.work"), "go 1.22\n\nuse .\n\nreplace example.com/dep v0.0.0 => ./dep\n")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	if d, err := c.Diagnostics(ctx, "main.go"); err != nil || len(d) != 0 {
		t.Errorf("after go.work created: diagnostics %v, %v; want none", d, err)
	}
}

func TestGoplsSeesSameSizeEditOfOpenFile(t *testing.T) {
	root := t.TempDir()
	writeModule(t, root, "example.com/m")
	main := filepath.Join(root, "main.go")
	writeSame(t, main, "package main\n\nimport \"example.com/m/lib\"\n\nfunc main() { _ = lib.L() }\n")
	c, ctx := startGopls(t, root)
	if d, err := c.Diagnostics(ctx, "main.go"); err != nil || len(d) != 0 {
		t.Fatalf("baseline diagnostics = %v, %v", d, err)
	}
	// Same size and mtime: lib.L becomes lib.M, which does not exist.
	writeSame(t, main, "package main\n\nimport \"example.com/m/lib\"\n\nfunc main() { _ = lib.M() }\n")
	d, err := c.Diagnostics(ctx, "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(d) == 0 {
		t.Errorf("no diagnostic for lib.M after a same-size, same-mtime edit of the open file")
	}
}
