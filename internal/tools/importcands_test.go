package tools

import (
	"reflect"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// BUG-QF-002: gopls offered a path no module of the build list provides (only in the module cache). It is not
// an option; the modules the go.mod does provide are the candidates.
func TestChooseImportsDropsUnresolvableOffer(t *testing.T) {
	resolvable := func(p string) bool { return p != "go.yaml.in/yaml/v3" }
	offered := []importOption{{Path: "go.yaml.in/yaml/v3"}}
	scanned := []importOption{{Path: "gopkg.in/yaml.v3"}, {Path: "gopkg.in/yaml.v2"}}
	opts, dropped := chooseImports(offered, scanned, resolvable)
	if want := []importOption{{Path: "gopkg.in/yaml.v2"}, {Path: "gopkg.in/yaml.v3"}}; !reflect.DeepEqual(opts, want) {
		t.Errorf("options = %+v, want %+v", opts, want)
	}
	if want := []string{"go.yaml.in/yaml/v3"}; !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
	// Nothing resolvable at all: no option, and the dropped path is what the reason names.
	opts, dropped = chooseImports(offered, nil, resolvable)
	if len(opts) != 0 || !reflect.DeepEqual(dropped, []string{"go.yaml.in/yaml/v3"}) {
		t.Errorf("only an unresolvable offer: options %+v, dropped %v", opts, dropped)
	}
}

// The offer (with the alias gopls gave it) and a scanned candidate for the same path are one option; the
// standard library offer stays a single option.
func TestChooseImportsKeepsOfferAndDedupes(t *testing.T) {
	always := func(string) bool { return true }
	offered := []importOption{{Alias: "str", Path: "strings"}, {Path: "fmt"}}
	scanned := []importOption{{Path: "strings"}, {Path: "example.com/x/strings"}}
	opts, dropped := chooseImports(offered, scanned, always)
	want := []importOption{{Path: "example.com/x/strings"}, {Path: "fmt"}, {Alias: "str", Path: "strings"}}
	if !reflect.DeepEqual(opts, want) || len(dropped) != 0 {
		t.Errorf("options = %+v (dropped %v), want %+v", opts, dropped, want)
	}
	if spec := importSpec(opts[2]); spec != `str "strings"` {
		t.Errorf("alias lost: %q", spec)
	}
}

func TestUndefinedQualifierReadsTheSelector(t *testing.T) {
	text := "package use\n\nfunc U() int { return lib.Foo() }\n"
	d := lsp.Diagnostic{
		Message: "undefined: lib",
		Range:   lsp.Range{Start: lsp.Position{Line: 2, Character: 22}, End: lsp.Position{Line: 2, Character: 25}},
	}
	name, sel, ok := undefinedQualifier(text, d)
	if !ok || name != "lib" || sel != "Foo" {
		t.Errorf("undefinedQualifier = %q, %q, %v; want lib, Foo, true", name, sel, ok)
	}
	d.Message = "declared and not used: lib"
	if _, _, ok := undefinedQualifier(text, d); ok {
		t.Errorf("only an undefined name is a qualifier")
	}
}

func TestGuessNames(t *testing.T) {
	for path, want := range map[string]string{
		"gopkg.in/yaml.v3":    "yaml",
		"example.com/x/v2":    "x",
		"github.com/a/go-cfg": "cfg",
	} {
		if !hasName(guessNames(path), want) {
			t.Errorf("guessNames(%q) = %v, want %q among them", path, guessNames(path), want)
		}
	}
}
