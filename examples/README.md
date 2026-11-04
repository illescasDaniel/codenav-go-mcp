# Examples

`shop/` is a small Go module (a cart, its items and a pricer) to try the server on. It is its own module, so
it is not part of the `codenav-go-mcp` build, binary or tests of the root module, but the integration tests in
`internal/server` run the real server against a copy of it.

You need `gopls` on your PATH. With no `CODENAV_GO_MCP_WORKSPACE`, the server uses the directory it is started in (or the
project the client reports), so the simplest way is to start your client inside `examples/shop`. A relative
`CODENAV_GO_MCP_WORKSPACE` such as `.` also works; it is resolved against the server's working directory.

## Claude Code

```bash
cd examples/shop
claude mcp add codenav-demo -- go run github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest
```

To use a local build instead of the published one (`go run` cannot reach it from inside the example's module), build it first with `go build -o /tmp/codenav-go-mcp ./cmd/codenav-go-mcp` from the repository root and register `/tmp/codenav-go-mcp`.

## MCP Inspector

Run from the repository root; the relative workspace is resolved against the server's working directory.

```bash
npx @modelcontextprotocol/inspector -e CODENAV_GO_MCP_WORKSPACE=examples/shop go run ./cmd/codenav-go-mcp
```

Then call `symbol_info` with `{"name": "Cart"}`, `implementations` with `{"name": "Pricer"}`, or
`rename_symbol` with `apply=false` to see a preview.

## Claude Desktop

Claude Desktop does not start servers in your project, so here the workspace must be an absolute path (a relative one would
resolve against Desktop's own directory). In `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "codenav-go-demo": {
      "command": "go",
      "args": ["run", "github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest"],
      "env": { "CODENAV_GO_MCP_WORKSPACE": "/absolute/path/to/examples/shop" }
    }
  }
}
```

`go` must be on the PATH that Claude Desktop sees; if it is not, put the full path to `go` in `command`.

Write tools change the files in `examples/shop`. Add `"CODENAV_GO_MCP_READ_ONLY": "1"` to the env to keep them read-only,
or use `git checkout examples/shop` afterwards.
