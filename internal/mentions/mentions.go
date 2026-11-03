// Package mentions finds places that still use the old name after a rename: what the type
// checker could not link (strings, comments, docs, dynamic uses).
package mentions

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
)

// maxFiles bounds the files a leftover scan reads; a variable so that tests can shrink it.
var maxFiles = 2000

const maxBytes = 512_000

// ScanCap is the number of files a leftover scan reads at most (FindScopedLimited reports when it stopped there).
func ScanCap() int { return maxFiles }

var textSuffixes = map[string]bool{".md": true, ".rst": true, ".txt": true, ".toml": true, ".yml": true, ".yaml": true, ".json": true, ".cfg": true, ".ini": true}

type Mention struct {
	Path    string
	Line    int
	Col     int    // 1-based byte column of a code mention, else 0
	Kind    string // code | string | comment | text
	Snippet string
	loose   bool // found by the relaxed whole-word rule for short names (capped)
}

func snippet(lines []string, row int) string {
	if row < 1 || row > len(lines) {
		return ""
	}
	s := strings.TrimSpace(lines[row-1])
	if len(s) > 140 {
		s = s[:140]
	}
	return s
}

// shortName: names this short are also ordinary words (a, id, go), so prose needs code context.
// Upper-case short names (U, K, V, ID, F) are not words, except the English "A" and "I", so they are
// searched like any other name.
func shortName(name string) bool {
	if len(name) >= 3 {
		return false
	}
	if name == "A" || name == "I" {
		return true
	}
	for _, r := range name {
		if r < 'A' || r > 'Z' {
			return true
		}
	}
	return false
}

// codeContext matches name written as code in prose: `name`, [name], name( or .name
func codeContext(name string) *regexp.Regexp {
	q := regexp.QuoteMeta(name)
	return regexp.MustCompile("`" + q + "`|\\[" + q + "\\]|(?:^|[^\\w])" + q + `\(|\.` + q + `(?:[^\w]|$)`)
}

// localIdents returns the byte offsets of the identifiers called name that the parser resolves to a
// declaration inside a function (a local variable, parameter, named result or local type): they are
// different objects than a package-level symbol of that name, which they shadow.
func localIdents(path, text, name string) map[int]bool {
	f, err := parser.ParseFile(token.NewFileSet(), path, text, 0)
	if f == nil || err != nil && len(f.Decls) == 0 {
		return nil
	}
	fset := token.NewFileSet()
	f, _ = parser.ParseFile(fset, path, text, 0)
	type span struct{ a, b token.Pos }
	var funcs []span
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			funcs = append(funcs, span{fd.Pos(), fd.End()})
		}
	}
	local := map[int]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name || id.Obj == nil || !id.Obj.Pos().IsValid() {
			return true
		}
		if id.Obj.Kind == ast.Typ {
			if _, isSpec := id.Obj.Decl.(*ast.TypeSpec); !isSpec {
				return true // a type parameter keeps being listed
			}
		}
		for _, sp := range funcs {
			if id.Obj.Pos() >= sp.a && id.Obj.Pos() < sp.b {
				local[fset.Position(id.Pos()).Offset] = true
				break
			}
		}
		return true
	})
	return local
}

func goMentions(path, rel, name string, word, prose *regexp.Regexp, text string, codeIn func(path, text string) bool, skipLocals bool) []Mention {
	lines := strings.Split(text, "\n")
	var locals map[int]bool
	if skipLocals {
		locals = localIdents(path, text, name)
	}
	var found []Mention
	// A name of one or two letters is mostly prose elsewhere, but inside the package of the renamed symbol
	// a whole-word mention in a string or comment is likely about it.
	inScope := shortName(name) && codeIn != nil && codeIn(path, text)
	// A name this short also names type parameters and locals of unrelated generic code in other
	// packages: their comments and strings are not about the renamed symbol.
	outOfScope := len(name) < 3 && codeIn != nil && !codeIn(path, text)
	// A file that cannot refer to the symbol but declares a name of its own: its comments and strings talk
	// about that declaration ("// Map maps T to U" above another package's Map).
	if !outOfScope && codeIn != nil && !codeIn(path, text) && declaresName(path, text, name) {
		outOfScope = true
	}
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(text))
	var sc scanner.Scanner
	sc.Init(file, []byte(text), func(token.Position, string) {}, scanner.ScanComments)
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		p := fset.PositionFor(pos, false) // not the //line-adjusted position: the lines are this file's own
		switch {
		case tok == token.IDENT && lit == name:
			if codeIn != nil && !codeIn(path, text) {
				continue
			}
			if locals[p.Offset] {
				continue // a local variable or parameter that shadows the symbol: another object
			}
			found = append(found, Mention{rel, p.Line, p.Column, "code", snippet(lines, p.Line), false})
		case outOfScope && (tok == token.COMMENT || tok == token.STRING || tok == token.CHAR):
			continue
		case tok == token.COMMENT && (prose.MatchString(lit) || inScope && word.MatchString(lit)):
			found = append(found, Mention{rel, p.Line, 0, "comment", snippet(lines, p.Line), !prose.MatchString(lit)})
		case (tok == token.STRING || tok == token.CHAR) && word.MatchString(lit):
			loose := false
			if shortName(name) {
				if v, err := strconv.Unquote(lit); err != nil || v != name {
					if !inScope {
						continue
					}
					loose = true
				}
			}
			if tagLike(lit) && !tagNames(lit, name) {
				continue // another field's struct tag
			}
			found = append(found, Mention{rel, p.Line, 0, "string", snippet(lines, p.Line), loose})
		}
	}
	return found
}

// declaresName: the Go file declares a top-level func, type, var or const, or a method, called name.
func declaresName(path, text, name string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), path, text, parser.SkipObjectResolution)
	if f == nil || err != nil && len(f.Decls) == 0 {
		return false
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				return true
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					if sp.Name.Name == name {
						return true
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if n.Name == name {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// Find lists occurrences of name that remain once the rename is applied: Go identifiers, comments,
// strings, and docs. after maps an absolute path to its text after the rename; other files are
// read from disk. only, when set, restricts the scan to that file (parameters and locals).
func Find(root, name string, after map[string]string, only string) []Mention {
	return FindScoped(root, name, after, only, nil)
}

// FindScoped is Find with code mentions limited to the Go files codeIn accepts (the files that could
// refer to the renamed symbol); comments, strings and docs are still searched everywhere.
func FindScoped(root, name string, after map[string]string, only string, codeIn func(path, text string) bool) []Mention {
	found, _ := FindScopedLimited(root, name, after, only, codeIn)
	return found
}

// FindScopedLimited is FindScoped that also reports whether the scan stopped at maxFiles files: the rest of the tree
// was not searched.
func FindScopedLimited(root, name string, after map[string]string, only string, codeIn func(path, text string) bool) ([]Mention, bool) {
	word := regexp.MustCompile(`(?:^|[^\w])` + regexp.QuoteMeta(name) + `(?:[^\w]|$)`)
	prose := word // what counts in comments and text files
	if shortName(name) {
		prose = codeContext(name)
	}
	var found []Mention
	seen := 0
	truncated := false
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if only != "" && !d.IsDir() && p != only {
			return nil
		}
		if d.IsDir() {
			if p != root {
				if exclude.IsDirName(d.Name()) {
					return filepath.SkipDir
				}
				if _, serr := os.Stat(filepath.Join(p, ".git")); serr == nil {
					return filepath.SkipDir // another checkout or linked worktree
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // a FIFO, a device or a symlink: reading it can block or leave the tree
		}
		ext := strings.ToLower(filepath.Ext(p))
		isGo := ext == ".go"
		if !isGo && !textSuffixes[ext] {
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > maxBytes {
			return nil
		}
		if seen++; seen > maxFiles {
			truncated = true
			return filepath.SkipAll
		}
		text, ok := after[p]
		if !ok {
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			text, _ = edits.DecodeSource(raw)
		}
		if !strings.Contains(text, name) {
			return nil
		}
		rel := edits.RelativeName(p, root)
		if isGo {
			found = append(found, goMentions(p, rel, name, word, prose, text, codeIn, only == "")...)
			return nil
		}
		for i, line := range strings.Split(text, "\n") {
			if prose.MatchString(line) {
				found = append(found, Mention{rel, i + 1, 0, "text", snippet([]string{line}, 1), false})
			}
		}
		return nil
	})
	order := map[string]int{"code": 0, "string": 1, "comment": 2, "text": 3}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] < order[b.Kind]
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	// One entry per line and kind: `U` twice in `Map[T, U any]` is one place to look at.
	type key struct {
		path, kind string
		line       int
	}
	seenLine := map[key]bool{}
	uniq := found[:0]
	for _, m := range found {
		if k := (key{m.Path, m.Kind, m.Line}); !seenLine[k] {
			seenLine[k] = true
			uniq = append(uniq, m)
		}
	}
	const maxLoose = 5
	loose, kept := 0, uniq[:0:0]
	for _, m := range uniq {
		if m.loose {
			if loose++; loose > maxLoose {
				continue
			}
		}
		kept = append(kept, m)
	}
	return kept, truncated
}

// Notes renders one note per kind: a heading plus a few example lines.
func Notes(ms []Mention, old string) []string {
	headings := []struct{ kind, text string }{
		{"code", "`" + old + "` still appears as code the type checker did not link (check these)"},
		{"string", "`" + old + "` appears in strings (reflection, struct tags, messages)"},
		{"comment", "`" + old + "` appears in comments"},
		{"text", "`" + old + "` appears in non-Go files"},
	}
	const per = 6
	var notes []string
	for _, h := range headings {
		var group []Mention
		for _, m := range ms {
			if m.Kind == h.kind {
				group = append(group, m)
			}
		}
		if len(group) == 0 {
			continue
		}
		lines := []string{h.text + ": " + itoa(len(group))}
		for i, m := range group {
			if i == per {
				lines = append(lines, "    ... "+itoa(len(group)-per)+" more")
				break
			}
			lines = append(lines, "    "+m.Path+":"+itoa(m.Line)+": "+m.Snippet)
		}
		notes = append(notes, strings.Join(lines, "\n"))
	}
	return notes
}

func itoa(n int) string { return strconv.Itoa(n) }

var tagRE = regexp.MustCompile(`\w+:"([^"]*)"`)

// tagLike: a raw string made of key:"value" pairs, i.e. a struct tag.
func tagLike(lit string) bool {
	return strings.HasPrefix(lit, "`") && tagRE.MatchString(lit)
}

// tagNames: some value of the struct tag is name (ignoring case), as in `json:"name,omitempty"` for field Name.
func tagNames(lit, name string) bool {
	for _, m := range tagRE.FindAllStringSubmatch(lit, -1) {
		if strings.EqualFold(strings.Split(m[1], ",")[0], name) {
			return true
		}
	}
	return false
}
