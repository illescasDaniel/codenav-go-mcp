package gosrc

import (
	"go/ast"
	"go/parser"
	"regexp"
	"strconv"
	"strings"
)

// Qualifiers lists the package qualifiers (the x of x.Y) that the Go expression or type expr
// uses, sorted by first appearance. Unparsable text yields none.
func Qualifiers(expr string) []string {
	e, err := parser.ParseExpr(expr)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && !seen[id.Name] {
				seen[id.Name] = true
				out = append(out, id.Name)
			}
		}
		return true
	})
	return out
}

// UsesCgo reports whether src imports the pseudo-package "C" and the byte range [start,end) refers
// to it (a C.xxx selector).
func UsesCgo(filename, src string, start, end int) bool {
	fset, f, err := parse(filename, src)
	if err != nil {
		return false
	}
	cgo := false
	for _, i := range f.Imports {
		if i.Path.Value == `"C"` {
			cgo = true
		}
	}
	if !cgo {
		return false
	}
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "C" {
				if off := fset.Position(id.Pos()).Offset; off >= start && off < end {
					used = true
				}
			}
		}
		return !used
	})
	return used
}

var versionRE = regexp.MustCompile(`^v[0-9]+$`)

// ImportName is the name an import of importPath is used under without an alias: its last
// element (the one before a /vN suffix).
func ImportName(importPath string) string {
	parts := strings.Split(importPath, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && versionRE.MatchString(last) {
		last = parts[len(parts)-2]
	}
	return last
}

// ImportScope maps the qualifier each import of src is used under to its `alias "path"` spec
// (blank and dot imports are left out), and lists the names declared at file scope.
func ImportScope(filename, src string) (imports map[string]string, declared map[string]bool) {
	imports, declared = map[string]string{}, map[string]bool{}
	_, f, err := parse(filename, src)
	if err != nil {
		return
	}
	for _, i := range f.Imports {
		p, _ := strconv.Unquote(i.Path.Value)
		name, spec := ImportName(p), i.Path.Value
		if i.Name != nil {
			if i.Name.Name == "_" || i.Name.Name == "." {
				continue
			}
			name, spec = i.Name.Name, i.Name.Name+" "+spec
		}
		imports[name] = spec
	}
	for _, n := range declNames(f) {
		declared[n] = true
	}
	return
}

// stdlibPackages maps the qualifier of an unambiguous standard-library package to its path.
var stdlibPackages = map[string]string{
	"bufio": "bufio", "bytes": "bytes", "context": "context", "errors": "errors", "flag": "flag",
	"fmt": "fmt", "io": "io", "log": "log", "maps": "maps", "math": "math", "net": "net",
	"os": "os", "path": "path", "reflect": "reflect", "regexp": "regexp", "runtime": "runtime",
	"slices": "slices", "sort": "sort", "strconv": "strconv", "strings": "strings", "sync": "sync",
	"testing": "testing", "time": "time", "unicode": "unicode", "iter": "iter", "cmp": "cmp",
	"json": "encoding/json", "xml": "encoding/xml", "csv": "encoding/csv", "binary": "encoding/binary",
	"hex": "encoding/hex", "base64": "encoding/base64", "http": "net/http", "url": "net/url",
	"filepath": "path/filepath", "atomic": "sync/atomic", "utf8": "unicode/utf8", "slog": "log/slog",
	"exec": "os/exec", "signal": "os/signal", "big": "math/big", "fs": "io/fs", "ioutil": "io/ioutil",
	"tabwriter": "text/tabwriter", "sha256": "crypto/sha256", "md5": "crypto/md5", "heap": "container/heap",
	"list": "container/list", "pprof": "runtime/pprof", "httptest": "net/http/httptest", "iotest": "testing/iotest",
}

// ambiguousStdlib are qualifiers several standard packages share; they need an explicit import.
var ambiguousStdlib = map[string]bool{"rand": true, "template": true, "scanner": true, "parser": true, "printer": true, "format": true, "token": true}

// StdlibImport resolves a qualifier to a standard-library import spec. ambiguous is set when
// several packages share the name; both results are empty for an unknown name.
func StdlibImport(qualifier string) (spec string, ambiguous bool) {
	if p, ok := stdlibPackages[qualifier]; ok {
		return strconv.Quote(p), false
	}
	return "", ambiguousStdlib[qualifier]
}
