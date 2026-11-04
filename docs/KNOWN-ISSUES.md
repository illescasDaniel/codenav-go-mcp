# Known issues

This page lists what codenav-go-mcp does not do, or does less well than its answers suggest, and what to do
about it.

## References on dependency symbols

`references` on a symbol declared in a module-cache dependency (for example `assert.Equal` from testify) should
list the workspace call sites too. A batch 20 session reported that it sometimes listed only the dependency's
own call sites. Block 21 could not reproduce that on gopls v0.24.0:

| Query | Result |
|---|---|
| `gopls references` (CLI) on the call site `use.go:11:9` | lists `use.go:11:9` and the dependency's call sites |
| `gopls references` (CLI) on the dependency declaration | lists `use.go:11:9` as well |
| `Client.References` on the declaration, server started as the session does (`symbolScope: workspace`) | lists `use.go:11:9` |
| same, without `symbolScope` | same |
| same, after waiting 5 s | same |
| same, after `definition` on the declaration first | same |
| same, after `references` on the call site first | same |
| same, after opening only the call-site file | same |
| `references` through the MCP server on the call site and on the declaration | both show `use.go:11:9` |
| same bed nested under a parent module directory | same |

The bug is not reproduced in the variants above, and no mitigation was added. Variants not tried yet:

- `go.work` and multi-module workspaces;
- `replace` directives and vendored dependencies;
- a call-site file with build tags or type errors;
- an unsaved edit (overlay) on the call-site file;
- other gopls versions;
- the references tool's own filtering and exclude code. Commit 368a9fe changed its all-dropped logic, so check
  that path first.

If a list of dependency call sites has no workspace entry, run `references` on a call site in your own file
(for example `assert.Equal(` in your test) for the complete list.

## A hung gopls

gopls is restarted when it stops answering. A request that times out (after 20 s) while gopls has sent nothing
at all since the request was sent ends the server at once. A server that keeps sending messages (it is busy or
loading packages) is not ended on the first timeout: it is ended at the third stall in a row. Requests that time
out together count as one stall, and any answer from the server resets the count. Each restart posts one notice,
"gopls did not answer for 20 s and was restarted". The call that timed out gets the message "the language server
stopped answering and was restarted; retry", and the next call starts a new server. Read tools retry the request once on their own after such a restart
(never a write), so a read normally still answers.

A single legitimate request that stays silent for more than 20 s (for example a heavy references or type-check
run that sends no progress messages) is treated as hung and ended.

A busy server that answers late still returns "did not answer in time" on each of its first two stalls. It is
ended only when three stalls in a row pass without any answer.

## symbol_info on a name declared thousands of times

A name declared in thousands of packages (measured: 31000 packages, each declaring `func Shared()`) is answered
with the first 50 candidates, the total, and a note on how to narrow the list. Block 21 measured the time per
phase on that bed:

- gopls `workspace/symbol` for the name: 5 to 13 s on the first call (it depends on gopls's own load and
  caches); this part belongs to gopls and is not changed here. The lookup asks gopls again for `Name$` when the
  first answer is capped, so a cold call pays for two such requests.
- merging the index's declarations with gopls's answer compared every declaration with every symbol so far:
  about 30 s per call. That is fixed: the merge now uses a map by file and line.
- index refresh (120000 entries): about 0.5 s; formatting and labelling of the candidates: capped at 50.

After the fix a warm call takes about 0.3 to 1 s, plus gopls's own time. The answer lists 50 candidates and the
total, so it stays about 3.5 KB.

## Other limits

- `move` there and back is not byte-identical (DET-002): the declaration returns at the end of the file, and the
  emptied file stays, with a note. Removing the file is pinned by an existing test, so this is kept.
- `hover` keeps markdown escapes in doc comments (LEAK-008): backslashes can show in the text.
- `implementations` of a concrete type lists standard-library interfaces only from packages the workspace
  already loads (NAVN-003; a gopls scope limit).
- `outline` omits `func _()` declarations (NAVN-009).
- cgo files under CgoEnabled=false are not reported as excluded.
- Import cycles that come only from build tags or from test-only imports are not caught before gopls reports
  them.
- An `edit` that breaks a `#include` in a cgo preamble passes silently: overlay edits do not run cgo.
- `verify_changes` type-checks at most 60 files; the rest are reported as not checked.
- A Go package in a nested directory under `build/`, `dist/` or `venv/` that has no `.go` file directly in that
  directory is treated as build output and excluded from write tools (SHAPE-001). A test pins this shape.
- A `go.mod` created below the root inside the 30 s nested-scan window is not seen until the cache expires.
- `search_symbol` and the name-resolution errors in symbol lookup are not routed through the "no views" mapping.
- `readableRoots` runs `go env` without a timeout; a timed-out `go env GOROOT` is remembered as unknown for the
  process lifetime.
- The "missing go.sum" hint uses the import path in `go get`, which may not be the module that provides it.
- A request batch holds all its ids in flight until the whole answer array is written.
- A repeated id inside one batch is refused with "duplicate request id".
- Edit previews still describe a broken import with the generic "no required module" wording (`diagdelta`).

Last reviewed: block 21
