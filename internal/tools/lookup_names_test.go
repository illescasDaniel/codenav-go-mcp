package tools

import (
	"context"
	"strings"
	"testing"
)

// NAVN-005: a bare lowercase name is most often a local or a parameter, which no index holds: the message says so
// and points at the position tools, instead of implying the name is a missing field or method.
func TestNotFoundMessageNamesLocalVariables(t *testing.T) {
	dir := writeModule(t, map[string]string{"p/p.go": "package p\n\nfunc F(ctx int) int { return ctx }\n"})
	w := &Workspace{Root: dir}
	msg := notFoundMessage(context.Background(), w, "ctx", false, nil)
	if !strings.Contains(msg, "not indexed by name") || !strings.Contains(msg, "hover, definition or references") {
		t.Errorf("bare local name: %q", msg)
	}
	for name, want := range map[string]bool{
		"ctx":    true,
		"_x":     true,
		"Square": false,
		"int":    false, // a builtin, not a local
		"":       false,
		"a b":    false,
	} {
		if got := isLocalName(name); got != want {
			t.Errorf("isLocalName(%q) = %v, want %v", name, got, want)
		}
	}
}

// BED-002: a qualifier the workspace binds under another name is explained as an alias, not as an unknown package.
// The test covers the lookup of the alias itself; the message wiring is exercised by the cases above.
func TestImportAliasFindsTheBoundPackage(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go": "package p\n\nimport (\n\tmetav1 \"k8s.io/apimachinery/pkg/apis/meta/v1\"\n\t\"fmt\"\n)\n\nvar _ metav1.ObjectMeta\nvar _ = fmt.Sprint\n",
	})
	if got := importAlias(dir, "metav1"); got != "k8s.io/apimachinery/pkg/apis/meta/v1" {
		t.Errorf("importAlias(metav1) = %q, want the apimachinery meta/v1 path", got)
	}
	if got := importAlias(dir, "fmt"); got != "" {
		t.Errorf("importAlias(fmt) = %q, want none (fmt is imported under its own name)", got)
	}
	if got := importAlias(dir, "nosuch"); got != "" {
		t.Errorf("importAlias(nosuch) = %q, want none", got)
	}
}
