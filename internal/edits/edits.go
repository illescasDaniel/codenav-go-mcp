// Package edits holds text-edit primitives for the write tools: LSP TextEdits (UTF-16, CRLF, BOM),
// whole-file changes, diffs and edit plans. Pure: no language server, no writes.
package edits

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

const bom = "\xef\xbb\xbf"

// DecodeSource splits a leading UTF-8 BOM from the text.
func DecodeSource(raw []byte) (text string, hasBOM bool) {
	s := string(raw)
	if strings.HasPrefix(s, bom) {
		return s[len(bom):], true
	}
	return s, false
}

func EncodeSource(text string, withBOM bool) []byte {
	if withBOM {
		return []byte(bom + text)
	}
	return []byte(text)
}

func ReadSource(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	t, b := DecodeSource(raw)
	return t, b, nil
}

func HashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// FileHash is the hash of a file's bytes, "" when it does not exist.
func FileHash(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return HashBytes(b)
}

func detectEOL(text string) string {
	crlf := strings.Count(text, "\r\n")
	if crlf > strings.Count(text, "\n")-crlf {
		return "\r\n"
	}
	return "\n"
}

func lineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			starts = append(starts, i+1)
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			starts = append(starts, i+1)
		}
	}
	return starts
}

// offset converts an LSP position (UTF-16 character) to a byte offset, clamped to the line.
func offset(text string, starts []int, line, character int) int {
	if line >= len(starts) {
		return len(text)
	}
	begin := starts[line]
	end := len(text)
	if line+1 < len(starts) {
		end = starts[line+1]
	}
	content := strings.TrimRight(text[begin:end], "\r\n")
	units := 0
	for i, r := range content {
		if units >= character {
			return begin + i
		}
		units += len(utf16.Encode([]rune{r}))
	}
	return begin + len(content)
}

type span struct {
	start, end, order int
	text              string
}

// ApplyTextEdits applies LSP edits (all positioned against the original text). Overlaps are an error.
func ApplyTextEdits(text string, es []lsp.TextEdit) (string, error) {
	if len(es) == 0 {
		return text, nil
	}
	eol := detectEOL(text)
	starts := lineStarts(text)
	spans := make([]span, len(es))
	for i, e := range es {
		s := offset(text, starts, e.Range.Start.Line, e.Range.Start.Character)
		t := offset(text, starts, e.Range.End.Line, e.Range.End.Character)
		if t < s {
			return "", toolerr.Inputf("edit range ends before it starts (line %d)", e.Range.Start.Line+1)
		}
		nt := strings.ReplaceAll(strings.ReplaceAll(e.NewText, "\r\n", "\n"), "\n", eol)
		spans[i] = span{s, t, i, nt}
	}
	sort.SliceStable(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.end != b.end {
			return a.end < b.end
		}
		return a.order < b.order
	})
	var sb strings.Builder
	cursor := 0
	for _, sp := range spans {
		if sp.start < cursor {
			return "", toolerr.Inputf("overlapping edits near line %d", strings.Count(text[:sp.start], "\n")+1)
		}
		sb.WriteString(text[cursor:sp.start])
		sb.WriteString(sp.text)
		cursor = sp.end
	}
	sb.WriteString(text[cursor:])
	return sb.String(), nil
}

// FileChange is a whole-file effect: Old nil creates the file, New nil deletes it.
type FileChange struct {
	Path           string
	Old, New       *string
	OldBOM, NewBOM bool
	// Notes are things worth telling about this file's change (e.g. an import that was listed twice).
	Notes []string
}

func (c FileChange) Kind() string {
	switch {
	case c.Old == nil:
		return "create"
	case c.New == nil:
		return "delete"
	}
	return "modify"
}

func (c FileChange) IsNoop() bool {
	return c.OldBOM == c.NewBOM && (c.Old == nil) == (c.New == nil) && (c.Old == nil || *c.Old == *c.New)
}

func (c FileChange) OldBytes() []byte {
	if c.Old == nil {
		return nil
	}
	return EncodeSource(*c.Old, c.OldBOM)
}

func (c FileChange) NewBytes() []byte {
	if c.New == nil {
		return nil
	}
	return EncodeSource(*c.New, c.NewBOM)
}

func (c FileChange) Inverse() FileChange {
	return FileChange{Path: c.Path, Old: c.New, New: c.Old, OldBOM: c.NewBOM, NewBOM: c.OldBOM}
}

func Str(s string) *string { return &s }

// ParseWorkspaceEdit returns text edits per absolute path; resource operations are refused.
func ParseWorkspaceEdit(we *lsp.WorkspaceEdit) (map[string][]lsp.TextEdit, error) {
	out := map[string][]lsp.TextEdit{}
	if we == nil {
		return out, nil
	}
	for uri, es := range we.Changes {
		p := lsp.URIToPath(uri)
		if p == "" {
			return nil, toolerr.Inputf("edit targets a non-file document: %s", uri)
		}
		out[p] = append(out[p], es...)
	}
	for _, raw := range we.DocumentChanges {
		var probe struct {
			Kind string `json:"kind"`
		}
		_ = jsonUnmarshal(raw, &probe)
		if probe.Kind != "" {
			return nil, toolerr.Inputf("the language server asked for a file operation (%s), not supported", probe.Kind)
		}
		var d lsp.TextDocumentEdit
		if err := jsonUnmarshal(raw, &d); err != nil {
			return nil, err
		}
		p := lsp.URIToPath(d.TextDocument.URI)
		if p == "" {
			return nil, toolerr.Inputf("edit targets a non-file document: %s", d.TextDocument.URI)
		}
		out[p] = append(out[p], d.Edits...)
	}
	return out, nil
}

// ChangesFromEdits applies per-file edits to what is on disk now; no-ops are dropped.
func ChangesFromEdits(by map[string][]lsp.TextEdit) ([]FileChange, error) {
	paths := make([]string, 0, len(by))
	for p := range by {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []FileChange
	for _, p := range paths {
		old, b, err := ReadSource(p)
		if err != nil {
			return nil, err
		}
		nt, err := ApplyTextEdits(old, by[p])
		if err != nil {
			return nil, err
		}
		c := FileChange{Path: p, Old: &old, New: &nt, OldBOM: b, NewBOM: b}
		if !c.IsNoop() {
			out = append(out, c)
		}
	}
	return out, nil
}

func RelativeName(path, root string) string {
	return exclude.RelativeTo(path, root)
}

// EditPlan is a set of whole-file changes previewed first and written later.
type EditPlan struct {
	Description string
	Changes     []FileChange
	Notes       []string
	BaseHashes  map[string]string // path -> hash of bytes at plan time ("" = absent)
	AllowLarge  bool              // allow_large was given when the plan was made; apply_edit honours it
	Previewed   bool              // kept in the plan store: a preview exists for it
}

func NewPlan(desc string, changes []FileChange, notes ...string) *EditPlan {
	p := &EditPlan{Description: desc, Changes: changes, Notes: notes, BaseHashes: map[string]string{}}
	for _, c := range changes {
		if old := c.OldBytes(); old != nil {
			p.BaseHashes[c.Path] = HashBytes(old)
		} else {
			p.BaseHashes[c.Path] = ""
		}
	}
	return p
}

func (p *EditPlan) Inverse() *EditPlan {
	inv := make([]FileChange, len(p.Changes))
	for i, c := range p.Changes {
		inv[len(inv)-1-i] = c.Inverse()
	}
	return NewPlan("undo: "+p.Description, inv)
}

func (p *EditPlan) Paths() []string {
	out := make([]string, len(p.Changes))
	for i, c := range p.Changes {
		out[i] = c.Path
	}
	return out
}

func splitLines(s *string) []string {
	if s == nil || *s == "" {
		return nil
	}
	// Lines keep their terminator, like git: a last line without "\n" compares unequal to the same
	// line with one, so adding or removing only the final newline still shows as a change, while a
	// file that ends in "\n" and gains lines after it diffs cleanly (no -}/+}).
	parts := strings.SplitAfter(*s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

const noNewlineMarker = "\\ No newline at end of file"

type op struct {
	kind byte // ' ', '-', '+'
	text string
}

// diffLines is an LCS line diff after trimming the common prefix and suffix.
func diffLines(a, b []string) []op {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	am, bm := a[p:len(a)-s], b[p:len(b)-s]
	var ops []op
	for _, l := range a[:p] {
		ops = append(ops, op{' ', l})
	}
	n, m := len(am), len(bm)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && am[i] == bm[j]:
			ops = append(ops, op{' ', am[i]})
			i++
			j++
		case j >= m || (i < n && lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', am[i]})
			i++
		default:
			ops = append(ops, op{'+', bm[j]})
			j++
		}
	}
	for _, l := range a[len(a)-s:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

// CountChanged returns (added, removed) line counts.
func CountChanged(c FileChange) (added, removed int) {
	for _, o := range diffLines(splitLines(c.Old), splitLines(c.New)) {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return
}

func UnifiedDiff(c FileChange, root string, context int) string {
	name := RelativeName(c.Path, root)
	from, to := "a/"+name, "b/"+name
	if c.Old == nil {
		from = "/dev/null"
	}
	if c.New == nil {
		to = "/dev/null"
	}
	ops := diffLines(splitLines(c.Old), splitLines(c.New))
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
	// Group ops into hunks separated by more than 2*context unchanged lines.
	oldNo, newNo := make([]int, len(ops)), make([]int, len(ops))
	o, n := 1, 1
	for i, x := range ops {
		oldNo[i], newNo[i] = o, n
		if x.kind != '+' {
			o++
		}
		if x.kind != '-' {
			n++
		}
	}
	i := 0
	for i < len(ops) {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i >= len(ops) {
			break
		}
		start := max(0, i-context)
		end := i
		last := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				last = end
			} else if end-last > 2*context {
				break
			}
			end++
		}
		end = min(len(ops), last+context+1)
		oc, nc := 0, 0
		for _, x := range ops[start:end] {
			if x.kind != '+' {
				oc++
			}
			if x.kind != '-' {
				nc++
			}
		}
		os, ns := oldNo[start], newNo[start]
		if oc == 0 {
			os--
		}
		if nc == 0 {
			ns--
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", os, oc, ns, nc)
		for _, x := range ops[start:end] {
			sb.WriteByte(x.kind)
			sb.WriteString(strings.TrimSuffix(x.text, "\n"))
			sb.WriteByte('\n')
			if !strings.HasSuffix(x.text, "\n") {
				sb.WriteString(noNewlineMarker + "\n")
			}
		}
		i = end
	}
	return sb.String()
}

func (p *EditPlan) Summary(root string) string {
	lines := make([]string, len(p.Changes))
	for i, c := range p.Changes {
		a, r := CountChanged(c)
		tag := map[string]string{"create": " (new file)", "delete": " (deleted)"}[c.Kind()]
		lines[i] = fmt.Sprintf("  %s%s: +%d -%d", RelativeName(c.Path, root), tag, a, r)
	}
	return strings.Join(lines, "\n")
}

func (p *EditPlan) Diff(root string, context, maxLines int) string {
	var sb strings.Builder
	for _, c := range p.Changes {
		sb.WriteString(UnifiedDiff(c, root, context))
	}
	lines := strings.Split(sb.String(), "\n")
	if len(lines) <= maxLines {
		return sb.String()
	}
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n... diff truncated (%d more lines)\n", len(lines)-maxLines)
}

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// MatchEOL converts text to the line ending style used by like.
func MatchEOL(text, like string) string {
	eol := detectEOL(like)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if eol == "\n" {
		return text
	}
	return strings.ReplaceAll(text, "\n", eol)
}

// RestoreEOL gives text (any line endings) the line endings of like, line by line when like mixes
// CRLF and LF: lines that a diff pairs with a line of like keep that line's terminator, the rest
// (added lines) get like's dominant ending. So a mixed
// file is not normalised and unchanged lines stay out of the diff.
func RestoreEOL(text, like string) string {
	crlf := strings.Count(like, "\r\n")
	if crlf == 0 || crlf == strings.Count(like, "\n") {
		return MatchEOL(text, like)
	}
	eol := detectEOL(like)
	oldLines := strings.SplitAfter(like, "\n")
	newLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// newLines[i] for i < len(newLines)-1 is followed by a newline; the last part never is.
	n := len(newLines) - 1
	ends := make([]string, n)
	term := func(l string) (content, end string) {
		if strings.HasSuffix(l, "\r\n") {
			return l[:len(l)-2], "\r\n"
		}
		if strings.HasSuffix(l, "\n") {
			return l[:len(l)-1], "\n"
		}
		return l, ""
	}
	var olds [][2]string
	for _, l := range oldLines {
		if c, e := term(l); e != "" {
			olds = append(olds, [2]string{c, e})
		}
	}
	// Align the lines of both texts (LCS) rather than by index, so a line gofmt added or removed in
	// the middle does not shift every later ending onto the wrong line. Lines the alignment pairs
	// keep their own ending; added lines keep the dominant one.
	oc := make([]string, len(olds))
	for i, o := range olds {
		oc[i] = o[0]
	}
	i, j := 0, 0
	var gone []int // indexes in olds of the lines removed since the last kept line
	for _, o := range diffLines(oc, newLines[:n]) {
		switch o.kind {
		case ' ':
			ends[j] = olds[i][1]
			i++
			j++
			gone = nil
		case '-':
			gone = append(gone, i)
			i++
		default:
			// A changed line takes the ending of the line it replaces.
			if len(gone) > 0 {
				ends[j] = olds[gone[0]][1]
				gone = gone[1:]
			}
			j++
		}
	}
	// An added line (no ending yet) takes the ending of its neighbour above, so inserted code looks
	// like the code around it; at the top of the file it takes the one below; the dominant ending is
	// only the last resort (a text with no ending of its own to copy).
	for k := 1; k < n; k++ {
		if ends[k] == "" {
			ends[k] = ends[k-1]
		}
	}
	for k := n - 2; k >= 0; k-- {
		if ends[k] == "" {
			ends[k] = ends[k+1]
		}
	}
	for k := range ends {
		if ends[k] == "" {
			ends[k] = eol
		}
	}
	var b strings.Builder
	for i, l := range newLines {
		b.WriteString(l)
		if i < n {
			b.WriteString(ends[i])
		}
	}
	return b.String()
}

// Offset converts a 0-based LSP position (UTF-16 character) to a byte offset in text.
func Offset(text string, line, character int) int {
	return offset(text, lineStarts(text), line, character)
}

// PositionOf is the inverse of Offset: the 0-based line and UTF-16 character of a byte offset.
func PositionOf(text string, off int) (line, character int) {
	if off > len(text) {
		off = len(text)
	}
	line = strings.Count(text[:off], "\n")
	begin := strings.LastIndex(text[:off], "\n") + 1
	return line, lsp.Utf16Len(text[begin:off])
}
