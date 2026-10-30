// Package mentions finds places that still use the old name after a rename: what the type
// checker could not link (strings, comments, docs, dynamic uses).
package mentions

import (
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

const (
	maxFiles = 2000
	maxBytes = 512_000
)

var textSuffixes = map[string]bool{".md": true, ".rst": true, ".txt": true, ".toml": true, ".yml": true, ".yaml": true, ".json": true, ".cfg": true, ".ini": true}

type Mention struct {
	Path    string
	Line    int
	Kind    string // code | string | comment | text
	Snippet string
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
func shortName(name string) bool { return len(name) < 3 }

// codeContext matches name written as code in prose: `name`, [name], name( or .name
func codeContext(name string) *regexp.Regexp {
	q := regexp.QuoteMeta(name)
	return regexp.MustCompile("`" + q + "`|\\[" + q + "\\]|(?:^|[^\\w])" + q + `\(|\.` + q + `(?:[^\w]|$)`)
}

func goMentions(path, rel, name string, word, prose *regexp.Regexp, text string, codeIn func(path, text string) bool) []Mention {
	lines := strings.Split(text, "\n")
	var found []Mention
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(text))
	var sc scanner.Scanner
	sc.Init(file, []byte(text), func(token.Position, string) {}, scanner.ScanComments)
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		p := fset.Position(pos)
		switch {
		case tok == token.IDENT && lit == name:
			if codeIn != nil && !codeIn(path, text) {
				continue
			}
			found = append(found, Mention{rel, p.Line, "code", snippet(lines, p.Line)})
		case tok == token.COMMENT && prose.MatchString(lit):
			found = append(found, Mention{rel, p.Line, "comment", snippet(lines, p.Line)})
		case (tok == token.STRING || tok == token.CHAR) && word.MatchString(lit):
			if shortName(name) {
				if v, err := strconv.Unquote(lit); err != nil || v != name {
					continue
				}
			}
			found = append(found, Mention{rel, p.Line, "string", snippet(lines, p.Line)})
		}
	}
	return found
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
	word := regexp.MustCompile(`(?:^|[^\w])` + regexp.QuoteMeta(name) + `(?:[^\w]|$)`)
	prose := word // what counts in comments and text files
	if shortName(name) {
		prose = codeContext(name)
	}
	var found []Mention
	seen := 0
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
		ext := strings.ToLower(filepath.Ext(p))
		isGo := ext == ".go"
		if !isGo && !textSuffixes[ext] {
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > maxBytes {
			return nil
		}
		if seen++; seen > maxFiles {
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
			found = append(found, goMentions(p, rel, name, word, prose, text, codeIn)...)
			return nil
		}
		for i, line := range strings.Split(text, "\n") {
			if prose.MatchString(line) {
				found = append(found, Mention{rel, i + 1, "text", snippet([]string{line}, 1)})
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
	return found
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
