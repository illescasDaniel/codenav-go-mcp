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
	ErrorSeverity   = 1
	WarningSeverity = 2
	HintSeverity    = 4
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

// pkgConflictRE is go/packages' complaint about a directory holding two package names; it ends with the
// absolute directory, repeated by every file of it.
var pkgConflictRE = regexp.MustCompile(`^found packages (\w+) \(([^)]+)\) and (\w+) \(([^)]+)\) in \S+$`)

const pkgConflictPrefix = "the files of this directory declare different packages: "

// friendlyMessage rewrites the diagnostics whose raw text is unhelpful (absolute paths, repeated per file).
func friendlyMessage(msg string) string {
	if m := pkgConflictRE.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("%spackage %s (%s) and package %s (%s)", pkgConflictPrefix, m[1], m[2], m[3], m[4])
	}
	return msg
}

// FromLSP converts diagnostics of the file whose current text is text.
func FromLSP(rel, text string, items []lsp.Diagnostic) []Entry {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	items = lsp.DedupeDiagnostics(items)
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
		out = append(out, Entry{rel, l + 1, it.Range.Start.Character + 1, sev, code, friendlyMessage(it.Message), src})
	}
	return out
}

type Delta struct {
	New, Fixed   []Entry
	Unchanged    int
	FilesChecked int
	Unchecked    []string
	// Ignored describes diagnostics left out as analysis artefacts (see DropCgoArtefacts).
	Ignored []string
	// NotChecked gives the reasons the check is not complete (the Go configuration does not load, an edited package
	// does not load, no file was checked, ...). A delta with reasons says nothing clean, whatever it counted.
	NotChecked []string
	// Capped counts the edited and dependent files past the check limit, which were not type-checked.
	Capped int
	// DependentsOff is set when include_dependents=false: the packages importing the edited files were not
	// checked on purpose. Not a hold; the answer says so next to the count.
	DependentsOff bool
}

const cgoListFailure = "go list failed to return CompiledGoFiles"

// DropCgoArtefacts removes what gopls reports for a cgo package it cannot load from an in-memory copy
// (the package moved or was edited in an overlay): the CompiledGoFiles failure itself and the
// "undefined: C.x" errors of the same directory. They say nothing about the code, so they are left
// out of both sides of the comparison. It returns the kept entries and the files the dropped ones were in.
func DropCgoArtefacts(es []Entry) ([]Entry, []string) {
	dirs := map[string]bool{}
	for _, e := range es {
		if strings.Contains(e.Message, cgoListFailure) {
			dirs[dirOf(e.Path)] = true
		}
	}
	if len(dirs) == 0 {
		return es, nil
	}
	var out []Entry
	seen := map[string]bool{}
	var files []string
	for _, e := range es {
		if dirs[dirOf(e.Path)] && (strings.Contains(e.Message, cgoListFailure) || strings.HasPrefix(e.Message, "undefined: C.") || strings.HasPrefix(e.Message, "could not import C")) {
			if !seen[e.Path] {
				seen[e.Path] = true
				files = append(files, e.Path)
			}
			continue
		}
		out = append(out, e)
	}
	return out, files
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
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
	unchanged := len(after) - len(nw)
	less(nw)
	less(fixed)
	return Delta{New: dedupe(nw), Fixed: dedupe(fixed), Unchanged: unchanged}
}

// dedupe drops repeats of the same diagnostic (path, position, message) from a sorted list.
func dedupe(es []Entry) []Entry {
	seen := map[string]bool{}
	out := es[:0:0]
	for _, e := range es {
		k := fmt.Sprintf("%s|%d|%d|%s", e.Path, e.Line, e.Column, e.Message)
		if strings.HasPrefix(e.Message, pkgConflictPrefix) {
			k = dirOf(e.Path) + "|" + e.Message // every file of the directory reports the same conflict
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	return out
}

// NewCountText is the count of new errors as the answer words it: exact, a lower bound when files were left
// out by the check limit, and never a bare "0" in that case (the files left out may hold errors too).
func (d Delta) NewCountText() string {
	ne := len(d.NewErrors())
	switch {
	case d.Capped > 0 && ne == 0:
		return "0 new error(s) in the checked files"
	case d.Capped > 0:
		return fmt.Sprintf("at least %d new error(s)", ne)
	}
	return fmt.Sprintf("%d new error(s)", ne)
}

func Format(d Delta, limit int) string {
	ne, fe := len(d.NewErrors()), len(d.FixedErrors())
	head := fmt.Sprintf("Diagnostics: %s, %d fixed", d.NewCountText(), fe)
	if other := len(d.New) - ne; other > 0 {
		head += fmt.Sprintf(", %d new warning(s)", other)
	}
	// "unchanged" counts diagnostics, not files: the label says so.
	head += fmt.Sprintf(" (%d file(s) checked, %d diagnostic(s) unchanged", d.FilesChecked, d.Unchanged)
	if d.DependentsOff {
		head += "; importing packages NOT checked because include_dependents=false - run verify_changes(include_dependents=true) before relying on this"
	}
	head += ")"
	if len(d.NotChecked) > 0 {
		// Not a clean count: the check did not run where it had to, so the head says so instead of "0 new error(s)".
		head = "NOT CHECKED: " + strings.Join(d.NotChecked, "; ")
		if ne > 0 {
			head += fmt.Sprintf("\n  %d new error(s) in the files that were checked", ne)
		}
	}
	lines := []string{head}
	if len(d.Ignored) > 0 {
		lines = append(lines, "  ignored: "+strings.Join(d.Ignored, "; "))
	}
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
