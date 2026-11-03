package server

import (
	"strings"
	"testing"
)

// BUG-NAVN-011: implementations names a nested module that the workspace does not cover and that mentions the name.
func TestBlock20NImplementationsNotesNestedModule(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "sub/go.mod", "module example.com/n2sub\n\ngo 1.21\n")
	writeFile(t, root, "sub/impl.go", "package sub\n\nvar _ = Shape(nil)\n")
	out := call(t, cs, "implementations", map[string]any{"name": "Shape"})
	if !strings.Contains(out, "nested module example.com/n2sub (sub/go.mod)") {
		t.Errorf("implementations gives no nested-module note:\n%s", out)
	}
}

// BUG-NAVN-013: callers of an interface method says that calls through concrete implementations are included; the
// same statement is not made for a concrete method.
func TestBlock20NCallersOfInterfaceMethodSaysConcreteIncluded(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "cn/sink.go", "package cn\n\ntype Sink interface{ Write(b []byte) }\n\ntype FileSink struct{}\n\n"+
		"func (FileSink) Write(b []byte) {}\n\nfunc send(s Sink) { s.Write(nil) }\n\nfunc viaConcrete() { FileSink{}.Write(nil) }\n")
	const note = "calls through in-workspace concrete implementations are included"
	if out := call(t, cs, "callers", map[string]any{"name": "Sink.Write"}); !strings.Contains(out, note) {
		t.Errorf("callers of an interface method lacks the concrete-calls note:\n%s", out)
	}
	if out := call(t, cs, "callers", map[string]any{"name": "FileSink.Write"}); strings.Contains(out, note) {
		t.Errorf("callers of a concrete method has the interface note:\n%s", out)
	}
}

// Test (b) of batch N1 through the session: an existing README.md passed to hover is refused as not a .go file.
func TestBlock20NReadmePassedToHoverIsNotGo(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "README.md", "# readme\n")
	out, isErr := callIsError(t, cs, "hover", map[string]any{"file_path": "README.md", "line": 1, "column": 1})
	if !isErr || !strings.Contains(out, "not a .go file") {
		t.Errorf("hover on an existing README.md:\n%s (isError=%v)", out, isErr)
	}
	out, isErr = callIsError(t, cs, "outline", map[string]any{"file_path": "README.md"})
	if !isErr || !strings.Contains(out, "not a .go file") {
		t.Errorf("outline of an existing README.md:\n%s (isError=%v)", out, isErr)
	}
}

// BUG-LEAK-005 and BUG-ENV-007 through the session: workspace names the go.work in use and the gopls in use.
func TestBlock20NWorkspaceToolNamesSettings(t *testing.T) {
	cs, root := session(t, map[string]string{"GOWORK": "off"})
	out := call(t, cs, "workspace", map[string]any{})
	for _, want := range []string{root, "read-only: no", "gopls: ", "GOWORK env: off", "go.work in use: off"} {
		if !strings.Contains(out, want) {
			t.Errorf("workspace lacks %q:\n%s", want, out)
		}
	}
}

// BUG-NAVP-003 through the session: references to a dependency's function counts the dependency locations as not shown, and never
// prints a module-cache path. gopls may omit the workspace's own call site for module-cache symbols (known quirk),
// so the answer must not read as "no workspace uses". The dependency is the offline
// testify module, so the test needs no network.
func TestBlock20NReferencesThroughSessionDropsDependencySites(t *testing.T) {
	cs, root := session(t, map[string]string{"GOPROXY": "off", "GOSUMDB": "off"})
	writeFile(t, root, "go.mod", "module example.com/n2ref\n\ngo 1.21\n\nrequire github.com/stretchr/testify v1.11.1\n")
	writeFile(t, root, "go.sum", n2TestifyGoSum)
	writeFile(t, root, "use.go", "package n2ref\n\nimport \"github.com/stretchr/testify/assert\"\n\n"+
		"func Check(t assert.TestingT) {\n\tassert.Equal(t, 1, 1)\n}\n")
	out := call(t, cs, "references", map[string]any{"file_path": "use.go", "line": 6, "column": 9})
	if !strings.Contains(out, "inside dependencies") || strings.Contains(out, "No references found") {
		t.Errorf("references lacks the all-dependencies answer:\n%s", out)
	}
	if strings.Contains(out, "/pkg/mod/") || strings.Contains(out, "testify@") {
		t.Errorf("references prints a module-cache path:\n%s", out)
	}
}

const n2TestifyGoSum = `github.com/davecgh/go-spew v1.1.1 h1:vj9j/u1bqnvCEfJOwUhtlOARqs3+rkHYY13jYWTU97c=
github.com/davecgh/go-spew v1.1.1/go.mod h1:J7Y8YcW2NihsgmVo/mv3lAwl/skON4iLHjSsI+c5H38=
github.com/pmezard/go-difflib v1.0.0 h1:4DBwDE0NGyQoBHbLQYPwSUPoCMWR5BEzIk/f1lZbAQM=
github.com/pmezard/go-difflib v1.0.0/go.mod h1:iKH77koFhYxTK1pcRnkKkqfTogsbg7gZNVY4sRDYZ/4=
github.com/stretchr/testify v1.11.1 h1:7s2iGBzp5EwR7/aIZr8ao5+dra3wiQyKjjFuvgVKu7U=
github.com/stretchr/testify v1.11.1/go.mod h1:wZwfW3scLgRK+23gO65QZefKpKQRnfz6sD981Nm4B6U=
gopkg.in/check.v1 v0.0.0-20161208181325-20d25e280405/go.mod h1:Co6ibVJAznAaIkqp8huTwlJQCZ016jof/cbN4VW5Yz0=
gopkg.in/yaml.v3 v3.0.1 h1:fxVm/GzAzEWqLHuvctI91KS9hhNmmWOoWu0XTYJS7CA=
gopkg.in/yaml.v3 v3.0.1/go.mod h1:K4uyk7z7BCEPqu6E+C64Yfv1cQ7kz7rIZviUmN+EgEM=
`
