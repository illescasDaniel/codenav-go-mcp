// Package lsp is a minimal LSP client: JSON-RPC framing plus the handful of
// requests the navigation tools need. It knows nothing about Go or gopls.
package lsp

import "encoding/json"

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type Diagnostic struct {
	Range    Range           `json:"range"`
	Severity int             `json:"severity,omitempty"`
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

// Symbol covers both DocumentSymbol (hierarchical: Range, SelectionRange,
// Children) and SymbolInformation / WorkspaceSymbol (Location, ContainerName).
type Symbol struct {
	Name           string    `json:"name"`
	Detail         string    `json:"detail,omitempty"`
	Kind           int       `json:"kind"`
	Range          *Range    `json:"range,omitempty"`
	SelectionRange *Range    `json:"selectionRange,omitempty"`
	Children       []*Symbol `json:"children,omitempty"`
	Location       *Location `json:"location,omitempty"`
	ContainerName  string    `json:"containerName,omitempty"`
}

type CallHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail,omitempty"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

type IncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

type TextDocumentEdit struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Edits []TextEdit `json:"edits"`
}

// WorkspaceEdit keeps only the text edits; resource operations are left raw.
type WorkspaceEdit struct {
	Changes         map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges []json.RawMessage     `json:"documentChanges,omitempty"`
}

// SymbolKind labels, indexed by LSP SymbolKind value.
var SymbolKindNames = map[int]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "type", 6: "method",
	7: "property", 8: "field", 9: "constructor", 10: "enum", 11: "interface",
	12: "function", 13: "variable", 14: "constant", 15: "string", 16: "number",
	17: "boolean", 18: "array", 19: "object", 20: "key", 21: "null",
	22: "enum member", 23: "struct", 24: "event", 25: "operator", 26: "type parameter",
}
