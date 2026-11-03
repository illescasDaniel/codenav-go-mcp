package server

import (
	"strings"
	"testing"
)

// BUG-NAVP-007: a path with a trailing space is a missing file, not "not a .go file".
func TestBlock20NMissingPathWithTrailingSpaceSaysNotFound(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "pn/pn.go", "package pn\n\nfunc Name() int { return 1 }\n")
	out, isErr := callIsError(t, cs, "hover", map[string]any{"file_path": "pn/pn.go ", "line": 3, "column": 6})
	if !isErr || !strings.Contains(out, "File not found: pn/pn.go") || strings.Contains(out, "not a .go file") {
		t.Errorf("hover on a path with a trailing space:\n%s (isError=%v)", out, isErr)
	}
}

// BUG-NAVN-008 and BUG-NAVP-004: outline, hover and definition of a file that does not parse name the syntax
// error and its position, so a valid identifier does not read as "nothing here".
func TestBlock20NSyntaxErrorInTheFileItselfIsNamed(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "bk/broken.go", "package bk\n\nfunc Good() int { return 1 }\n\nfunc Broken( {\n\treturn\n}\n")
	const note = "bk/broken.go:5:14: syntax error"
	if out := call(t, cs, "outline", map[string]any{"file_path": "bk/broken.go"}); !strings.Contains(out, note) {
		t.Errorf("outline of a file that does not parse:\n%s", out)
	}
	if out := call(t, cs, "hover", map[string]any{"file_path": "bk/broken.go", "line": 3, "column": 6}); !strings.Contains(out, note) {
		t.Errorf("hover on a valid identifier in a file that does not parse:\n%s", out)
	}
	if out := call(t, cs, "definition", map[string]any{"file_path": "bk/broken.go", "line": 3, "column": 6}); !strings.Contains(out, note) {
		t.Errorf("definition on a valid identifier in a file that does not parse:\n%s", out)
	}
}

// BUG-DIAG-007: a file under a broken go.mod is not blamed on build constraints; the answer names the configuration.
func TestBlock20NBrokenGoModIsNamedNotBuildConstraints(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "go.mod", "module example.com/n1\n\ngo 1.21\n\nrequire example.com/bogus v9.9.9-nope\n")
	writeFile(t, root, "g/g.go", "package g\n\nfunc G() int { return 1 }\n")
	out, _ := callIsError(t, cs, "diagnostics", map[string]any{"file_path": "g/g.go"})
	if !strings.Contains(out, "the Go configuration is broken") || strings.Contains(out, "build constraints") {
		t.Errorf("diagnostics under a broken go.mod:\n%s", out)
	}
}
