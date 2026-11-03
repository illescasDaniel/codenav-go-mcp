package resolve

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// normalizeQuery rewrites the spellings of a receiver that name a symbol the same way the plain name does:
// "(*T).M" and "*T.M" are "T.M", and a generic receiver "T[P].M" (its type arguments) is "T.M". A bracket that is
// not followed by a dot (Foo[int]) is left alone.
func normalizeQuery(q string) string {
	if strings.HasPrefix(q, "(") {
		if i := strings.Index(q, ")."); i > 0 {
			q = strings.TrimPrefix(q[1:i], "*") + q[i+1:]
		}
	}
	if strings.HasPrefix(q, "*") && strings.Contains(q[1:], ".") {
		q = q[1:]
	}
	return stripTypeArgs(q)
}

// stripTypeArgs removes each bracketed type argument list that is followed by a dot ("List[T]." becomes "List.").
func stripTypeArgs(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); {
		if q[i] == '[' {
			depth, j := 0, i
			for ; j < len(q); j++ {
				if q[j] == '[' {
					depth++
				} else if q[j] == ']' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j+1 < len(q) && q[j] == ']' && q[j+1] == '.' {
				i = j + 1
				continue
			}
		}
		b.WriteByte(q[i])
		i++
	}
	return b.String()
}

// localAliases maps each type alias declared in the package directory dir to the name it aliases, when that
// is a bare identifier (type A = B: "A" to "B"; a qualified or generic alias is not followed). It reads the
// files as they are now: an alias is not cached, since an edit can change it.
func localAliases(dir string) map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	fset := token.NewFileSet()
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".go" || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, _ := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if f == nil {
			continue
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.TYPE {
				continue
			}
			for _, sp := range g.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok || !ts.Assign.IsValid() || ts.TypeParams != nil {
					continue
				}
				if id, ok := ts.Type.(*ast.Ident); ok {
					out[ts.Name.Name] = id.Name
				}
			}
		}
	}
	return out
}

// canonicalType follows the aliases of one package to the type they name (A = B, B = C: A is C); name itself
// when it is no alias. A cycle, which does not compile, stops after a bounded number of steps.
func canonicalType(aliases map[string]string, name string) string {
	for i := 0; i < 16; i++ {
		t, ok := aliases[name]
		if !ok || t == name {
			return name
		}
		name = t
	}
	return name
}

// aliasedMembers finds the candidates the query names through a type alias: the query "T.M" (or "pkg.T.M") also
// means "X.M" when X and T are the same type in the candidate's package. So the method of an alias
// (func (s Sq) Area) answers Square.Area, and a field reached through an alias chain (A = B, B = C) answers A.F.
// cands are the symbols known by the member's name ("X.M"); the candidate's own package decides what each name
// means, so nothing is guessed across packages.
func aliasedMembers(cands []*lsp.Symbol, query string) []*lsp.Symbol {
	parts := strings.Split(query, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return nil
	}
	member, qtype := parts[len(parts)-1], parts[len(parts)-2]
	qpkg := ""
	if len(parts) == 3 {
		qpkg = parts[0]
	}
	aliasesOf := map[string]map[string]string{}
	var out []*lsp.Symbol
	for _, s := range cands {
		if s.Location == nil {
			continue
		}
		i := strings.LastIndex(s.Name, ".")
		if i < 0 || s.Name[i+1:] != member || strings.Contains(s.Name[:i], ".") {
			continue
		}
		if qpkg != "" && pkgBase(s.ContainerName) != qpkg {
			continue
		}
		dir := filepath.Dir(canonPath(s.Location.URI))
		al, ok := aliasesOf[dir]
		if !ok {
			al = localAliases(dir)
			aliasesOf[dir] = al
		}
		if canonicalType(al, s.Name[:i]) == canonicalType(al, qtype) {
			out = append(out, s)
		}
	}
	return out
}

// memberNamed lists the symbols that are a member called name ("Config.Name" for the bare name "Name").
func memberNamed(cands []*lsp.Symbol, name string) []*lsp.Symbol {
	var out []*lsp.Symbol
	for _, s := range cands {
		if i := strings.LastIndex(s.Name, "."); i > 0 && s.Name[i+1:] == name && !strings.Contains(s.Name[:i], ".") {
			out = append(out, s)
		}
	}
	return out
}

// memberRefusal answers a bare member name ("Name") that only exists qualified: the members that carry it, so the
// caller can pick one. The list is capped; the total is stated.
func memberRefusal(name string, mem []*lsp.Symbol, root string) error {
	const shown = 20
	list := mem
	more := ""
	if len(list) > shown {
		more = fmt.Sprintf("\n  ... and %d more", len(list)-shown)
		list = list[:shown]
	}
	return toolerr.Inputf("%q is a member name, not a symbol: %d member(s) carry it (struct fields and methods are named Type.Name). Qualify it with its type (Type.%s):\n%s%s",
		name, len(mem), name, candidateLines(list, root), more)
}

// qualifierRefusal answers "q.Name" when q is both a workspace package and a standard-library package: it names
// both import paths, since which one a file means depends on its imports.
func qualifierRefusal(query, std string, cands []*lsp.Symbol) error {
	i := strings.LastIndex(query, ".")
	q, member := query[:i], query[i+1:]
	seen := map[string]bool{}
	var local []string
	for _, c := range cands {
		if c.ContainerName != std && pkgBase(c.ContainerName) == q && !seen[c.ContainerName] {
			seen[c.ContainerName] = true
			local = append(local, c.ContainerName)
		}
	}
	return toolerr.Inputf("%q names two packages: %s in the workspace and the standard library's %s. Write the import path of the one you mean: `%s.%s` (standard library) or `%s.%s` (workspace).",
		query, strings.Join(local, ", "), std, std, member, local[0], member)
}

var (
	gorootOnce  sync.Once
	goroot      string
	exportsMu   sync.Mutex
	exportsMemo = map[string]map[string]bool{}
)

// stdlibExports is the set of exported package-level names of the standard-library package at path, read from
// the sources under GOROOT and memoised. It is nil when GOROOT is unknown (no go command, a -trimpath build) or the
// package is not there: a nil set declares nothing, so nothing is refused for it.
func stdlibExports(path string) map[string]bool {
	gorootOnce.Do(func() {
		if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
			goroot = strings.TrimSpace(string(out))
		}
	})
	if goroot == "" {
		return nil
	}
	exportsMu.Lock()
	defer exportsMu.Unlock()
	if m, ok := exportsMemo[path]; ok {
		return m
	}
	m := readExports(filepath.Join(goroot, "src", filepath.FromSlash(path)))
	exportsMemo[path] = m
	return m
}

// readExports collects the exported top-level names of the non-test .go files in dir.
func readExports(dir string) map[string]bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	add := func(name string) {
		if token.IsExported(name) {
			out[name] = true
		}
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					add(d.Name.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						add(s.Name.Name)
					case *ast.ValueSpec:
						for _, id := range s.Names {
							add(id.Name)
						}
					}
				}
			}
		}
	}
	return out
}

// qualifierAmbiguous returns the standard-library import path that a two-part query "q.Name" also names, when
// the candidates come from a workspace package called q: without a file the qualifier cannot be read, so the
// answer is a refusal that offers both paths. "" when the qualifier is not ambiguous.
func qualifierAmbiguous(query string, cands []*lsp.Symbol) string {
	i := strings.LastIndex(query, ".")
	if i < 0 || strings.ContainsAny(query[:i], "./") {
		return ""
	}
	spec, _ := gosrc.StdlibImport(query[:i])
	if spec == "" {
		return ""
	}
	std, err := strconv.Unquote(spec)
	if err != nil {
		return ""
	}
	// NAVN-010: the standard-library package is a candidate only when it really declares the member; otherwise
	// the workspace's package is the only reading and nothing is refused.
	if !stdlibExports(std)[query[i+1:]] {
		return ""
	}
	for _, c := range cands {
		if c.ContainerName != "" && c.ContainerName != std && pkgBase(c.ContainerName) == query[:i] {
			return std
		}
	}
	return ""
}
