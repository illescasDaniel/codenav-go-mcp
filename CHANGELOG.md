# Changelog

## 1.1.0

- Add `batch` to run up to 25 navigation and edit calls sequentially in one MCP request,
  with per-call results, configurable error handling, and the existing write safeguards.

## 1.0.1

- Documentation: the command now has a package comment, shown on pkg.go.dev.

## 1.0.0

First release. An MCP server for Go, backed by gopls.

- Navigation through the type checker: definitions, references, implementations, callers, outlines,
  diagnostics, symbol search and workspace info.
- Write tools that type-check before writing, with previews, holds, a 40-file limit, `apply_edit` to override
  a hold, and undo: rename, change signature, move (functions, types, packages) and symbol edits.
- A hung gopls is restarted, and read tools retry once after a restart.
- Refusals and failures set `isError`; a valid empty answer does not.
- Known limits are listed in `docs/KNOWN-ISSUES.md`.
