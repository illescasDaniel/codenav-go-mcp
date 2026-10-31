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
| `search_symbol` | Workspace symbol search (ranked; `kind=`, `path=`, `fuzzy=` filters). Exact and prefix matches come from an own declaration index, so they are never capped or missed; gopls adds the fuzzy ones |
| `workspace` | Which directory is being navigated, and why |
| `hover`, `definition`, `references` | Position tools: `file_path`, `line`, `column` (1-indexed). `diagnostics` takes `file_path` only |

Names may be dotted: `Type.Method`, `Type.Field`, `pkg.Name` (`pkg` is the package name or its directory's name). `name` and `query` are
aliases. A missing required parameter (of any tool; an explicit JSON `null` counts as missing, and as not given for an optional one) gets `Missing required parameter(s): x, y.` plus the tool's usage, not a raw schema error.

## Write tools: type check before every write

The edit is applied to in-memory copies first (a gopls overlay), gopls re-checks the edited
files, their package and the packages importing them (including external `_test`
packages), and the response says which diagnostics the edit adds or fixes. Nothing is
written until that is on the table. Output is gofmt-ed with imports fixed, except in a file that was not gofmt-clean before the edit: that one keeps its formatting, so the diff shows the edit only (the output says so).

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

- **Nested modules:** gopls loads only the modules that the root `go.mod` or `go.work` covers. A directory
  with its own `go.mod` outside that is added to gopls as an extra workspace folder, so its symbols are found
  by name and its own packages are type-checked. Importers in *other* uncovered modules (a `replace`-d
  sibling directory, with no `go.work`) are type-checked too, so breakage shows as a new error and
  `max_new_errors` refuses the apply, but those callers are **not rewritten**: fix them by hand, or join the
  modules with `go work init ./a ./b` so the edit rewrites them. A `move` out of an uncovered module is
  refused with that explanation.
- **Case-sensitive names:** write tools (`rename_symbol`, `edit_symbol`, `change_signature`, `move`) never
  guess the case: `gen` does not resolve to `Gen` or to a package directory called `gen`; the error says
  "Did you mean `Gen`?". Read tools still match loosely but print a note when only the case differed.
- **Broken configuration:** when `go list -m` fails (a syntax error in `go.mod`, a `go.work` `use` of a
  missing directory) every tool output ends with a warning naming the problem instead of silently
  returning empty results.
- **organize_imports** also removes the comments that belonged to a removed import (its doc and trailing
  comment, and a heading whose imports all went); comments of kept imports are untouched.
- **File paths** given to the read tools must lie in the workspace, GOROOT, the module cache or a
  `replace`/`use`-d directory; anything else (`../other-project/main.go`) is refused. `definition` on an
  import path lists at most 10 files (`(+K more files)`); on `C.x` it points at the cgo preamble line that
  declares `x`. Renaming a cgo-generated symbol is refused (rename the C symbol); cgo hover shows `C.x`. Hover prints a pkg.go.dev link only for real module paths (none for a `replace`-d directory or a module of the workspace). Modules listed in `go.work` `use` are type-checked by edit previews and handled by `move` like the root module. `change_signature` and `edit_symbol delete` refuse a `//export` func (delete accepts `force=true`); `edit_symbol replace` keeps `import "C"`. Wrongly typed or unknown arguments get a short message with the tool's usage line. An `apply=false` preview warns when its new errors exceed `max_new_errors`. `diagnostics` on a file outside the workspace's modules says so. Round 12: a read-only preview warns "would be refused" when new errors exceed `max_new_errors`; `_` is "blank identifier, nothing to rename"; identifiers over 255 characters are refused; responses over 400 KB are truncated with an omitted count; huge integers get a friendly message plus usage; absolute paths in gopls messages are rewritten workspace-relative; the "NOT rewritten" sibling note appears only when a sibling module actually uses the symbol, the nested-module note only on not-found errors it can explain, and the broken-config warning once per result and not on argument mistakes; `edit` with a `package` clause that differs from its directory is refused with one message; package qualifiers differing only by case get a "Did you mean" suggestion; shadowing locals are not listed as unlinked leftovers.
- **Name lookups outside the workspace** work for the read-only tools (`symbol_info`, `callers`,
  `implementations`): `json.Marshal`, `encoding/json.Decoder.Decode` or a symbol of a required dependency
  resolve through a scratch file that exists only as a gopls overlay (never on disk). A short qualifier is
  matched against the imports seen in the workspace and the standard library; a full import path always
  works. A package no module provides is reported as such. Write tools refuse these symbols.
- **Symbol index:** next to gopls the server keeps its own index of declarations (funcs, methods, types,
  struct fields, interface methods, vars, consts) built with `go/parser` from every `.go` file below the
  workspace root, skipping `.git`, `vendor`, `node_modules`, `testdata`, hidden and `_` directories, and the files the go tool never compiles: names starting with `_` or `.` and `//go:build ignore` files (a tag-excluded file such as `!linux` stays indexed; symbols in the build cache, cgo's `_Cfunc_*`/`_cgo_*`, are filtered out of `search_symbol`). It is
  refreshed on every call (only files whose mtime or size changed are re-parsed) and does not depend on
  the build configuration. Name lookups merge it with gopls: gopls supplies kind and position where it has
  the symbol, and the index adds declarations gopls misses (files behind build tags, other GOOS/GOARCH
  files, packages that are not built, a directory created a moment ago). A name declared in several files
  is therefore reported as ambiguous, each file with its build constraint (`build: //go:build !linux`),
  and `file_path` picks one; a declaration only in a constrained file is used as is, and write tools say
  the type check cannot see it. `search_symbol` adds the index's exact and prefix matches to gopls's
  (capped, fuzzy) answer, so those are never missing; the fuzzy tail is still gopls's, and the output is
  still limited to 50 lines. Local variables and parameters are not indexed. A declaration from an
  unsaved or non-`.go` source is not seen.
- **New directories:** files written outside the tools are picked up on the next call. A brand-new
  package directory is loaded by opening one of its files, so the first call after creating it can
  take a moment longer.
- **Concurrent writers:** applying and undoing take an advisory file lock (`<state dir>/<hash of workspace>/lock`),
  so two server processes on one workspace take turns, and every file is re-hashed right before it is written;
  a file that changed in between fails the write clearly. On OSes without `flock` support the lock is skipped
  with a note.
- **Crash safety:** before a multi-file write the server records the plan, base hashes and the original
  files in a log under the user cache directory (`<cache>/codenav-go-mcp/<hash of workspace>/`, never in
  the tree) and removes it when the write finishes or is rolled back. If the process dies mid-write, the
  next start posts a notice naming the files that may be half-written and the `cp`/`rm` commands that
  restore them (the leftover is kept in a `recovered-*` folder there). If the log cannot be written, the
  edit still goes ahead with a note. Undo history lives in memory only: switching workspaces drops it, and
  says so.
- **Moved or deleted files in the type-check:** a deleted file is replaced by its bare `package` clause in
  the in-memory "after" state (by a `//go:build ignore` stub when the whole directory goes), so importers
  the rewrite missed show up as errors. Only importers gopls loads under the current GOOS/GOARCH and
  `CODENAV_GO_MCP_BUILD_FLAGS` are seen.
- **Unused local variables:** gopls offers no fix for `declared and not used`, so `quick_fix` adds two of its
  own, and with both on offer it lists them and waits for `choice=`:
  "Mark x as used (`_ = x`)" adds the line after the declaring statement (or at the start of each body or
  clause for `if`/`for`/`switch` initializers, range variables and type-switch bindings), and
  "Remove unused variable x" deletes the declaration while keeping what its initializer does: a statement
  with no side effects (identifiers, literals, selectors, composite literals, conversions) goes; `x := f()`
  becomes `f()` (`_ = expr` when the call is part of a larger expression); `a, x := f()` becomes `a, _ := f()`
  (`=` when no new variable would be left); members of a `var (...)` group leave it; initializers, range
  variables, type-switch bindings and select receives are dropped or blanked. Parameters, results and
  labelled statements get no fix. Removing a variable can leave the variables it was computed from unused;
  that shows up as a new error, so nothing is written, and a second run removes them. A conversion to a
  user-defined type that is the whole right-hand side is treated as a call (`MyInt(f())`), which the type
  check rejects rather than writes.
- **Formatting of messy files:** a write tool gofmt-s the files it changes only when they were gofmt-clean before.
  A file that was not is written with the edit alone (it must still parse), and the output names it; there is
  no partial, region-only gofmt. Run `gofmt -w` once if you want the whole file formatted.
- **Output caps:** `hover` and `symbol_info` print at most about 4 KB of hover text (a package's documentation can be
  tens of KB); the end is cut with a `(truncated, N more bytes)` note.
- **Struct tags:** renames change Go identifiers only. `rename_symbol` on a field that has a struct tag, and
  `edit_symbol` `replace` of a struct type whose tags changed (or whose tagged field disappeared), say so in a
  note; there is no note for fields without tags, and no guessing about reflection or `encoding/json` use.
  `change_signature` and `move` have no tag-specific behaviour.

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
| `CODENAV_GO_MCP_BUILD_FLAGS` | none | Passed to gopls as `buildFlags` and to `go list` (dependents), e.g. `-tags=integration` |
| `CODENAV_GO_MCP_STATE_DIR` | `<user cache dir>/codenav-go-mcp` | Where the crash-recovery log is kept |

## Development

```bash
go vet ./... && go test ./...
```

Live tests run against `testdata/fixture` and are skipped when gopls is missing.
The design and the gopls findings behind it are in [PLAN.md](PLAN.md).

## License

MIT
