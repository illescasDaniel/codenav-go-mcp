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
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
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

// Covered holds, per absolute path, the (line, UTF-16 character) starts the rename already edits.
type Covered map[string]map[[2]int]bool

func CoveredBy(by map[string][]lsp.TextEdit) Covered {
	c := Covered{}
	for p, es := range by {
		m := map[[2]int]bool{}
		for _, e := range es {
			m[[2]int{e.Range.Start.Line, e.Range.Start.Character}] = true
		}
		c[p] = m
	}
	return c
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

func goMentions(path, rel, name string, word *regexp.Regexp, covered map[[2]int]bool, text string) []Mention {
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
			line0 := p.Line - 1
			char := 0
			if line0 < len(lines) && p.Column-1 <= len(lines[line0]) {
				char = lsp.Utf16Len(lines[line0][:p.Column-1])
			}
			if !covered[[2]int{line0, char}] {
				found = append(found, Mention{rel, p.Line, "code", snippet(lines, p.Line)})
			}
		case tok == token.COMMENT && word.MatchString(lit):
			found = append(found, Mention{rel, p.Line, "comment", snippet(lines, p.Line)})
		case (tok == token.STRING || tok == token.CHAR) && word.MatchString(lit):
			found = append(found, Mention{rel, p.Line, "string", snippet(lines, p.Line)})
		}
	}
	return found
}

// Find lists occurrences of name the rename did not edit: Go identifiers, comments, strings, and docs.
func Find(root, name string, covered Covered) []Mention {
	word := regexp.MustCompile(`(?:^|[^\w])` + regexp.QuoteMeta(name) + `(?:[^\w]|$)`)
	var found []Mention
	seen := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
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
		raw, rerr := os.ReadFile(p)
		if rerr != nil || !strings.Contains(string(raw), name) {
			return nil
		}
		text, _ := edits.DecodeSource(raw)
		rel := edits.RelativeName(p, root)
		if isGo {
			found = append(found, goMentions(p, rel, name, word, covered[p], text)...)
			return nil
		}
		for i, line := range strings.Split(text, "\n") {
			if word.MatchString(line) {
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
