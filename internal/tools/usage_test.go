package tools

import (
	"strings"
	"testing"
)

func TestDropNulls(t *testing.T) {
	got := string(dropNulls([]byte(`{"name":null,"line":3,"x": null}`)))
	if strings.Contains(got, "null") || !strings.Contains(got, `"line":3`) {
		t.Errorf("got %s", got)
	}
	in := []byte(`{"a":1}`)
	if string(dropNulls(in)) != string(in) {
		t.Error("input without nulls must be unchanged")
	}
}

func TestTidyHoverDropsBogusLinks(t *testing.T) {
	for _, link := range []string{
		"[`outdep.Hello` on pkg.go.dev](https://pkg.go.dev/../outdep@#Hello)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/./x@#F)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/example.com/x@#F)",
		"[`main.F` on pkg.go.dev](https://pkg.go.dev/command-line-arguments#F)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/example.com/x#F)",
		"[`x` on pkg.go.dev](https://pkg.go.dev/example.com/x/sub)",
	} {
		if got := tidyHover("func F()\n\n---\n\n" + link); strings.Contains(got, "pkg.go.dev") {
			t.Errorf("%s kept: %q", link, got)
		}
	}
	for _, link := range []string{
		"[`fmt.Println` on pkg.go.dev](https://pkg.go.dev/fmt#Println)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/example.com/x@v1.2.3#F)",
	} {
		if got := tidyHover("func F()\n\n---\n\n" + link); !strings.Contains(got, "pkg.go.dev") {
			t.Errorf("%s dropped: %q", link, got)
		}
	}
}

// Round 13 (item 3): a number that overflows an int is echoed as a number only, never with the JSON that
// follows it in the request.
func TestFriendlyDecodeErrorOverflowEchoesNoJSONTail(t *testing.T) {
	raw := `json: cannot unmarshal number 9223372036854776000,"file_path":... overflows into Go struct field tools.posIn.column of type int`
	got := friendlyDecodeError(raw)
	if want := "column must be a whole number of reasonable size (got a number too large, which does not fit)."; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	for _, bad := range []string{"file_path", `"`, "..."} {
		if strings.Contains(got, bad) {
			t.Errorf("%q leaked into %q", bad, got)
		}
	}
	// A plain value is still echoed.
	if got := friendlyDecodeError(`json: cannot unmarshal "1e+30" into Go struct field tools.posIn.column of type int`); !strings.Contains(got, `1e+30`) {
		t.Errorf("plain values keep their echo: %q", got)
	}
}

// Round 14 (item 1): the decoder names a field of the argument map as "map[string]interface {}.column".
func TestFriendlyDecodeErrorMapField(t *testing.T) {
	got := friendlyDecodeError(`validating "arguments": unmarshaling arguments: json: cannot unmarshal "1e400" into Go struct field map[string]interface {}.column of type float64`)
	if !strings.HasPrefix(got, "column has the wrong type") && !strings.HasPrefix(got, "column must be") {
		t.Errorf("raw decoder text not rewritten: %q", got)
	}
}
