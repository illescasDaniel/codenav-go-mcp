// Package server wires the MCP server: workspace selection, the gopls client lifecycle and tools.
package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/notices"
	"github.com/illescasDaniel/codenav-go-mcp/internal/tools"
	"github.com/illescasDaniel/codenav-go-mcp/internal/txn"
	"github.com/illescasDaniel/codenav-go-mcp/internal/workspace"
)

const (
	ServerName = "codenav-go-mcp"

	EnvWorkspace  = "CODENAV_GO_MCP_WORKSPACE"
	EnvReadOnly   = "CODENAV_GO_MCP_READ_ONLY"
	EnvGopls      = "CODENAV_GO_MCP_GOPLS"
	EnvBuildFlags = "CODENAV_GO_MCP_BUILD_FLAGS"
)

// Manager owns the selected workspace and the gopls client serving it.
type Manager struct {
	selector *workspace.Selector
	board    *notices.Board

	mu     sync.Mutex
	client *lsp.Client
	// rootsMu guards the roots/list failure state below (see clientRoots); it is never held during a call.
	rootsMu      sync.Mutex
	rootsFailed  rootsClient // the session whose last roots/list failed
	rootsRetryAt time.Time   // no new roots/list to rootsFailed before this
	index        *index.Index
	sel          workspace.Selection
	journal      *txn.Journal
	store        *txn.PlanStore
}

func NewManager() *Manager {
	return &Manager{selector: workspace.NewSelector(EnvWorkspace), board: notices.NewBoard(ServerName)}
}

func (m *Manager) Notes() *notices.Board { return m.board }

func (m *Manager) Selector() *workspace.Selector { return m.selector }

// rootsClient is the part of a server session that roots/list needs; *mcp.ServerSession satisfies it.
type rootsClient interface {
	InitializeParams() *mcp.InitializeParams
	ListRoots(context.Context, *mcp.ListRootsParams) (*mcp.ListRootsResult, error)
}

// rootsRetryAfter is how long a session whose roots/list failed (or timed out) is left alone before it is asked
// again. Without it every tool call would wait up to 3 s for the same answer. It is a variable so tests can
// shorten it.
var rootsRetryAfter = 30 * time.Second

// clientRoots asks the client for its roots. A pinned workspace ignores roots, so the client is not asked at
// all. A failure is remembered for rootsRetryAfter, unless the caller's own context ended (a cancelled tool
// call says nothing about the client). A notifications/roots/list_changed clears the remembered failure.
func (m *Manager) clientRoots(ctx context.Context, sess rootsClient) []string {
	if sess == nil || m.selector.Pinned {
		return nil
	}
	m.rootsMu.Lock()
	skip := m.rootsFailed == sess && time.Now().Before(m.rootsRetryAt)
	m.rootsMu.Unlock()
	if skip {
		return nil
	}
	if p := sess.InitializeParams(); p == nil || p.Capabilities == nil || (!p.Capabilities.Roots.ListChanged && p.Capabilities.RootsV2 == nil) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := sess.ListRoots(cctx, nil)
	if err != nil || res == nil {
		if ctx.Err() == nil {
			m.rootsMu.Lock()
			m.rootsFailed, m.rootsRetryAt = sess, time.Now().Add(rootsRetryAfter)
			m.rootsMu.Unlock()
		}
		return nil
	}
	m.rootsMu.Lock()
	if m.rootsFailed == sess {
		m.rootsFailed = nil
	}
	m.rootsMu.Unlock()
	uris := make([]string, 0, len(res.Roots))
	for _, r := range res.Roots {
		uris = append(uris, r.URI)
	}
	return uris
}

// rootsChanged clears the remembered roots/list failure when the client reports that its roots changed.
func (m *Manager) rootsChanged(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "notifications/roots/list_changed" {
			m.rootsMu.Lock()
			m.rootsFailed = nil
			m.rootsMu.Unlock()
		}
		return next(ctx, method, req)
	}
}

// noteRoot posts the notices a newly selected root needs.
func (m *Manager) noteRoot(root string) {
	if !workspace.IsGoProject(root) {
		m.board.Post("not a Go project (no go.mod or go.work); symbol answers will be empty")
	}
}

// Acquire returns a started client for the current workspace, switching and restarting as needed.
func (m *Manager) Acquire(ctx context.Context, sess *mcp.ServerSession) (*tools.Workspace, error) {
	var rc rootsClient
	if sess != nil {
		rc = sess
	}
	sel := m.selector.Select(m.clientRoots(ctx, rc))
	if m.selector.Pinned {
		if st, err := os.Stat(sel.Root); err != nil || !st.IsDir() {
			// Not wrapping the stat error keeps it out of the "File not found" rewording of the tool errors.
			return nil, fmt.Errorf("$%s=%s is not a directory", EnvWorkspace, os.Getenv(EnvWorkspace))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil && (sel.Root != m.sel.Root || !m.client.Alive()) {
		if sel.Root != m.sel.Root {
			m.board.Post(fmt.Sprintf("workspace switched to %s", sel.Root))
		} else {
			m.board.Post("gopls exited unexpectedly and was restarted")
		}
		_ = m.client.Stop()
		m.client = nil
	}
	if m.client == nil {
		cmd, warn, err := GoplsCommand()
		if err != nil {
			return nil, err
		}
		if warn != "" {
			m.board.Post(warn)
		}
		root := sel.Root
		if root != m.sel.Root {
			m.noteRoot(root)
		}
		c := lsp.New(lsp.Options{Root: root, Command: cmd, Language: "go", InitOptions: goplsSettings(), OnNotice: m.board.Post,
			WatchSuffix: ".go", SkipDir: exclude.IsDirName, ExtraFolders: uncoveredDirs(root),
			ConfigNames:      []string{"go.mod", "go.sum", "go.work", "go.work.sum"},
			ExtraConfigFiles: func() []string { return gomod.ConfigFiles(root) }})
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
		m.client = c
		m.index = index.New(sel.Root)
		switch {
		case m.journal == nil:
			m.journal, m.store = txn.NewJournal(guardFor(sel.Root)), txn.NewPlanStore()
			m.journal.Notify = m.board.Post
			m.journal.CheckPending()
		case sel.Root != m.sel.Root:
			m.journal.Rebind(guardFor(sel.Root)) // posts a notice when undo history is dropped
			m.journal.CheckPending()
			m.store.Clear("workspace switch to " + sel.Root)
		}
		// Same workspace after a gopls restart: edits and previews are still valid.
		m.sel = sel
	}
	if err := m.client.Refresh(); err != nil {
		return nil, err
	}
	if err := m.index.Refresh(); err != nil {
		return nil, err
	}
	// Modules found after the start (or listed in a go.work since) join or leave the server's folders.
	if err := m.client.SyncFolders(uncoveredDirs(m.sel.Root)); err != nil {
		return nil, err
	}
	return &tools.Workspace{Client: m.client, Index: m.index, Root: m.sel.Root, Source: m.sel.Source, Journal: m.journal, Store: m.store}, nil
}

// maxExtraFolders bounds how many uncovered nested modules become gopls workspace folders.
const maxExtraFolders = 20

// uncoveredDirs lists the modules below root that its go.mod/go.work does not cover: gopls only
// indexes the packages of a workspace folder (or of a file opened in them) for such modules.
func uncoveredDirs(root string) []string {
	var out []string
	for _, n := range gomod.UncoveredModules(root, exclude.IsDirName, maxExtraFolders) {
		out = append(out, n.Dir)
	}
	return out
}

func goplsSettings() map[string]any {
	s := map[string]any{
		// Keep the standard library and dependencies out of workspace/symbol.
		"symbolScope": "workspace",
	}
	if f := strings.Fields(os.Getenv(EnvBuildFlags)); len(f) > 0 {
		s["buildFlags"] = f
	}
	return s
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		_ = m.client.Stop()
		m.client = nil
	}
}

// New builds the MCP server with all tools registered.
func New(version string) (*mcp.Server, *Manager) {
	m := NewManager()
	if os.Getenv("GOCACHE") == "off" {
		m.board.Post("GOCACHE=off: references and cgo hover can be incomplete")
	}
	readOnly := readOnlyFromEnv()
	instructions := "Navigate Go code with gopls: symbol_info, search_symbol, definition, references, implementations, callers, outline, hover, diagnostics. " +
		"Edit it with edit, edit_symbol, rename_symbol, change_signature, move, quick_fix: each type-checks the change in memory first and writes only if it adds no new errors " +
		"(apply=false previews; apply_edit applies a preview, undo_edit reverts, verify_changes re-checks the workspace)."
	if readOnly {
		instructions = "Navigate Go code with gopls: symbol_info, search_symbol, definition, references, implementations, callers, outline, hover, diagnostics. " +
			"The edit tools (edit, edit_symbol, rename_symbol, change_signature, move, quick_fix) only preview here: this server is read-only, so they type-check the change in memory and show the diff and the new errors, but write nothing. " +
			"verify_changes re-checks the workspace."
	}
	s := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, &mcp.ServerOptions{Instructions: instructions})
	s.AddReceivingMiddleware(tools.FriendlyValidation)
	s.AddReceivingMiddleware(m.rootsChanged)
	tools.RegisterNav(s, m)
	tools.RegisterWrite(s, m, readOnly)
	return s, m
}

// Explain forwards to the workspace selector.
func (m *Manager) Explain(source string) string { return m.selector.Explain(source) }

func goEnv(name string) string {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readOnlyFromEnv reports whether CODENAV_GO_MCP_READ_ONLY asks for a server that never writes.
func readOnlyFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvReadOnly))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func guardFor(root string) txn.WriteGuard {
	g := txn.WriteGuard{Root: root, MaxFiles: txn.DefaultMaxFiles, Extra: tools.GoGuardRule, ReadOnly: readOnlyFromEnv()}
	for _, d := range []string{goEnv("GOROOT"), goEnv("GOMODCACHE")} {
		if d != "" {
			g.NeverWrite = append(g.NeverWrite, d)
		}
	}
	return g
}
