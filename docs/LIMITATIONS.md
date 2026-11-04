# Limitations

What the tools cannot do, or do differently than you might expect. Limits that come from gopls or the Go toolchain are in [KNOWN-ISSUES.md](KNOWN-ISSUES.md).

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
  declares `x`. Renaming a cgo-generated symbol is refused (rename the C symbol); cgo hover shows `C.x`. Hover prints a pkg.go.dev link only for real module paths (none for a `replace`-d directory or a module of the workspace). Modules listed in `go.work` `use` are type-checked by edit previews and handled by `move` like the root module. `change_signature` and `edit_symbol delete` refuse a `//export` func (delete accepts `force=true`); `edit_symbol replace` keeps `import "C"`. Wrongly typed or unknown arguments get a short message with the tool's usage line. An `apply=false` preview warns when its new errors exceed `max_new_errors`. `diagnostics` on a file outside the workspace's modules says so. Round 12: a read-only preview warns "would be refused" when new errors exceed `max_new_errors`; `_` is refused with a message that calls it the blank identifier and says there is nothing to rename; identifiers over 255 characters are refused; responses over 400 KB are truncated with an omitted count; huge integers get a friendly message plus usage; absolute paths in gopls messages are rewritten workspace-relative; the "NOT rewritten" sibling note appears only when a sibling module actually uses the symbol, the nested-module note only on not-found errors it can explain, and the broken-config warning once per result and not on argument mistakes; `edit` with a `package` clause that differs from its directory is refused with one message; package qualifiers differing only by case get a "Did you mean" suggestion; shadowing locals are not listed as unlinked leftovers. Round 13: mixed LF/CRLF files keep the ending of every untouched line (inserted lines follow their neighbour); cgo synthetic callers and go-build locations are filtered from references, callers and implementations, and callers are capped like references; cgo `C.x` hover works and `_Ctype_int` shows as `C.int`; gopls errors are mapped to friendly messages centrally (no package metadata, stdlib lookups with nothing loaded); rename leftovers skip same-named fields, selectors, keyed literals and methods; the sibling-module note appears only when a sibling uses a changed declaration (heuristic: identifier tokens); the package-clause refusal names the clause as written and lists external test files apart; a config problem reads `errors parsing go.mod: go.mod:1: ...`; `quick_fix` on a directory says so; `move` from a broken package shows clean compiler lines; `edit` counts new warnings hidden by errors and notes an import listed twice. Round 14: an integer beyond 2^53 (including `1e400`) gets one message that echoes the number as written; a file outside the workspace shows relative `../` paths at any depth (never a mix with absolute ones) and the "outside the workspace" messages never print the absolute root; `edit` of an explicit `file_path` outside the workspace says so; a workspace without `go.mod` gets no made-up pkg.go.dev link; paths with a NUL byte or an empty required `file_path` are refused as such; hover maps cgo's markdown-escaped `\_Ctype\_int` to `C.int`; the sibling-module note is decided by the type checker (a sibling must use a changed declaration, not merely a same-named one); the rename leftover scan asks the type checker about each remaining identifier with the renamed files in place; malformed JSON lines on stdin (a truncated message, a bare `Infinity`) get a `-32700` parse error with id `null` and the server keeps serving. Vet-style warnings (printf and the other gopls analyzers) are part of `diagnostics` and the edit before/after comparison, but gopls runs analyzers only on packages that type-check: while a package has errors its warnings are not reported (the edit output counts the new ones it hid), so a `Printf` mistake shows up once the package compiles.
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
  the type check cannot see it. `search_symbol` takes its count and its listed matches from the index's
  exact and prefix hits, ordered by rank, path, line and column, so they are never missing. gopls's fuzzy
  matches come after them: gopls returns at most 100 matches per query, so when it hits that cap those
  fuzzy matches are a labelled sample and are not counted. The output is still limited to 50 lines. The
  index stops at 30000 `.go` files; when it does, the answer says so and its counts are lower bounds.
  A `path` is a workspace-relative directory or file prefix (`internal/` matches `internal/...`, not
  `plugins/`), or a glob: a glob with no `/` matches base names (`*_test.go`), `**/` crosses directories.
  An absolute path inside the workspace is accepted; one outside is refused as an input error.
  `query` and `name` are aliases and must not differ. Local variables and parameters are not indexed. A declaration from an
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
  tens of KB); the end is cut with a `(truncated, N more bytes)` note. Hover text is gopls' markdown rendering of the doc
  comment, so `<`, `*` and `[` can show backslash-escaped and a link target can appear nested in brackets.
- **Stdlib interfaces in `implementations`:** a concrete type is matched against the standard-library interfaces only in
  packages gopls has loaded for the workspace, so an import added elsewhere (for example `fmt` for `fmt.Stringer`) can change the list.
- **Struct tags:** renames change Go identifiers only. `rename_symbol` on a field that has a struct tag, and
  `edit_symbol` `replace` of a struct type whose tags changed (or whose tagged field disappeared), say so in a
  note; there is no note for fields without tags, and no guessing about reflection or `encoding/json` use.
  `change_signature` and `move` have no tag-specific behaviour.
