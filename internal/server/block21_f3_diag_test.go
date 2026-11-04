package server

import (
	"strings"
	"testing"
)

// BUG-LEAK-003 at the tool level, against gopls: a required module whose replaced go.mod does not parse is named as
// the cause in the diagnostics answer, relative to the workspace, and the generic "no required module" wording is gone.
func TestBlock21F3DiagnosticsNamesBrokenReplacedGoMod(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "go.mod", readFile(t, root, "go.mod")+
		"\nrequire example.com/b21dep v0.0.0\n\nreplace example.com/b21dep => ./b21dep\n")
	writeFile(t, root, "b21dep/go.mod", "module example.com/b21dep\n\ngo 1.21\n\nrequire (\n\tgolang.org/x/nothere v1.0.0\n")
	writeFile(t, root, "b21dep/dep.go", "package b21dep\n\nfunc Dep() int { return 1 }\n")
	writeFile(t, root, "b21use/use.go", "package b21use\n\nimport \"example.com/b21dep\"\n\nfunc Use() int { return b21dep.Dep() }\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "b21use/use.go"})
	if strings.Contains(out, "no required module provides package") {
		t.Fatalf("the generic wording stayed:\n%s", out)
	}
	mustHave(t, "diagnostics", out, "could not import example.com/b21dep (parsing b21dep/go.mod")
	if strings.Contains(out, root) {
		t.Errorf("the answer shows an absolute path of the workspace:\n%s", out)
	}
}
