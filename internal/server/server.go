// Package server wires the MCP server: workspace selection, the gopls client lifecycle and tools.
package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/notices"
	"github.com/illescasDaniel/codenav-go-mcp/internal/tools"
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
	sel    workspace.Selection
}

func NewManager() *Manager {
	return &Manager{selector: workspace.NewSelector(EnvWorkspace), board: notices.NewBoard(ServerName)}
}

func (m *Manager) Notes() *notices.Board { return m.board }

func (m *Manager) Selector() *workspace.Selector { return m.selector }

func clientRoots(ctx context.Context, sess *mcp.ServerSession) []string {
	if sess == nil {
		return nil
	}
	if p := sess.InitializeParams(); p == nil || p.Capabilities == nil || (!p.Capabilities.Roots.ListChanged && p.Capabilities.RootsV2 == nil) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := sess.ListRoots(ctx, nil)
	if err != nil || res == nil {
		return nil
	}
	uris := make([]string, 0, len(res.Roots))
	for _, r := range res.Roots {
		uris = append(uris, r.URI)
	}
	return uris
}

// Acquire returns a started client for the current workspace, switching and restarting as needed.
func (m *Manager) Acquire(ctx context.Context, sess *mcp.ServerSession) (*tools.Workspace, error) {
	sel := m.selector.Select(clientRoots(ctx, sess))
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
		c := lsp.New(lsp.Options{Root: sel.Root, Command: cmd, Language: "go", InitOptions: goplsSettings()})
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
		m.client, m.sel = c, sel
	}
	if err := m.client.Refresh(); err != nil {
		return nil, err
	}
	return &tools.Workspace{Client: m.client, Root: m.sel.Root, Source: m.sel.Source}, nil
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
	s := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, &mcp.ServerOptions{
		Instructions: "Navigate Go code with gopls: symbol_info, search_symbol, definition, references, implementations, callers, outline, hover, diagnostics.",
	})
	tools.RegisterNav(s, m)
	return s, m
}

// Explain forwards to the workspace selector.
func (m *Manager) Explain(source string) string { return m.selector.Explain(source) }
