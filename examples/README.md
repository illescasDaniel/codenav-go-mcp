# Examples

`shop/` is a small Go module (a cart, its items and a pricer) to try the server on. It is its own module, so
it is not part of the `codenav-go-mcp` build, binary or tests of the root module, but the integration tests in
`internal/server` run the real server against a copy of it.

Point the server at it with `CODENAV_GO_MCP_WORKSPACE`. You need `gopls` on your PATH.

## MCP Inspector

```bash
CODENAV_GO_MCP_WORKSPACE="$PWD/examples/shop" npx @modelcontextprotocol/inspector go run ./cmd/codenav-go-mcp
```

Then call `symbol_info` with `{"name": "Cart"}`, `implementations` with `{"name": "Pricer"}`, or
`rename_symbol` with `apply=false` to see a preview.

## Claude Desktop

In `claude_desktop_config.json` (use absolute paths; build the binary first with `go build -o codenav-go-mcp ./cmd/codenav-go-mcp`):

```json
{
  "mcpServers": {
    "codenav-go-demo": {
      "command": "/absolute/path/to/codenav-go-mcp",
      "env": { "CODENAV_GO_MCP_WORKSPACE": "/absolute/path/to/codenav-go-mcp/examples/shop" }
    }
  }
}
```

Write tools change the files in `examples/shop`. Add `"CODENAV_GO_MCP_READ_ONLY": "1"` to the env to keep them read-only,
or use `git checkout examples/shop` afterwards.
