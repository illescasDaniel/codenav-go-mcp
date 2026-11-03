package lsp

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// DedupeDiagnostics merges diagnostics that report one problem twice, keeping the most severe at its
// own position. gopls does this for a redeclaring ":=": a compiler error (code NoNewVar) over the
// whole statement plus a nonewvars analyzer warning over the ":=" with the same text. Two entries are
// merged when their messages match (ignoring case, spacing and a trailing period) and they start on
// the same line or their ranges overlap (a multi-line statement starts the two on different lines).
// Before merging, cgo failure dumps are condensed (see CondenseCgo) with the file unknown.
func DedupeDiagnostics(items []Diagnostic) []Diagnostic {
	items = CondenseCgo("", items)
	rank := func(d Diagnostic) int {
		if d.Severity == 0 {
			return 99
		}
		return d.Severity
	}
	out := make([]Diagnostic, 0, len(items))
	keys := make([]string, 0, len(items))
next:
	for _, d := range items {
		k := diagKey(d.Message)
		for i, o := range out {
			if keys[i] == k && (o.Range.Start.Line == d.Range.Start.Line || overlaps(o.Range, d.Range)) {
				if rank(d) < rank(o) {
					out[i] = d
				}
				continue next
			}
		}
		out = append(out, d)
		keys = append(keys, k)
	}
	return out
}

func diagKey(msg string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Join(strings.Fields(msg), " ")), ".")
}

// cgoHeaderRE is the "# import/path" line go list puts first in a package failure; a cgo failure dump follows it.
var cgoHeaderRE = regexp.MustCompile(`^# \S+$`)

// cgoMarker is the line cgo prints before gcc's own report in a dump. Only a message that has it is a dump: a
// "cgo:" line alone does not qualify, since gopls' own positioned cgo errors carry one.
const cgoMarker = "full error output:"

// gccErrorRE is a compiler report line of the form "file:line:col: error: text" (or "fatal error").
var gccErrorRE = regexp.MustCompile(`^\s*(\S+?):(\d+):(\d+): ((?:fatal )?error: .*)$`)

// CondenseCgo turns the cgo failure dumps among items into the compiler's own errors. A dump is "# pkg", cgo's
// generated C prolog and wrapper code, "full error output:", then gcc's report; it used to be shown whole at 1:1.
// The prolog is dropped. When file (the workspace-relative path of the diagnosed Go file) is known, gcc's first error
// in that file gives the diagnostic its position, and the message becomes "cgo: <gcc error>" with the excerpt below
// it. Every other gcc error (foo.c, helper.h, another .go file) keeps its "file:line:col" in the message, and the
// position is never moved. With file "" (unknown to the caller) the position is never moved either. A dump gcc
// printed no error line for is replaced by "cgo could not be run on this package: <last line>". Anything else is
// returned unchanged.
func CondenseCgo(file string, items []Diagnostic) []Diagnostic {
	out := make([]Diagnostic, len(items))
	for i, d := range items {
		out[i] = condenseCgo(file, d)
	}
	return out
}

// belongsTo reports whether gcc's file name g is the Go file file. An unknown file ("") matches nothing: a
// dump is then never relocated to a Go file it might not be about.
func belongsTo(g, file string) bool {
	if file == "" {
		return false
	}
	g, f := path.Clean(g), path.Clean(file)
	return g == f || strings.HasSuffix(f, "/"+g) || strings.HasSuffix(g, "/"+f)
}

func condenseCgo(file string, d Diagnostic) Diagnostic {
	lines := strings.Split(d.Message, "\n")
	if len(lines) < 2 || !cgoHeaderRE.MatchString(lines[0]) {
		return d
	}
	rest := lines[1:]
	marker := -1
	for i, l := range rest {
		if strings.TrimSpace(l) == cgoMarker {
			marker = i
			break
		}
	}
	// A dump has the marker. gopls also sends a bare "# pkg" + gcc report for a broken header (no marker), but only
	// for a package-load failure: that is the "go list" source. Anything else is not cgo's.
	if marker < 0 && d.Source != "go list" {
		return d
	}
	// Up to the marker is cgo's generated code and is dropped; gcc's report follows it (all of it when there is none).
	report := rest
	if marker >= 0 {
		report = rest[marker+1:]
	}
	pick, mapped := -1, false
	for i, l := range report {
		m := gccErrorRE.FindStringSubmatch(l)
		if m != nil && belongsTo(m[1], file) {
			pick, mapped = i, true
			break
		}
	}
	if pick < 0 {
		for i, l := range report {
			if gccErrorRE.MatchString(l) {
				pick = i
				break
			}
		}
	}
	out := d
	if pick >= 0 {
		m := gccErrorRE.FindStringSubmatch(report[pick])
		msg := "cgo: " + m[4]
		if !mapped {
			msg = "cgo: " + m[1] + ":" + m[2] + ":" + m[3] + ": " + m[4]
		} else {
			line, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			pos := Position{Line: max(line-1, 0), Character: max(col-1, 0)}
			out.Range = Range{Start: pos, End: pos}
		}
		// The other lines of gcc's report are kept as they are: the ones before the picked error and its excerpt.
		if rest := strings.TrimRight(strings.Join(report[:pick], "\n")+"\n"+strings.Join(report[pick+1:], "\n"), " \t\r\n"); strings.TrimSpace(rest) != "" {
			msg += "\n" + strings.TrimLeft(rest, "\n")
		}
		out.Message = msg
		return out
	}
	if marker < 0 {
		return d // without the marker there is no dump to summarise: keep the message as gopls sent it
	}
	last := ""
	for _, l := range report {
		if t := strings.TrimSpace(l); t != "" {
			last = t
		}
	}
	if last == "" {
		return d
	}
	out.Message = "cgo could not be run on this package: " + last
	return out
}

func before(a, b Position) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Character < b.Character
}

// overlaps reports whether two ranges share a character (end exclusive); an empty range counts when
// it touches the other.
func overlaps(a, b Range) bool {
	if a.Start == a.End || b.Start == b.End {
		return !before(a.End, b.Start) && !before(b.End, a.Start)
	}
	return before(a.Start, b.End) && before(b.Start, a.End)
}
