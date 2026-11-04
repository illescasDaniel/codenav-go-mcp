# Examples

`shop/` is a small Go module (a cart, its items and a pricer) to try the server on. It is its own module, so
it is not part of the `codenav-go-mcp` build, binary or tests of the root module, but the integration tests in
`internal/server` run the real server against a copy of it.

You need `gopls` on your PATH. With no `CODENAV_GO_MCP_WORKSPACE`, the server uses the project the client reports, or the
directory it is started in, so start your client inside `examples/shop`. No environment variable is needed.

## Claude Code

Add a `.mcp.json` to `examples/shop` (the same as [`.mcp.json.example`](../.mcp.json.example) at the repository root), then start
`claude` there and approve the server:

```json
{
	"mcpServers": {
		"codenav-go": {
			"command": "go",
			"args": ["run", "github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest"]
		}
	}
}
```

Or from `examples/shop`: `claude mcp add codenav-demo -- go run github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest`.

## Codex

Add this to `.codex/config.toml` in `examples/shop` (a trusted project), then start `codex` there:

```toml
[mcp_servers.codenav-go]
command = "go"
args = ["run", "github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest"]
startup_timeout_sec = 60
```

Or from `examples/shop`: `codex mcp add codenav-go -- go run github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest`
(that one is user-wide). The first start needs extra time to download and build the server.

## MCP Inspector

Run from the repository root; the relative workspace is resolved against the server's working directory.

```bash
npx @modelcontextprotocol/inspector -e CODENAV_GO_MCP_WORKSPACE=examples/shop go run ./cmd/codenav-go-mcp
```

## Things to try

Ask for `symbol_info` with `{"name": "Cart"}`, `implementations` with `{"name": "Pricer"}`, or `rename_symbol` with
`apply=false` to see a preview. Ask the agent to use `workspace` to confirm which project the server sees.

To use a local build instead of the published one (`go run` cannot reach it from inside the example's module), run
`go build -o /tmp/codenav-go-mcp ./cmd/codenav-go-mcp` from the repository root and use `/tmp/codenav-go-mcp` as the command.

Write tools change the files in `examples/shop`. Set `CODENAV_GO_MCP_READ_ONLY=1` in the server's `env` to keep them read-only,
or run `git checkout examples/shop` afterwards.
