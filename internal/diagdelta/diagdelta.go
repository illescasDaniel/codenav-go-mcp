// Package diagdelta reports what an edit does to diagnostics: new problems, fixed ones, the rest.
// Diagnostics are compared by message and source line, not line number, so a shifted file
// does not turn every existing problem into "one fixed, one new".
package diagdelta

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

const (
	ErrorSeverity = 1
	HintSeverity  = 4
)

var labels = map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}

type Entry struct {
	Path         string
	Line, Column int
	Severity     int
	Code         string
	Message      string
	SourceLine   string
}

func (e Entry) IsError() bool { return e.Severity == ErrorSeverity }

func (e Entry) strict() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", e.Path, e.Code, e.Severity, e.Message, e.SourceLine)
}
func (e Entry) loose() string {
	return fmt.Sprintf("%s|%s|%d|%s", e.Path, e.Code, e.Severity, e.Message)
}

var (
	lineRefRE = regexp.MustCompile(`\bline \d+`)
	posRefRE  = regexp.MustCompile(`\.go:\d+(?::\d+)?`)
	quotedRE  = regexp.MustCompile("\"[^\"\n]*\"|`[^`\n]*`")
)

// mask hides what moves or renames change in a message: line numbers and quoted names.
func mask(msg string) string {
	msg = lineRefRE.ReplaceAllString(msg, "line N")
	msg = posRefRE.ReplaceAllString(msg, ".go:N")
	return quotedRE.ReplaceAllString(msg, "Q")
}

// Errors are never paired by the masked keys: offsetting a new error against a fixed one that
// merely looks alike could let it slip past max_new_errors.
func (e Entry) maskedInFile() string {
	if e.IsError() {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%s", e.Path, e.Code, e.Severity, mask(e.Message))
}
func (e Entry) maskedAnywhere() string {
	if e.IsError() {
		return ""
	}
	return fmt.Sprintf("%s|%d|%s", e.Code, e.Severity, mask(e.Message))
}

func (e Entry) Render() string {
	code := ""
	if e.Code != "" {
		code = " [" + e.Code + "]"
	}
	msg, _, _ := strings.Cut(e.Message, "\n")
	return fmt.Sprintf("%s:%d:%d %s%s: %s", e.Path, e.Line, e.Column, labels[e.Severity], code, msg)
}

// FromLSP converts diagnostics of the file whose current text is text.
func FromLSP(rel, text string, items []lsp.Diagnostic) []Entry {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		l := it.Range.Start.Line
		src := ""
		if l >= 0 && l < len(lines) {
			src = strings.TrimSpace(lines[l])
		}
		sev := it.Severity
		if sev == 0 {
			sev = ErrorSeverity
		}
		code := strings.Trim(string(it.Code), `"`)
		if code == "null" {
			code = ""
		}
		out = append(out, Entry{rel, l + 1, it.Range.Start.Character + 1, sev, code, it.Message, src})
	}
	return out
}

type Delta struct {
	New, Fixed   []Entry
	Unchanged    int
	FilesChecked int
	Unchecked    []string
}

func (d Delta) NewErrors() []Entry   { return filter(d.New, true) }
func (d Delta) FixedErrors() []Entry { return filter(d.Fixed, true) }
func (d Delta) IsClean() bool        { return len(d.NewErrors()) == 0 }

func filter(es []Entry, errorsOnly bool) []Entry {
	var out []Entry
	for _, e := range es {
		if e.IsError() == errorsOnly {
			out = append(out, e)
		}
	}
	return out
}

func subtract(left, right []Entry, key func(Entry) string) (ul, ur []Entry) {
	pool := map[string][]Entry{}
	var order []string
	for _, e := range right {
		k := key(e)
		if k == "" {
			ur = append(ur, e)
			continue
		}
		if _, ok := pool[k]; !ok {
			order = append(order, k)
		}
		pool[k] = append(pool[k], e)
	}
	for _, e := range left {
		k := key(e)
		if p := pool[k]; k != "" && len(p) > 0 {
			pool[k] = p[:len(p)-1]
		} else {
			ul = append(ul, e)
		}
	}
	for _, k := range order {
		ur = append(ur, pool[k]...)
	}
	return
}

// Diff pairs identical diagnostics, then pairs what is left by message alone, then warnings and
// hints by their message with line numbers and quoted names masked (first within a file, then
// across files, so a move or rename does not show the same diagnostic as fixed and new).
func Diff(before, after []Entry) Delta {
	nw, fixed := subtract(after, before, Entry.strict)
	nw, fixed = subtract(nw, fixed, Entry.loose)
	nw, fixed = subtract(nw, fixed, Entry.maskedInFile)
	nw, fixed = subtract(nw, fixed, Entry.maskedAnywhere)
	less := func(s []Entry) {
		sort.SliceStable(s, func(i, j int) bool {
			a, b := s[i], s[j]
			if a.Path != b.Path {
				return a.Path < b.Path
			}
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.Column < b.Column
		})
	}
	less(nw)
	less(fixed)
	return Delta{New: nw, Fixed: fixed, Unchanged: len(after) - len(nw)}
}

func Format(d Delta, limit int) string {
	ne, fe := len(d.NewErrors()), len(d.FixedErrors())
	head := fmt.Sprintf("Diagnostics: %d new error(s), %d fixed", ne, fe)
	if other := len(d.New) - ne; other > 0 {
		head += fmt.Sprintf(", %d new warning(s)/hint(s)", other)
	}
	head += fmt.Sprintf(" (%d file(s) checked, %d unchanged)", d.FilesChecked, d.Unchanged)
	lines := []string{head}
	if len(d.Unchecked) > 0 {
		lines = append(lines, "  not checked: "+strings.Join(d.Unchecked[:min(5, len(d.Unchecked))], ", "))
	}
	if len(d.New) > 0 {
		lines = append(lines, "New:")
		for _, e := range d.New[:min(limit, len(d.New))] {
			lines = append(lines, "  + "+e.Render())
		}
		if len(d.New) > limit {
			lines = append(lines, fmt.Sprintf("  ... %d more", len(d.New)-limit))
		}
	}
	if len(d.Fixed) > 0 {
		n := max(3, limit/3)
		lines = append(lines, "Fixed:")
		for _, e := range d.Fixed[:min(n, len(d.Fixed))] {
			lines = append(lines, "  - "+e.Render())
		}
		if len(d.Fixed) > n {
			lines = append(lines, fmt.Sprintf("  ... %d more", len(d.Fixed)-n))
		}
	}
	return strings.Join(lines, "\n")
}
