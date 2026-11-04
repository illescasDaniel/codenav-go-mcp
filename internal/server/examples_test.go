package server

import (
	"strings"
	"testing"
)

const exampleShop = "../../examples/shop"

// The README shows this exact answer; keep them in step.
func TestExampleShopSymbolInfo(t *testing.T) {
	cs, _ := sessionFrom(t, exampleShop, nil)
	out := call(t, cs, "symbol_info", map[string]any{"name": "Cart"})
	for _, want := range []string{
		"Cart  [struct]  (cart/cart.go:5:6)",
		"func (c *Cart) Add(it Item)",
		"func (c *Cart) Total() int64",
		"4 reference(s) in 2 file(s):",
		"cart/cart.go: L5, L18, L27",
		"cmd/shop/main.go: L10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("symbol_info Cart lacks %q:\n%s", want, out)
		}
	}
}

func TestExampleShopImplementations(t *testing.T) {
	cs, _ := sessionFrom(t, exampleShop, nil)
	out := call(t, cs, "implementations", map[string]any{"name": "Pricer"})
	if !strings.Contains(out, "MapPricer") {
		t.Errorf("implementations of Pricer does not list MapPricer:\n%s", out)
	}
}

func TestExampleShopRenamePreviewThenApply(t *testing.T) {
	cs, root := sessionFrom(t, exampleShop, nil)
	out := call(t, cs, "rename_symbol", map[string]any{"name": "Cart.Total", "new_name": "Sum", "apply": false})
	if !strings.Contains(out, "Sum") || strings.Contains(out, "Error") {
		t.Errorf("rename preview unexpected:\n%s", out)
	}
	if b := readFile(t, root, "cart/cart.go"); strings.Contains(b, "func (c *Cart) Sum") {
		t.Error("apply=false wrote the file")
	}
	call(t, cs, "rename_symbol", map[string]any{"name": "Cart.Total", "new_name": "Sum"})
	if b := readFile(t, root, "cmd/shop/main.go"); !strings.Contains(b, "c.Sum()") {
		t.Errorf("rename did not reach the caller:\n%s", b)
	}
}

func TestExampleShopHasNoDiagnostics(t *testing.T) {
	cs, _ := sessionFrom(t, exampleShop, nil)
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "cart/cart.go"})
	if !strings.Contains(out, "No diagnostics") {
		t.Errorf("example has diagnostics:\n%s", out)
	}
}
