package tools

import (
	"context"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// Import candidates for an undefined qualifier (BUG-QF-001, BUG-QF-002). gopls offers one `Add import` per
// undefined name, so a name several packages could provide (yaml.v2 and yaml.v3 both used) would be added
// silently. The options are gopls' offer plus the packages the workspace and its module graph hold under that
// name, and only packages a module of the build list provides are kept.

// importOption is one `Add import` option: the import path, and the name gopls gave it in its title (if any).
type importOption struct {
	Alias string
	Path  string
}

// importTitle is the title of the option as quick_fix lists it (choice= matches against it).
func importTitle(o importOption) string { return "Add import " + strconv.Quote(o.Path) }

// importSpec is the import spec the option adds (`alias "path"` or `"path"`).
func importSpec(o importOption) string {
	return strings.TrimSpace(o.Alias + " " + strconv.Quote(o.Path))
}

var (
	undefinedRE = regexp.MustCompile(`^undefined: ([A-Za-z_]\w*)`)
	selectorRE  = regexp.MustCompile(`^[ \t]*\.[ \t]*([A-Za-z_]\w*)`)
	versionElem = regexp.MustCompile(`^v[0-9]+$`)
	dotVersion  = regexp.MustCompile(`\.v[0-9]+$`)
)

// undefinedQualifier reports whether d is gopls' "undefined: NAME" and returns NAME and the selector written after
// it (`Foo` in `lib.Foo()`, "" if there is none). Qualifiers only come from undefined names.
func undefinedQualifier(text string, d lsp.Diagnostic) (name, sel string, ok bool) {
	m := undefinedRE.FindStringSubmatch(strings.SplitN(d.Message, "\n", 2)[0])
	if m == nil {
		return "", "", false
	}
	end := edits.Offset(text, d.Range.End.Line, d.Range.End.Character)
	if end > len(text) {
		return m[1], "", true
	}
	if s := selectorRE.FindStringSubmatch(text[end:]); s != nil {
		sel = s[1]
	}
	return m[1], sel, true
}

// guessNames are the package names an import path is likely to have: its last element, without a major-version
// element (/v2) or suffix (.v3), and without a go- prefix or -go suffix. Only a filter; a candidate is checked.
func guessNames(path string) []string {
	elems := strings.Split(path, "/")
	last := elems[len(elems)-1]
	if versionElem.MatchString(last) && len(elems) > 1 {
		last = elems[len(elems)-2]
	}
	base := dotVersion.ReplaceAllString(last, "")
	base = strings.TrimSuffix(strings.TrimPrefix(base, "go-"), "-go")
	return []string{last, base}
}

func hasName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// goFilesIn lists the Go files of dir that the build constraints of this platform keep (not tests).
func goFilesIn(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if ok, err := build.Default.MatchFile(dir, n); err == nil && ok {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

// packageClauseName is the package name the files declare (the first one that parses).
func packageClauseName(files []string) string {
	fset := token.NewFileSet()
	for _, f := range files {
		if af, err := parser.ParseFile(fset, f, nil, parser.PackageClauseOnly); err == nil {
			return af.Name.Name
		}
	}
	return ""
}

// declares reports whether the files declare the top-level name sel (a func, var, const or type).
func declares(files []string, sel string) bool {
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == sel {
					return true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.Name == sel {
								return true
							}
						}
					case *ast.TypeSpec:
						if s.Name.Name == sel {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// importScan lists the packages that could stand for the qualifier name: packages named name (declaring sel when
// one is written) that the file's module can import. Its own directory is left out. It finds (a) the workspace's
// packages, (b) packages the workspace imports under such a name, and (c) the root packages of the build list.
func importScan(ctx context.Context, w *Workspace, abs, name, sel string) []importOption {
	dir := filepath.Dir(abs)
	cands := map[string]bool{}
	fits := func(p gomod.Package) bool {
		return p.Dir != dir && p.Name == name && (sel == "" || declares(p.Files, sel))
	}
	var imported []string
	seenImport := map[string]bool{}
	if pkgs, err := gomod.Packages(ctx, w.Root); err == nil {
		for _, p := range pkgs {
			if fits(p) {
				cands[p.ImportPath] = true
			}
			for _, i := range p.Imports {
				if !seenImport[i] && hasName(guessNames(i), name) {
					seenImport[i] = true
					imported = append(imported, i)
				}
			}
		}
	}
	for _, p := range gomod.Resolve(ctx, dir, imported) {
		if fits(p) {
			cands[p.ImportPath] = true
		}
	}
	if mods, ok := gomod.BuildList(ctx, dir); ok {
		for _, m := range mods {
			if m.Dir == "" || m.Dir == dir || !hasName(guessNames(m.Path), name) {
				continue
			}
			files := goFilesIn(m.Dir)
			if len(files) == 0 || packageClauseName(files) != name {
				continue
			}
			if sel == "" || declares(files, sel) {
				cands[m.Path] = true
			}
		}
	}
	out := make([]importOption, 0, len(cands))
	for path := range cands {
		out = append(out, importOption{Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// resolvableFn says whether an import path can be imported from dir: the standard library, or a path under a module
// of the build list. When go cannot list the build list, every path counts (nothing is dropped without evidence).
func resolvableFn(ctx context.Context, dir string) func(path string) bool {
	mods, ok := gomod.BuildList(ctx, dir)
	return func(p string) bool {
		if !ok {
			return true
		}
		if first, _, _ := strings.Cut(p, "/"); !strings.Contains(first, ".") {
			return true
		}
		for _, m := range mods {
			if p == m.Path || strings.HasPrefix(p, m.Path+"/") {
				return true
			}
		}
		return false
	}
}

// chooseImports is the decision for one undefined qualifier. The options are gopls' offered paths and the scanned
// candidates, those resolvable (see resolvableFn) and without duplicates, sorted by path. The offered paths that
// are not resolvable are returned as dropped; an offered option keeps the name gopls gave it.
func chooseImports(offered, scanned []importOption, resolvable func(path string) bool) (opts []importOption, dropped []string) {
	seen := map[string]bool{}
	for _, o := range offered {
		if seen[o.Path] {
			continue
		}
		seen[o.Path] = true
		if !resolvable(o.Path) {
			dropped = append(dropped, o.Path)
			continue
		}
		opts = append(opts, o)
	}
	for _, s := range scanned {
		if seen[s.Path] {
			continue
		}
		seen[s.Path] = true
		if resolvable(s.Path) {
			opts = append(opts, s)
		}
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Path < opts[j].Path })
	return opts, dropped
}

// hasAddImport reports whether gopls offered an `Add import` among acts.
func hasAddImport(acts []codeAction) bool {
	for _, a := range acts {
		if addImportRE.MatchString(a.Title) {
			return true
		}
	}
	return false
}

// importOptionsFor is chooseImports for the `Add import` actions gopls offered for the undefined qualifier.
func importOptionsFor(ctx context.Context, w *Workspace, abs, name, sel string, acts []codeAction) (opts []importOption, dropped []string) {
	var offered []importOption
	for _, a := range acts {
		if m := addImportRE.FindStringSubmatch(a.Title); m != nil {
			offered = append(offered, importOption{Alias: m[1], Path: m[2]})
		}
	}
	return chooseImports(offered, importScan(ctx, w, abs, name, sel), resolvableFn(ctx, filepath.Dir(abs)))
}
