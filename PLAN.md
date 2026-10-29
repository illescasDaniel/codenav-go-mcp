# codenav-go-mcp: plan

An MCP server for **Go code**, written in Go and backed by **gopls**. It does for Go
what `codenav-mcp` does for Python with ty: name-based, type-resolved navigation, plus
write tools that type-check an edit before writing it and can preview and undo it.

It is **one Go module and one binary**. There is no separate shared package. The
language-agnostic code that lives in `mcp-nav-shared` for the Python servers becomes
`internal/` packages here. `webnav-ts-mcp`'s `src/shared/` is the precedent for this.

## Why not just `gopls mcp`?

gopls v0.24 ships `gopls mcp`, with tools such as workspace, search, file context,
package API, references and diagnostics. This server differs in four ways:

- **The same tool surface as codenav-mcp/webnav.** It uses the same tool names and
  parameter aliases (`name`/`query`), and dotted names such as `Type.Method`. An agent
  that knows one codenav server knows this one.
- **Write tools with a type-check gate.** Every edit runs in-memory first (overlay).
  The server diffs the diagnostics of the edited packages and their importers, then
  writes atomically, keeps the edit undoable, and refuses stale files. `gopls mcp`
  has nothing like this.
- **Fixes gopls's refactoring gaps.** The probe below found that renaming an
  interface method does not rename its implementers.
- **Worktree-aware workspace selection.** It follows the MCP client's roots when they
  name another worktree of the same repository.

## Verified gopls v0.24.0 behaviour

These were probed against a fixture module (interface, two implementers, one of them
through embedding, an external `_test` package):

| Question | Finding | Consequence |
|---|---|---|
| Pull diagnostics? | `diagnosticProvider` is **not advertised**, but `textDocument/diagnostic` **works**, including for files that were never opened | Use pull first and fall back to push with a timeout, as `lsp_client` already does |
| Importers' diagnostics after an overlay change? | Pushed for **unopened** importers about 1s after `didChange` | `simulate` does not have to `didOpen` every dependent; pulling them is enough |
| New files as overlay only? | Yes, both in an existing package dir **and in a new package dir**, and importable from other overlays | Drop the Python side's "write the new file to disk during the check" |
| `positionEncoding` | Not returned, so UTF-16 is used | Keep UTF-16 columns in the tool contract, for parity |
| Rename of an interface method | Renames the interface method and its call sites **only**. `Square.Area` and the promoted `base.Area` are left behind, which silently breaks `var _ Shape = Square{}` | A Go version of `linked.py` is needed: widen the rename through `textDocument/implementation`, including promoted methods of embedded types |
| `textDocument/implementation` on an interface | Returns concrete types, including the ones that satisfy it through embedding | `implementations` is a thin wrapper. The Protocol probe file and its cache are gone |
| Code actions on a param / func | `refactor.rewrite.removeUnusedParam`, `moveParamLeft/Right`, `refactor.extract.toNewFile`, `implementInterface`, `fillStruct`, `inline.call`, `source.organizeImports`, `quickfix` | `change_signature` can use gopls for removing and reordering parameters. Adding a parameter is our own go/ast rewrite |
| `refactor.move.moveDeclaration` / `moveType` | Advertised, but the CLI never offered them | Investigate in phase 4 (they probably need an interactive destination). Do not count on them |

`gopls` is found in this order: `$CODENAV_GO_MCP_GOPLS`, `$(go env GOBIN)` /
`$(go env GOPATH)/bin`, `PATH`, and then `go run golang.org/x/tools/gopls@latest` as a
last resort, with a warning. This mirrors `ty_command.py`.

## Layout

```
cmd/codenav-go-mcp/main.go      flags (--version/--help), stdio serve
internal/
  # language-agnostic: the former mcp-nav-shared. These packages must NOT import
  # go/ast or anything gopls-specific. That rule keeps the boundary in one module.
  lsp/          JSON-RPC framing, request/notify, server->client replies, retry on
                ContentModified, document sync + mtime refresh, didChangeWatchedFiles,
                config-change restart, Overlay(), scratch docs, pull/push diagnostics,
                stderr tail on exit. Minimal hand-written LSP structs + json.RawMessage
  workspace/    WorkspaceSelector (env pin > client roots in the same git repo >
                CLAUDE_PROJECT_DIR > cwd), git common-dir check
  format/       path:line:col headers, snippets, grouped references, outline, ranked
                and filtered workspace symbols, callers, diagnostics, UTF-16 helpers
  resolve/      name -> symbol (dotted Type.Method, file filter, ambiguity listing)
  edits/        TextEdit / WorkspaceEdit (UTF-16, CRLF, BOM), FileChange, EditPlan, unified diff
  txn/          WriteGuard, EditJournal (stale check, atomic apply + rollback, undo), PlanStore
  diagdelta/    before/after diagnostics keyed on message + source line
  notices/      NoticeBoard (restarts, stale binary), appended to tool results
  params/       name/query alias resolution with hint text
  toolerr/      ToolInputError + formatting of LSP/timeout/missing-file errors
  exclude/      skipped dirs (.git, vendor, node_modules, testdata?, ...)
  # Go-specific
  gosrc/        go/parser + go/ast + go/scanner helpers: decl spans incl. doc comments,
                Type.Method lookup, syntax check, gofmt + goimports, import add/remove,
                generated-file detection
  gomod/        go.mod / go.work discovery, dir <-> import path, `go list -json` for
                dependents (incl. external _test packages), import-cycle check
  server/       MCP wiring (go-sdk), workspace switching, gopls client lifecycle
  tools/        handlers: nav.go, edit.go, symbol.go, rename.go, signature.go, move.go,
                apply.go, verify.go
testdata/fixture/  small module used by unit and live tests
```

Dependencies: `github.com/modelcontextprotocol/go-sdk` (v1.8.0 checked: typed
`mcp.AddTool`, `ToolAnnotations`, `ServerSession.ListRoots`; roots are deprecated in the
2026-07-28 protocol but still work, the same as on the Python side), plus
`golang.org/x/tools` for `go/ast/astutil` and `imports`. That's all.

## Python module -> Go mapping

| codenav-mcp / mcp-nav-shared | Go |
|---|---|
| `lsp_client.py` | `internal/lsp` (goroutine reader, `map[id]chan`, mutexes instead of asyncio locks) |
| `workspace.py`, `notices.py`, `params.py`, `errors.py`, `exclude.py` | the matching `internal/` packages, ported directly |
| `format.py`, `resolve.py` | `internal/format`, `internal/resolve`. Symbol kinds and the ranking are unchanged |
| `edits.py`, `transaction.py`, `diagnostics_delta.py` | `internal/edits`, `internal/txn`, `internal/diagdelta` |
| `server.py` nav tools | `tools/nav.go` |
| `implementations` + probe + `_class_infos` cache | **dropped**: `textDocument/implementation`. Results in `_test.go` files get their own heading (replaces `EXTRA_SOURCE_ROOTS`) |
| `ty_command.py` | `server/gopls.go` (lookup order above) |
| `pysource.py`, `imports.py` | `gosrc` (go/ast, astutil.AddImport/DeleteImport) |
| `tidy.py` (ruff, only where already sorted) | `imports.Process` + `format.Source` on **every** written file, because gofmt is canonical |
| `deps.py` | `gomod` via `go list -deps -test -json`: reverse deps, including `package foo_test` |
| `simulate.py` | `tools/simulate.go`: `Overlay()` the plan, pull diagnostics for the edited files and their reverse-dep packages, diff |
| `rename.py` + `linked.py` | gopls rename, widened by `implementation` on interface methods (both directions), including promoted methods through embedding. `mentions.py` becomes go/scanner comments and strings plus text files |
| `signature.py` | gopls `removeUnusedParam` / `moveParam*` where they fit. Otherwise our own: go/ast rewrite of the decl, plus call sites from `references` |
| `moves.py` | `move`: same-package file move (easy), then a cross-package symbol move, then a package-dir move (import-path rewrite) |
| `symbol_tools.py` (`quick_fix`) | gopls `quickfix` + `source.organizeImports`. Ambiguous fixes are listed, not guessed |
| `SOURCE_ROOT` / `EXTRA_SOURCE_ROOTS` | **dropped**: go.mod / go.work define the packages |

## Tool surface (names and parameters kept identical to codenav-mcp)

Navigation: `workspace`, `symbol_info`, `outline`, `callers`, `implementations` (also
works in reverse from a concrete type: "which interfaces does it satisfy"),
`search_symbol` (`kind`, `path`, `fuzzy`; gopls `symbolScope=workspace` keeps out the
stdlib and dependencies), `hover`, `definition`, `references`, `diagnostics`.

Write: `edit`, `edit_symbol` (replace/insert/delete a func, method, type, const or var
by name; `imports=[...]`), `rename_symbol`, `change_signature`, `move`, `quick_fix`,
`verify_changes`, `apply_edit`, `undo_edit`. They share the same rule: write when the
edit adds no more than `max_new_errors` (default 0) errors; `apply=false` always
previews; edits touching more than 40 files need `allow_large=true`.

Dotted names: `Type.Method`, `Type.Field`, and `pkg.Name` to choose a package.

Environment: `CODENAV_GO_MCP_WORKSPACE`, `CODENAV_GO_MCP_READ_ONLY`,
`CODENAV_GO_MCP_GOPLS`, and optionally `CODENAV_GO_MCP_BUILD_FLAGS`, which is passed to
gopls as `buildFlags` (for example `-tags=integration`).

## Go-specific rules

- **The write guard refuses** files outside the workspace, `vendor/`, GOROOT,
  GOMODCACHE, non-`.go` files (go.mod changes go through the tools, not raw edits),
  symlinks that leave the workspace, and files starting with
  `// Code generated ... DO NOT EDIT.`
- **Build constraints:** gopls only type-checks the current GOOS/GOARCH/tags. When an
  edit touches files excluded from the build, the result says so, because the
  diagnostics delta cannot see them.
- **Dependents** include external test packages (`package foo_test`) and in-package
  `_test.go` files.
- **Cross-package moves** refuse when the moved code refers to unexported identifiers
  of the source package (or is referred to unexported), or when the move would create an
  import cycle (a compile error in Go, not just a warning). Package qualifiers are
  rewritten (`shapes.Square` -> `geom.Square`), and an import is added or dropped per file.
- **Rename** checks the new name with `token.IsIdentifier`, and checks for an exported/
  unexported flip that would break callers outside the package; those errors show up in
  the diagnostics delta.
- **Output** is always gofmt-ed. Code that does not parse is never written.

## Phases

1. **Core plus the overlay spike.** Scaffold the module, `internal/lsp`, workspace,
   format, resolve, notices, and the nav tools. In the same phase, build the
   **`Overlay` + pull-diagnostics spike** behind a test: change a signature in memory and
   see the importer's new error, including a new-file overlay. The write half rests on
   this, so it is proven first, not discovered late.
2. **Write foundation:** edits, txn, diagdelta, simulate, `edit`, `apply_edit`,
   `undo_edit`, `verify_changes` (git `HEAD` versions shown as overlays).
3. **Symbol tools:** `edit_symbol`, `rename_symbol` (+ the implementer widening + mentions),
   `quick_fix`.
4. **Refactors:** `change_signature`, then `move` (same package -> cross-package -> package dir).
5. **Release:** README in the codenav style, `go install ...@latest`, GitHub release
   binaries (goreleaser), and an `.mcp.json` example using `go run ...@vX`.

Tests: unit tests on pure packages; live tests against `testdata/fixture`, skipped when
gopls is missing; MCP-level tests over the go-sdk in-memory transport. Port the
codenav-mcp test cases where they translate. For example, `test_write_edge_live` maps to
CRLF/BOM, stale files and rollback.

## Open questions

- Is a `refactor.move.*` action usable non-interactively (phase 4)?
- Pull diagnostics are not advertised: keep the push fallback, and add a test that fails
  if a gopls upgrade drops pull.
- Should `verify_changes` also run `go vet` (analyzers are already part of gopls
  diagnostics, so probably not)?
