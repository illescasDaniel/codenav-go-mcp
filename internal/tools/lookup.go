package tools

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/resolve"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// resolveSymbol is resolve.Resolve with a Go-aware explanation when nothing matches.
func resolveSymbol(ctx context.Context, w *Workspace, name, file string) (resolve.Symbol, error) {
	if strings.TrimSpace(name) == "_" || strings.HasSuffix(strings.TrimSpace(name), "._") {
		return resolve.Symbol{}, toolerr.Inputf("`_` is the blank identifier: it declares nothing and nothing refers to it, so there is nothing to rename, move or change. Name the declaration you mean (Type.Field, pkg.Func).")
	}
	sym, err := resolve.ResolveExact(ctx, w.Client, w.Index, w.Root, name, file)
	var nf *resolve.NotFoundError
	if errors.As(err, &nf) {
		return sym, &toolerr.InputError{Msg: notFoundMessage(ctx, w, nf.Query, false, nf.Suggest), Hint: nf.Suggest, NotFound: true}
	}
	return sym, err
}

// resolveSymbolRO is resolveSymbol for the read-only tools: a package-qualified name that is not in the
// workspace (json.Marshal, encoding/json.Decoder.Decode, a dependency's Client.Do) is looked up through
// a scratch gopls overlay (resolve.Probe). Write tools keep to workspace symbols: their guard refuses
// GOROOT and the module cache anyway.
func resolveSymbolRO(ctx context.Context, w *Workspace, name, file string) (resolve.Symbol, error) {
	sym, err := resolve.Resolve(ctx, w.Client, w.Index, w.Root, name, file)
	var nf *resolve.NotFoundError
	if !errors.As(err, &nf) {
		return sym, w.symbolNavErr(ctx, err, file)
	}
	if cgoQualified(nf.Query) {
		// NAVN-007: C is the pseudo-package of cgo, which go list cannot load, so no probe is launched for it. A
		// workspace type called C still gets its own not-found answer.
		if !workspaceSymbolNamed(ctx, w, "C") && packageDir(w.Root, "C") == "" {
			return sym, toolerr.NotFoundf("No symbol found matching %q: %s is a cgo name, not a Go package symbol; use hover or definition at a use of it.", nf.Query, nf.Query)
		}
		return sym, toolerr.NotFoundf("%s", notFoundMessage(ctx, w, nf.Query, false, nil))
	}
	psym, perr := resolve.Probe(ctx, w.Client, w.Root, nf.Query, workspaceImports(w.Root, file))
	switch {
	case perr == nil:
		return psym, nil
	case errors.Is(perr, resolve.ErrNoProbe):
		return sym, toolerr.NotFoundf("%s", notFoundMessage(ctx, w, nf.Query, false, nil))
	}
	var pe *resolve.ProbeError
	if errors.As(perr, &pe) {
		return sym, toolerr.NotFoundf("No symbol found matching %q: %s.", nf.Query, pe.Msg)
	}
	return sym, w.symbolNavErr(ctx, perr, file)
}

// maxImportScan bounds how many workspace files workspaceImports parses.
const maxImportScan = 3000

// workspaceImports maps each qualifier the workspace's Go files import packages under to the import
// paths (the most used first; file's own imports lead).
func workspaceImports(root, file string) map[string][]string {
	count := map[string]map[string]int{}
	note := func(path string, weight int) {
		src, _, err := edits.ReadSource(path)
		if err != nil {
			return
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			return
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := gosrc.ImportName(p)
			if imp.Name != nil {
				if imp.Name.Name == "_" || imp.Name.Name == "." {
					continue
				}
				name = imp.Name.Name
			}
			if count[name] == nil {
				count[name] = map[string]int{}
			}
			count[name][p] += weight
		}
	}
	if file != "" {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		note(p, 1_000_000)
	}
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir():
			if p != root && (exclude.IsDirName(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
		case strings.HasSuffix(p, ".go"):
			if n++; n > maxImportScan {
				return filepath.SkipAll
			}
			note(p, 1)
		}
		return nil
	})
	out := map[string][]string{}
	for name, paths := range count {
		for p := range paths {
			out[name] = append(out[name], p)
		}
		sort.Slice(out[name], func(i, j int) bool {
			a, b := out[name][i], out[name][j]
			return paths[a] > paths[b] || paths[a] == paths[b] && a < b
		})
	}
	return out
}

// builtins are Go's predeclared identifiers: they have no declaration to find.
var builtins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true, "delete": true,
	"imag": true, "len": true, "make": true, "max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true, "any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true, "true": true, "false": true, "iota": true, "nil": true,
}

// typeKinds are the LSP symbol kinds gopls uses for type declarations.
var typeKinds = map[int]bool{5: true, 10: true, 11: true, 23: true, 26: true}

// notFoundMessage explains why query matches no workspace symbol, with the hint that fits.
//
// ro is set for the read-only tools, which already tried a package-qualified name outside the workspace.
func notFoundMessage(ctx context.Context, w *Workspace, query string, ro bool, suggest []string) string {
	msg := fmt.Sprintf("No symbol found matching %q", shortQuery(query))
	parts := strings.Split(query, ".")
	first, last := parts[0], parts[len(parts)-1]
	outside := "; write tools resolve only workspace symbols - symbol_info, callers and implementations also take json.Marshal or encoding/json.Decoder.Decode, and hover or definition work at a use of any symbol."
	if ro {
		outside = "; for a standard-library or dependency symbol spell the import path (math/rand.Int, github.com/x/y.Client.Do; the module must be in go.mod)."
	}
	switch {
	case strings.Trim(query, ".") == "":
		msg += ": a symbol name is needed (Square.Area, shapes.Scale), not just dots."
	case len(parts) > 1 && last == "":
		msg += fmt.Sprintf(": %q ends with a dot; name the member after it (%sName), or drop the dot to look up %s itself.", query, query, strings.TrimSuffix(query, "."))
	case len(parts) > 1 && first == "":
		msg += fmt.Sprintf(": %q starts with a dot; write Type.Member or package.Name, or drop the dot to look up %s itself.", query, strings.TrimPrefix(query, "."))
	case len(parts) == 1 && builtins[query]:
		msg += ": " + query + " is a Go builtin (predeclared), with no declaration to show."
	case len(parts) == 1 && packageDir(w.Root, query) != "":
		dir := packageDir(w.Root, query)
		msg += fmt.Sprintf(": %s is a package (directory %s), not a symbol; outline its files or use search_symbol with path=%s.", query, dir, dir)
	case len(parts) == 1 && isLocalName(query):
		// NAVN-005: a local or a parameter is in no index; only the position tools reach it.
		msg += " (struct fields and methods are named Type.Name; local variables and parameters are not indexed by name: use hover, definition or references at one of their uses)."
	case len(parts) == 1:
		msg += " (struct fields and methods are named Type.Name)."
	case isWorkspaceType(ctx, w, first):
		msg += promotedRefusal(ctx, w, first, last)
	case packageDir(w.Root, first) != "":
		msg += fmt.Sprintf(": package %s declares no %s.", first, strings.Join(parts[1:], "."))
	case importAlias(w.Root, first) != "":
		// BED-002: the workspace imports the package under another name (metav1 for .../meta/v1).
		path := importAlias(w.Root, first)
		pkg, rest := gosrc.ImportName(path), strings.Join(parts[1:], ".")
		msg += fmt.Sprintf(": %s is an import alias for %s, not a package name (the package is %s); write %s.%s, or the import path %s.%s",
			first, path, pkg, pkg, rest, path, rest)
	case isStdlib(first):
		msg += fmt.Sprintf(": %s is in the standard library, outside the workspace%s", first, outside)
	case suggestsQualifier(suggest, first, false):
		// Only the member is miscased (main.Init1 for main.init1): the package itself exists, even when it
		// is no directory of that name (package main at the root).
		msg += fmt.Sprintf(": package %s declares no %s.", first, strings.Join(parts[1:], "."))
	case suggestsQualifier(suggest, first, true):
		msg += fmt.Sprintf(": no type or package named %s in the workspace (the qualifier is case-sensitive too).", first)
	default:
		msg += fmt.Sprintf(": no type or package named %s in the workspace%s", first, outside)
	}
	if len(suggest) > 0 {
		quoted := make([]string, len(suggest))
		for i, n := range suggest {
			quoted[i] = "`" + n + "`"
		}
		msg += fmt.Sprintf(" Did you mean %s? (names are case-sensitive; write tools never guess the case)", strings.Join(quoted, " or "))
	}
	var nested []gomod.Nested
	for _, n := range gomod.UncoveredModules(w.Root, exclude.IsDirName, 3) {
		if mayLiveIn(n, query) {
			nested = append(nested, n)
		}
	}
	if len(nested) > 0 {
		var names []string
		for _, n := range nested {
			rel := edits.RelativeName(n.Dir, w.Root)
			if n.Path != "" {
				rel = n.Path + " (" + rel + ")"
			}
			names = append(names, rel)
		}
		msg += fmt.Sprintf("\nNote: module(s) %s sit below the workspace without a go.work listing them; gopls indexes each as a separate folder, so other modules importing them are not seen. A go.work (go work init . ./<dir>) joins them.", strings.Join(names, ", "))
	}
	return msg
}

// suggestsQualifier reports whether a suggested spelling ("cs.Foo") has the qualifier first spelled exactly
// (fold false) or only equal to it ignoring case (fold true).
func suggestsQualifier(suggest []string, first string, fold bool) bool {
	for _, sg := range suggest {
		q, _, ok := strings.Cut(sg, ".")
		if ok && (!fold && q == first || fold && q != first && strings.EqualFold(q, first)) {
			return true
		}
	}
	return false
}

// mayLiveIn: the missing name could be declared in the nested module n - its qualifier names the module or
// one of its directories, or the name's last part appears in its Go files. Otherwise the module has
// nothing to do with the failed lookup.
func mayLiveIn(n gomod.Nested, query string) bool {
	parts := strings.Split(query, ".")
	last := parts[len(parts)-1]
	if last == "" {
		return false
	}
	if len(parts) > 1 {
		first := parts[0]
		if path.Base(n.Path) == first || filepath.Base(n.Dir) == first || strings.Contains(n.Path, "/"+first+"/") {
			return true
		}
		if packageDir(n.Dir, first) != "" {
			return true
		}
	}
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(last) + `\b`)
	seen, found := 0, false
	_ = filepath.WalkDir(n.Dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || found:
			return nil
		case d.IsDir():
			if p != n.Dir && (exclude.IsDirName(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
		case strings.HasSuffix(p, ".go"):
			if seen++; seen > 300 {
				return filepath.SkipAll
			}
			if b, rerr := os.ReadFile(p); rerr == nil && word.Match(b) {
				found = true
			}
		}
		return nil
	})
	return found
}

// isLocalName reports whether a bare name is spelled like a local variable or parameter (a lower-case
// identifier that is not a predeclared one).
func isLocalName(name string) bool {
	r := []rune(name)
	return len(r) > 0 && (unicode.IsLower(r[0]) || r[0] == '_') && token.IsIdentifier(name) && !builtins[name]
}

// importAlias is the import path the workspace's files bind the qualifier q to under a name other than the
// package's own ("" when none does).
func importAlias(root, q string) string {
	for _, p := range workspaceImports(root, "")[q] {
		if gosrc.ImportName(p) != q {
			return p
		}
	}
	return ""
}

func isStdlib(qualifier string) bool {
	spec, ambiguous := gosrc.StdlibImport(qualifier)
	return spec != "" || ambiguous
}

// isWorkspaceType reports whether a type called name is declared in the workspace.
func isWorkspaceType(ctx context.Context, w *Workspace, name string) bool {
	return len(workspaceTypeFiles(ctx, w, name)) > 0
}

// workspaceSymbolNamed reports whether a symbol called name, of any kind, is declared in the workspace.
func workspaceSymbolNamed(ctx context.Context, w *Workspace, name string) bool {
	syms, err := w.Client.WorkspaceSymbol(ctx, name)
	if err != nil {
		return false
	}
	for _, s := range syms {
		if s.Name == name && s.Location != nil && w.inWorkspace(filepath.Clean(lsp.URIToPath(s.Location.URI))) {
			return true
		}
	}
	return false
}

// workspaceTypeFiles lists the distinct files that declare a type called name in the workspace.
func workspaceTypeFiles(ctx context.Context, w *Workspace, name string) []string {
	syms, err := w.Client.WorkspaceSymbol(ctx, name)
	if err != nil {
		return nil
	}
	var files []string
	seen := map[string]bool{}
	for _, s := range syms {
		if s.Name == name && typeKinds[s.Kind] && s.Location != nil {
			if p := filepath.Clean(lsp.URIToPath(s.Location.URI)); w.inWorkspace(p) && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	return files
}

// cgoQualified reports whether query names a member of the cgo pseudo-package C (C.malloc, C.int).
func cgoQualified(query string) bool {
	first, rest, ok := strings.Cut(query, ".")
	return ok && first == "C" && rest != ""
}

// promotedRefusal explains "typ.member" when typ declares no member. A method promoted through embedding is not
// a declared symbol, so the answer names the embedded type that declares it (ESYM-008). The owner is named only when
// one workspace type carries the name: with two packages each declaring typ, which one is meant is unknown, so the
// general hint stands. When the embedded types of the package carry no such member, the general hint stands too.
func promotedRefusal(ctx context.Context, w *Workspace, typ, member string) string {
	if files := workspaceTypeFiles(ctx, w, typ); len(files) == 1 {
		dir := filepath.Dir(files[0])
		if owners := promotedFrom(dir, typ, declaresMember(ctx, w, dir, member)); len(owners) > 0 {
			uses := make([]string, len(owners))
			for i, o := range owners {
				uses[i] = o + "." + member
			}
			return fmt.Sprintf(": type %s declares no %s (methods promoted through embedding are not declared symbols): %s is declared on %s (embedded in %s); use %s.",
				typ, member, member, strings.Join(owners, ", "), typ, strings.Join(uses, " or "))
		}
		// A qualified embed (pkg.Inner) lies in another package, which is not read: the answer names it instead.
		if quals := qualifiedEmbeds(dir, typ); len(quals) > 0 {
			sentences := make([]string, len(quals))
			for i, q := range quals {
				sentences[i] = fmt.Sprintf("`%s` embeds `%s` (another package); `%s` may be promoted from it: query `%s.%s`.", typ, q, member, q, member)
			}
			return fmt.Sprintf(": type %s declares no %s (methods promoted through embedding are not declared symbols): %s",
				typ, member, strings.Join(sentences, " "))
		}
	}
	return fmt.Sprintf(": type %s declares no %s (methods promoted through embedding are not declared symbols; query the embedded type's method instead).", typ, member)
}

// declaresMember returns, for a type name N, whether the package in dir declares the member N.member. The
// workspace symbols named member are read once. gopls caps its answer, so a cut list can only miss an owner.
func declaresMember(ctx context.Context, w *Workspace, dir, member string) func(string) bool {
	have := map[string]bool{}
	if syms, err := w.Client.WorkspaceSymbol(ctx, member); err == nil {
		for _, s := range syms {
			if s.Location != nil && filepath.Dir(lsp.URIToPath(s.Location.URI)) == dir {
				have[s.Name] = true
			}
		}
	}
	return func(name string) bool { return have[name+"."+member] }
}

// maxEmbedDepth bounds how far promotedFrom follows embedded types.
const maxEmbedDepth = 8

// promotedFrom names the types that member is promoted to typ from: the embedded fields of typ, read from the Go
// files of dir (its package), breadth first, so the shallowest types that carry the member are the answer. hasMember
// says which types carry it. A qualified embedded type (fmt.Stringer) is not followed: it lies in another package.
func promotedFrom(dir, typ string, hasMember func(string) bool) []string {
	embeds := embeddedTypes(dir)
	level := []string{typ}
	seen := map[string]bool{typ: true}
	for depth := 0; len(level) > 0 && depth < maxEmbedDepth; depth++ {
		var next, found []string
		for _, t := range level {
			for _, e := range embeds[t] {
				if seen[e] {
					continue
				}
				seen[e] = true
				if hasMember(e) {
					found = append(found, e)
				} else {
					next = append(next, e)
				}
			}
		}
		if len(found) > 0 {
			return found
		}
		level = next
	}
	return nil
}

// embeddedTypes maps each struct type declared in the non-test Go files of dir to the types it embeds by bare
// name (Inner, *Inner).
func embeddedTypes(dir string) map[string][]string {
	out := map[string][]string{}
	forEachEmbed(dir, func(typ string, t ast.Expr) {
		if id, ok := t.(*ast.Ident); ok {
			out[typ] = append(out[typ], id.Name)
		}
	})
	return out
}

// qualifiedEmbeds lists the qualified types that the struct typ embeds in the non-test Go files of dir, as the
// source spells them (fmt.Stringer, *inner.Inner -> "inner.Inner"). The other package is not read.
func qualifiedEmbeds(dir, typ string) []string {
	var out []string
	forEachEmbed(dir, func(name string, t ast.Expr) {
		sel, ok := t.(*ast.SelectorExpr)
		if !ok || name != typ {
			return
		}
		if pkg, ok := sel.X.(*ast.Ident); ok {
			out = append(out, pkg.Name+"."+sel.Sel.Name)
		}
	})
	return out
}

// forEachEmbed calls visit for each embedded field of each struct type declared in the non-test Go files of dir, with
// the name of the struct and the field's type (a pointer is unwrapped).
func forEachEmbed(dir string, visit func(typ string, t ast.Expr)) {
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
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					if len(field.Names) > 0 {
						continue
					}
					t := field.Type
					if star, ok := t.(*ast.StarExpr); ok {
						t = star.X
					}
					visit(ts.Name.Name, t)
				}
			}
		}
	}
}
