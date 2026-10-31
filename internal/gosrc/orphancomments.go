package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// DropOrphanImportComments removes, from after, the comments that belonged to an import of before that
// no longer exists: the heading above it, a comment on its line (before or after it), the doc of an import
// declaration that disappeared altogether, and headings left with no import under them. Comments that
// head a kept import, or that stand apart (a blank line away) from the imports, are kept. Nothing is ever
// glued onto another line, and blank lines left behind are tidied. after is returned unchanged when
// either text does not parse.
func DropOrphanImportComments(filename, before, after string) string {
	return dropNewBlankBeforeHeading(dropOrphanImportComments(filename, before, after), before)
}

// dropNewBlankBeforeHeading removes a blank line inside an import block that the edit introduced right
// above a comment line: before, no blank line separated that comment from the import above it (the
// import between them, or its attached comment, was removed). Blank lines that were already there, and
// the ones goimports puts between groups of imports, stay.
func dropNewBlankBeforeHeading(after, before string) string {
	bl := strings.Split(before, "\n")
	al := strings.Split(after, "\n")
	inBlock := false
	var out []string
	for i, l := range al {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "import ("):
			inBlock = true
		case inBlock && l == ")":
			inBlock = false
		}
		if inBlock && t == "" && i > 0 && i+1 < len(al) && len(out) > 0 {
			prev, next := strings.TrimSpace(out[len(out)-1]), strings.TrimSpace(al[i+1])
			if prev != "" && (strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/*")) && adjacentInBefore(bl, prev, next) {
				continue
			}
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// adjacentInBefore: line prev precedes line next in lines with no blank line between them.
func adjacentInBefore(lines []string, prev, next string) bool {
	p := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == prev {
			p = i
			break
		}
	}
	if p < 0 {
		return false
	}
	for j := p + 1; j < len(lines); j++ {
		switch t := strings.TrimSpace(lines[j]); {
		case t == "":
			return false
		case t == next:
			return true
		}
	}
	return false
}

func dropOrphanImportComments(filename, before, after string) string {
	fset := token.NewFileSet()
	bf, err := parser.ParseFile(fset, filename, before, parser.ParseComments)
	if bf == nil || err != nil {
		return after
	}
	btf := fset.File(bf.Pos())
	lineOf := func(p token.Pos) int { return fset.Position(p).Line }

	// was: comment texts attached to an import in before.
	was := map[string]bool{}
	note := func(g *ast.CommentGroup) {
		if g != nil {
			for _, c := range g.List {
				was[c.Text] = true
			}
		}
	}
	bowned := map[*ast.CommentGroup]bool{}
	attachedTo := map[*ast.ImportSpec][]*ast.CommentGroup{}
	for _, d := range importDecls(bf) {
		note(d.Doc)
		bowned[d.Doc] = true
		for _, sp := range d.Specs {
			s := sp.(*ast.ImportSpec)
			note(s.Doc)
			note(s.Comment)
			bowned[s.Doc], bowned[s.Comment] = true, true
			attachedTo[s] = []*ast.CommentGroup{s.Doc, s.Comment}
			// a comment ending on the line the spec starts on: `/* c */ "fmt"`
			for _, g := range bf.Comments {
				if g.End() <= s.Pos() && lineOf(g.End()) == lineOf(s.Pos()) && g.Pos() > d.Pos() && !bowned[g] {
					note(g)
					attachedTo[s] = append(attachedTo[s], g)
				}
			}
		}
	}
	// removedText / keptText: the comments attached to imports that are gone / that stay (judged after
	// parsing the new text, below).
	removedText, keptText := map[string]bool{}, map[string]bool{}
	specKey := func(s *ast.ImportSpec) string {
		n := ""
		if s.Name != nil {
			n = s.Name.Name
		}
		return n + " " + s.Path.Value
	}
	// Unowned comments of the old block: glued to the line above (a heading is glued to what it heads;
	// a deliberate note is set apart by a blank line) and whether an import came after them.
	gluedBefore, followedBefore := map[string]bool{}, map[string]bool{}
	for _, d := range importDecls(bf) {
		if !d.Lparen.IsValid() {
			continue
		}
		for _, g := range bf.Comments {
			if g.Pos() > d.Lparen && g.End() < d.Rparen && !bowned[g] {
				gluedBefore[g.List[0].Text] = !blankBefore(before, btf.Offset(g.Pos()))
				for _, sp := range d.Specs {
					if sp.Pos() > g.End() {
						followedBefore[g.List[0].Text] = true
					}
				}
			}
		}
	}

	fset = token.NewFileSet()
	af, err := parser.ParseFile(fset, filename, after, parser.ParseComments)
	if af == nil || err != nil {
		return after
	}
	tf := fset.File(af.Pos())
	have := map[string]bool{}
	for _, sp := range af.Imports {
		have[specKey(sp)] = true
	}
	for _, d := range importDecls(bf) {
		for _, sp := range d.Specs {
			s := sp.(*ast.ImportSpec)
			into := keptText
			if !have[specKey(s)] {
				into = removedText
			}
			for _, g := range attachedTo[s] {
				if g != nil {
					for _, c := range g.List {
						into[c.Text] = true
					}
				}
			}
		}
	}
	// Headings that were never attached to an import (a comment glued to the closing parenthesis, or two
	// headings in a row) are only judged orphans when the block lost an import.
	lostImport := len(bf.Imports) > len(af.Imports)
	if len(was) == 0 && !lostImport {
		return after
	}
	owned := map[*ast.CommentGroup]bool{af.Doc: true}
	for _, d := range af.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			owned[d.Doc] = true
			if d.Tok == token.IMPORT {
				for _, sp := range d.Specs {
					owned[sp.(*ast.ImportSpec).Doc], owned[sp.(*ast.ImportSpec).Comment] = true, true
				}
			}
		case *ast.FuncDecl:
			owned[d.Doc] = true
		}
	}

	type span struct{ a, b int }
	var cand []span
	for _, d := range importDecls(af) {
		if !d.Lparen.IsValid() {
			continue
		}
		for gi, g := range af.Comments {
			if g.Pos() < d.Lparen || g.End() > d.Rparen {
				continue
			}
			if owned[g] {
				// Merged with the heading of a kept import: only the comments of removed imports go.
				for _, c := range g.List {
					if removedText[c.Text] && !keptText[c.Text] {
						cand = append(cand, span{tf.Offset(c.Pos()), tf.Offset(c.End())})
					}
				}
				continue
			}
			orphan := false
			if lostImport {
				specAfter, nextComment := false, false
				for _, sp := range d.Specs {
					if sp.Pos() > g.End() {
						specAfter = true
						break
					}
				}
				if gi+1 < len(af.Comments) && af.Comments[gi+1].Pos() < d.Rparen {
					nextComment = true
					for _, sp := range d.Specs {
						if sp.Pos() > g.End() && sp.Pos() < af.Comments[gi+1].Pos() {
							nextComment = false
						}
					}
				}
				// two headings in a row: the first lost its group; a comment glued to the closing
				// parenthesis, or a heading whose imports all went, heads nothing.
				t := g.List[0].Text
				orphan = nextComment || (!specAfter && (gluedBefore[t] || followedBefore[t]))
			}
			for _, c := range g.List {
				if orphan || was[c.Text] {
					cand = append(cand, span{tf.Offset(c.Pos()), tf.Offset(c.End())})
				}
			}
		}
	}
	if lostImport {
		// An import declaration that vanished altogether leaves its comments at file level, before the
		// first declaration that is not an import.
		limit := len(after)
		for _, d := range af.Decls {
			if g, ok := d.(*ast.GenDecl); !ok || g.Tok != token.IMPORT {
				limit = tf.Offset(d.Pos())
				if g, ok := d.(*ast.GenDecl); ok && g.Doc != nil {
					limit = tf.Offset(g.Doc.Pos())
				}
				if f, ok := d.(*ast.FuncDecl); ok && f.Doc != nil {
					limit = tf.Offset(f.Doc.Pos())
				}
				break
			}
		}
		inDecl := func(g *ast.CommentGroup) bool {
			for _, d := range importDecls(af) {
				if g.Pos() >= d.Pos() && g.End() <= d.End() {
					return true
				}
			}
			return false
		}
		for _, g := range af.Comments {
			if owned[g] || inDecl(g) || g.Pos() <= af.Name.End() || tf.Offset(g.End()) > limit {
				continue
			}
			for _, c := range g.List {
				if was[c.Text] {
					cand = append(cand, span{tf.Offset(c.Pos()), tf.Offset(c.End())})
				}
			}
		}
	}
	if len(cand) == 0 {
		return after
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].a > cand[j].a })
	// A comment is only cut when nothing but other cut comments (and, at most, the closing parenthesis)
	// shares its line.
	masked := []byte(after)
	for _, c := range cand {
		for i := c.a; i < c.b; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	lineBounds := func(src string, off int) (int, int) {
		ls := strings.LastIndex(src[:off], "\n") + 1
		le := strings.Index(src[off:], "\n")
		if le < 0 {
			return ls, len(src)
		}
		return ls, off + le + 1
	}
	// Line ranges holding the comments; ranges that touch (comments sharing a line) merge.
	var ranges []span
	for i := len(cand) - 1; i >= 0; i-- { // ascending
		ls, _ := lineBounds(string(masked), cand[i].a)
		_, le := lineBounds(string(masked), max(cand[i].a, cand[i].b-1))
		if n := len(ranges); n > 0 && ls < ranges[n-1].b {
			ranges[n-1].b = max(ranges[n-1].b, le)
			continue
		}
		ranges = append(ranges, span{ls, le})
	}
	for i := len(ranges) - 1; i >= 0; i-- {
		r := ranges[i]
		line := strings.TrimSpace(string(masked[r.a:r.b]))
		if line != "" && line != ")" {
			continue
		}
		keep := ""
		if line == ")" {
			keep = ")\n" // `/* heading */)`: only the comment goes, the parenthesis keeps its line
		}
		after = after[:r.a] + keep + after[r.b:]
	}
	return tidyImportBlanks(filename, after)
}

// blankBefore: the line above the one holding offset off is blank (or off is on the first line).
func blankBefore(src string, off int) bool {
	ls := strings.LastIndex(src[:off], "\n") + 1
	if ls == 0 {
		return true
	}
	prev := src[strings.LastIndex(src[:ls-1], "\n")+1 : ls-1]
	return strings.TrimSpace(prev) == ""
}

// tidyImportBlanks collapses runs of blank lines inside parenthesised import blocks to one and drops
// the ones right after "(" and right before ")".
func tidyImportBlanks(filename, src string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly|parser.ParseComments)
	if f == nil || err != nil {
		return src
	}
	tf := fset.File(f.Pos())
	decls := importDecls(f)
	for i := len(decls) - 1; i >= 0; i-- {
		d := decls[i]
		if !d.Lparen.IsValid() {
			continue
		}
		a, b := tf.Offset(d.Lparen), tf.Offset(d.Rparen)
		lines := strings.SplitAfter(src[a:b], "\n")
		var out []string
		for j, l := range lines {
			if j > 0 && j < len(lines)-1 && strings.TrimSpace(l) == "" {
				if len(out) == 1 || (len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "") {
					continue // after "(" or after another blank line
				}
			}
			out = append(out, l)
		}
		// blank lines directly before the closing parenthesis
		for len(out) > 2 && strings.TrimSpace(out[len(out)-2]) == "" && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[: len(out)-2 : len(out)-2]
			out = append(out, lines[len(lines)-1])
		}
		src = src[:a] + strings.Join(out, "") + src[b:]
	}
	return src
}
