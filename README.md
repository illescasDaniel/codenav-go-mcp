# codenav-go-mcp

An MCP server that gives AI agents type-resolved **Go** navigation, backed by
[gopls](https://pkg.go.dev/golang.org/x/tools/gopls). Definitions, references,
implementations and call sites go through the real type checker, not text grep.

The same resolution powers its edits: rename a symbol, change a signature, or move a
function, type or whole package, and every usage follows, including the methods that
implement an interface. Edits are type-checked and previewable before they are written,
and can be undone.

It is the Go sibling of `codenav-mcp` (Python/ty): same tool names and parameters.

## Quick start

You need Go and `gopls` (`go install golang.org/x/tools/gopls@latest`).

```bash
go install github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest
claude mcp add codenav-go -- codenav-go-mcp
```

Or without installing, in a project `.mcp.json` (Claude Code) / `.cursor/mcp.json` (Cursor):

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

See [.mcp.json.example](.mcp.json.example). In Cursor also set
`"CODENAV_GO_MCP_WORKSPACE": "${workspaceFolder}"`, because Cursor may start MCP servers
from your home directory. Prebuilt binaries are attached to each GitHub release.

## Tools

| Tool | Answers |
|------|---------|
| `symbol_info` | What is this? Header, hover, definition and references in one call |
| `outline` | What's in this file? |
| `callers` | Who calls this function? (call hierarchy) |
| `implementations` | Which types implement this interface, or which interfaces does this type satisfy? |
| `search_symbol` | Workspace symbol search (ranked; `kind=`, `path=`, `fuzzy=` filters) |
| `workspace` | Which directory is being navigated, and why |
| `hover`, `definition`, `references`, `diagnostics` | Position tools (`path:line:col`) |

Names may be dotted: `Type.Method`, `Type.Field`, `pkg.Name`. `name` and `query` are
aliases; a missing parameter gets a short hint instead of a validation error.

## Write tools: type check before every write

The edit is applied to in-memory copies first (a gopls overlay), gopls re-checks the edited
files, their package and the packages importing them (including external `_test`
packages), and the response says which diagnostics the edit adds or fixes. Nothing is
written until that is on the table. Output is always gofmt-ed with imports fixed.

| Tool | Does |
|------|------|
| `edit` | Type-checked text edit of a Go file: `old_string`/`new_string` or a whole `new_text` |
| `edit_symbol` | `replace` / `insert` / `delete` a func, method, type, const or var by name; `imports=[...]` added. Delete refuses while the symbol is used |
| `rename_symbol` | Rename across the workspace (`parameter=` renames a function's parameter instead). Interface methods are renamed together with every implementer (gopls alone leaves them behind and breaks the interface). Lists remaining mentions in comments, strings and docs |
| `change_signature` | Add / remove / reorder parameters and rewrite every call site; linked methods follow. Uses as a value are listed as manual work |
| `move` | With `name`: move a top-level func, type, const or var to another file, in the same or another package (qualifiers and imports rewritten; refuses unexported-use problems, import cycles and name clashes). Without `name`: move a package directory (`file_path` -> `to_file`) and rewrite every import |
| `quick_fix` | Apply gopls's quick fixes and organize imports; ambiguous fixes are listed instead of guessed |
| `verify_changes` | Which errors the working tree gained or lost since a git revision (default `HEAD`) |
| `apply_edit` / `undo_edit` | Write a previewed edit by id; revert an applied one |

Every write tool writes when the edit adds no more than `max_new_errors` (default 0; `-1` =
unlimited) errors. Otherwise nothing is written and you get the diagnostics plus an id for
`apply_edit`. `apply=false` always previews.

Safety rules, enforced in code:

- Only `.go` files inside the workspace are written. `vendor/`, `.git`, `node_modules`,
  GOROOT, GOMODCACHE, generated files (`Code generated ... DO NOT EDIT`) and symlinks
  leaving the workspace are refused.
- A preview remembers the exact bytes of every file; `apply_edit` refuses if any file
  changed since. Files are replaced atomically; a failure part-way restores the written ones.
- Every applied edit is undoable unless the files changed afterwards.
- Code that does not parse is never written. CRLF and BOM are preserved.
- Edits touching more than 40 files need `allow_large=true`.
- `CODENAV_GO_MCP_READ_ONLY=1` turns every write into a refusal (previews still work); `apply_edit` and `undo_edit` are not listed.

Positions are 1-indexed; `column` is a UTF-16 offset.

`move` also takes a spec out of a `const`/`var`/`type` block or a `var a, b = ...` line (refused
when that would change `iota` values, or for `var a, b = f()`), and a method to another file of
its own package (a type's methods follow it when it changes package).

Known limits: package moves refuse directories holding non-Go files; code excluded by build
constraints is invisible to the type check (the response says when an edit touches it).

## Limitations

- **Nested modules need a `go.work`.** gopls loads only the modules that the root `go.mod` or `go.work`
  covers. A directory with its own `go.mod` that is not listed in `go.work` is invisible to name
  lookups, and edits to it are not type-checked. Tools say so when they meet one; run
  `go work init . ./sub` (plus `go work use` for more) to include it.
- **Name lookups cover the workspace only.** `name="fmt.Println"` cannot resolve a standard-library or
  dependency symbol. Use `hover` or `definition` with `file_path`/`line`/`column` on a use of it.
- **Large workspaces:** gopls returns at most 100 symbols per query. An exact name hidden among more
  fuzzy matches is still found, because a suffix query and a qualified query are retried, but `search_symbol`
  output for broad queries is truncated.
- **New directories:** files written outside the tools are picked up on the next call. A brand-new
  package directory is loaded by opening one of its files, so the first call after creating it can
  take a moment longer.
- **Not offered by gopls, so not by `quick_fix`:** removing an unused local variable (use `edit`).

## Which `gopls` runs

1. `$CODENAV_GO_MCP_GOPLS`
2. `$(go env GOBIN)` / `$(go env GOPATH)/bin`
3. `gopls` on `PATH`
4. `go run golang.org/x/tools/gopls@latest` as a last resort (with a warning)

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `CODENAV_GO_MCP_WORKSPACE` | unset: follows the client's MCP roots when they name a worktree of the same git repository, else `CLAUDE_PROJECT_DIR`, else the working directory | Pins the project root |
| `CODENAV_GO_MCP_READ_ONLY` | unset | `1`/`true`: write tools refuse to write |
| `CODENAV_GO_MCP_GOPLS` | auto | Path to the gopls binary |
| `CODENAV_GO_MCP_BUILD_FLAGS` | none | Passed to gopls as `buildFlags`, e.g. `-tags=integration` |

## Development

```bash
go vet ./... && go test ./...
```

Live tests run against `testdata/fixture` and are skipped when gopls is missing.
The design and the gopls findings behind it are in [PLAN.md](PLAN.md).

## License

MIT
