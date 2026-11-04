# codenav-go-mcp

[![Go Reference](https://pkg.go.dev/badge/github.com/illescasDaniel/codenav-go-mcp.svg)](https://pkg.go.dev/github.com/illescasDaniel/codenav-go-mcp)
[![Release](https://img.shields.io/github/v/release/illescasDaniel/codenav-go-mcp)](https://github.com/illescasDaniel/codenav-go-mcp/releases)
[![CI](https://github.com/illescasDaniel/codenav-go-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/illescasDaniel/codenav-go-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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

### Codex

For a project-specific setup, add the following to `.codex/config.toml` in the
root of a trusted project:

```toml
[mcp_servers.codenav-go]
command = "go"
args = ["run", "github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest"]
startup_timeout_sec = 60
```

The first start may need extra time to download and build the server. For a
user-wide setup instead, run:

```bash
codex mcp add codenav-go -- go run github.com/illescasDaniel/codenav-go-mcp/cmd/codenav-go-mcp@latest
```

The CLI command writes to your user-wide Codex configuration. `@latest` follows
new releases; replace it with a version such as `@v1.0.0` to pin one. Start a
new Codex session or reload the client, approve the server if prompted, and use
`codex mcp list` or `/mcp` to check it. Ask Codex to use `workspace` to confirm
which project the server sees. For projects using build tags, set
`CODENAV_GO_MCP_BUILD_FLAGS` in the server's `env` table; see the environment
variables below. See [Codex MCP configuration](https://developers.openai.com/codex/mcp)
for client settings.

## Stability

From v1.0.0 the public surface is the MCP interface: tool names, parameter names and types, the `isError`
contract (see below) and the environment variables. Changes to it are additive (new tools, new optional
parameters, new output fields); a removal or a changed meaning would be a major version. The wording of
answers and refusals is written for agents to read and can change in any release, so don't parse it. The Go
packages under `internal/` are not an API. Known limits are in [docs/KNOWN-ISSUES.md](docs/KNOWN-ISSUES.md).

## Tools

| Tool | Answers |
|------|---------|
| `symbol_info` | What is this? Header, hover, definition and references in one call |
| `outline` | What's in this file? Blank `func _()` declarations are not listed (nothing can refer to them) |
| `callers` | Who calls this function? (call hierarchy) |
| `implementations` | Which types implement this interface, or which interfaces does this type satisfy? |
| `search_symbol` | Workspace symbol search (ranked; `kind=`, `path=`, `fuzzy=` filters). Exact and prefix matches come from an own declaration index, so they are never capped or missed and give the count; gopls adds the fuzzy ones after them, as a labelled sample when it is capped at 100 |
| `workspace` | Which directory is being navigated, and why |
| `hover`, `definition`, `references` | Position tools: `file_path`, `line`, `column` (1-indexed). `diagnostics` takes `file_path` only |

Names may be dotted: `Type.Method`, `Type.Field`, `pkg.Name` (`pkg` is the package name or its directory's name). `name` and `query` are
aliases. A missing required parameter (of any tool; an explicit JSON `null` counts as missing, and as not given for an optional one) gets `Missing required parameter(s): x, y.` plus the tool's usage, not a raw schema error.

## Example

Ask `symbol_info` about a type and get its declaration, doc comment, methods, definition and every use in one call.

The example project is in [examples/shop](examples/README.md) (try it with the MCP Inspector or Claude Desktop).

Request (`tools/call`):

```json
{
  "name": "symbol_info",
  "arguments": { "name": "Cart" }
}
```

Response (one text block, shown here as the agent reads it):

````text
Cart  [struct]  (cart/cart.go:5:6)

```go
type Cart struct { // size=40 (0x28), class=48 (0x30)
	Items    []Item
	Currency string
}
```

---

Cart is a customer's basket. The zero value is an empty cart.

```go
func (c *Cart) Add(it Item)
func (c *Cart) Total() int64
```

Definition:
cart/cart.go:5:6
    3 |
    4 | // Cart is a customer's basket. The zero value is an empty cart.
    5 | type Cart struct {
    6 | 	Items    []Item
    7 | 	Currency string

References:
4 reference(s) in 2 file(s):
cart/cart.go: L5, L18, L27
cmd/shop/main.go: L10
````

On the wire the result is `{"content": [{"type": "text", "text": "..."}]}`. A refusal or failure adds `"isError": true`.
Edits work the same way: `rename_symbol` writes when the change type-checks, and with `apply=false` it returns a preview with the diff and the type-check result instead.

## Errors and empty answers (isError)

Every tool result carries `isError`. It is `true` when the call was refused or failed: a bad or unknown argument, an unknown preview or edit id, a gopls failure, or a write that was not made (new errors over `max_new_errors`, a check the gate cannot make, a write over the file limit, or a read-only server). It is `false` for a valid query that finds nothing (no symbol matches, `No references found at that position.`, `No diagnostics.`) and for a preview. A read tool reports a query with no match as a result, not an error; a write tool reports the same case as an error, because the change was not made. A refused write still shows its diff in the text.

## Write tools: type check before every write

The edit is applied to in-memory copies first (a gopls overlay), gopls re-checks the edited
files, their package and the packages importing them (including external `_test`
packages), and the response says which diagnostics the edit adds or fixes. Nothing is
written until that is on the table. Output is gofmt-ed; imports are not added or removed (put the import change in the same edit, or run `quick_fix` with `organize_imports=true`, and the refusal says so), except in a file that was not gofmt-clean before the edit: that one keeps its formatting, so the diff shows the edit only (the output says so).

| Tool | Does |
|------|------|
| `edit` | Type-checked text edit of a Go file: `old_string`/`new_string` or a whole `new_text` |
| `edit_symbol` | `replace` / `insert` / `delete` a func, method, type, const or var by name; `imports=[...]` added. Delete refuses while the symbol is used. Replacing a struct type notes every field whose struct tag changed or that lost its tag with the field |
| `rename_symbol` | Rename across the workspace (`parameter=` renames a function's parameter instead). Interface methods are renamed together with every implementer (gopls alone leaves them behind and breaks the interface). Lists remaining mentions in comments, strings and docs (for a method or field only selectors, declarations and literal keys count as code, so a package-level func of the same name is not listed; other packages count only as `pkg.Name`). Lower-case names of one or two letters (and `A`, `I`) are also English words, so in comments and text files they count only as `name`, [name], name( or .name; upper-case ones (`U`, `ID`) are searched like any name. A Go function exported to C with `//export` is refused, with the locations in the directive and the cgo preamble to change by hand. Renaming a field that carries a struct tag notes that the tag values (`json:"user_name"`, ...) were not changed |
| `change_signature` | Add / remove / reorder parameters and rewrite every call site; linked methods follow. Uses as a value are listed as manual work |
| `move` | With `name`: move a top-level func, type, const or var to another file, in the same or another package (qualifiers and imports rewritten; refuses unexported-use problems, import cycles and name clashes). Without `name`: move a package directory (`file_path` -> `to_file`) and rewrite every import |
| `quick_fix` | Apply gopls's quick fixes (plus own fixes for `declared and not used`: mark it used, or remove it) and organize imports; ambiguous fixes are listed instead of guessed; organizing imports also drops the heading comments of the imports it removes. Ends with `Applied:` and `Not fixed:` lines (with reasons); `choice` that fits only some diagnostics applies nothing, and `strict=true` applies nothing unless every targeted diagnostic is fixed |
| `verify_changes` | Which errors the uncommitted changes gained or lost against git `HEAD` (`include_dependents`) |
| `apply_edit` / `undo_edit` | Write a previewed edit by id; revert an applied one |

Every write tool writes when the edit adds no more than `max_new_errors` (default 0; `-1` =
unlimited; other negative values are refused) errors. Otherwise nothing is written and you get the diagnostics plus an id for
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
- `CODENAV_GO_MCP_READ_ONLY=1` turns every write into a refusal that still shows the full diff; `apply=false` previews as usual and `apply=true` is refused (`apply` stays in the schemas so clients that always send it keep working). `apply_edit` and `undo_edit` are not listed.

Positions are 1-indexed; `column` is a UTF-16 offset.

`move` also takes a spec out of a `const`/`var`/`type` block or a `var a, b = ...` line (refused
when that would change `iota` values, or for `var a, b = f()`), and a method to another file of
its own package (a type's methods follow it when it changes package).

Known limits: `move` refuses a symbol that uses cgo (`C.xxx`) because the `import "C"` preamble must stay with it; package moves refuse directories holding non-Go files; code excluded by build
constraints is invisible to the type check (the response says when an edit touches it).

## Limitations

The tools have documented limits: nested modules, case-sensitive names for writes, output caps, build-constrained code and more. See [docs/LIMITATIONS.md](docs/LIMITATIONS.md). Limits that come from gopls or the Go toolchain, and open bugs, are in [docs/KNOWN-ISSUES.md](docs/KNOWN-ISSUES.md).

## Which `gopls` runs

1. `$CODENAV_GO_MCP_GOPLS`
2. `$(go env GOBIN)` / `$(go env GOPATH)/bin`
3. `gopls` on `PATH`
4. `go run golang.org/x/tools/gopls@latest` as a last resort (with a warning)

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `CODENAV_GO_MCP_WORKSPACE` | unset: follows the client's MCP roots when they name a worktree of the same git repository, else `CLAUDE_PROJECT_DIR`, else the working directory | Pins the project root |
| `CODENAV_GO_MCP_READ_ONLY` | unset | `1`, `true`, `yes` or `on` (any case): write tools refuse to write. Any other value, including `0`, `false`, `no` or empty, leaves them writable |
| `CODENAV_GO_MCP_GOPLS` | auto | Path to the gopls binary. If the whole value is an existing executable file it is used as is (so a path with a space works); otherwise it is split on whitespace into a command and arguments |
| `CODENAV_GO_MCP_BUILD_FLAGS` | none | Passed to gopls as `buildFlags` and to `go list` (dependents), e.g. `-tags=integration` |
| `CODENAV_GO_MCP_STATE_DIR` | `<user cache dir>/codenav-go-mcp` | Where the crash-recovery log is kept |

## Development

```bash
go vet ./... && go test ./...
```

Live tests run against `testdata/fixture` and are skipped when gopls is missing.

## License

MIT
