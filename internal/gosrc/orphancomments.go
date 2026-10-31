package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// DropOrphanImportComments removes, from the import blocks of after, the comments that were the
// heading or trailing comment of an import in before and that no import owns any more (the import
// was removed and the comment stayed behind, as organize-imports leaves it). Other comments are kept.
// after is returned unchanged when either text does not parse.
func DropOrphanImportComments(filename, before, after string) string {
	attached := func(f *ast.File) map[string]bool {
		m := map[string]bool{}
		for _, d := range importDecls(f) {
			for _, sp := range d.Specs {
				s := sp.(*ast.ImportSpec)
				for _, g := range []*ast.CommentGroup{s.Doc, s.Comment} {
					if g != nil {
						for _, c := range g.List {
							m[c.Text] = true
						}
					}
				}
			}
		}
		return m
	}
	fset := token.NewFileSet()
	bf, err := parser.ParseFile(fset, filename, before, parser.ParseComments)
	if bf == nil || err != nil {
		return after
	}
	was := attached(bf)
	fset = token.NewFileSet()
	af, err := parser.ParseFile(fset, filename, after, parser.ParseComments)
	if af == nil || err != nil {
		return after
	}
	// Headings that were never attached to an import (a comment glued to the closing parenthesis, or two
	// headings in a row) are only judged orphans when the block lost an import.
	lostImport := len(bf.Imports) > len(af.Imports)
	if len(was) == 0 && !lostImport {
		return after
	}
	owned := map[*ast.CommentGroup]bool{}
	for _, d := range importDecls(af) {
		for _, sp := range d.Specs {
			s := sp.(*ast.ImportSpec)
			owned[s.Doc], owned[s.Comment] = true, true
		}
	}
	// Unowned comments of the old text that sat right under the line above them (no blank line): a
	// deliberate note is set apart by a blank line, a heading is glued to what it headed.
	gluedBefore := map[string]bool{}
	{
		bset := token.NewFileSet()
		if bf2, err := parser.ParseFile(bset, filename, before, parser.ParseComments); err == nil {
			btf := bset.File(bf2.Pos())
			bowned := map[*ast.CommentGroup]bool{}
			for _, d := range importDecls(bf2) {
				for _, sp := range d.Specs {
					bowned[sp.(*ast.ImportSpec).Doc], bowned[sp.(*ast.ImportSpec).Comment] = true, true
				}
			}
			for _, d := range importDecls(bf2) {
				for _, g := range bf2.Comments {
					if d.Lparen.IsValid() && g.Pos() > d.Lparen && g.End() < d.Rparen && !bowned[g] {
						gluedBefore[g.List[0].Text] = !blankBefore(before, btf.Offset(g.Pos()))
					}
				}
			}
		}
	}
	type span struct{ a, b int }
	var cut []span
	tf := fset.File(af.Pos())
	for _, d := range importDecls(af) {
		if !d.Lparen.IsValid() {
			continue
		}
		for gi, g := range af.Comments {
			if g.Pos() < d.Lparen || g.End() > d.Rparen || owned[g] {
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
				glued := gluedBefore[g.List[0].Text]
				// two headings in a row: the first lost its group; a comment glued to the closing
				// parenthesis heads nothing.
				orphan = nextComment || (!specAfter && glued)
			}
			for _, c := range g.List {
				if !orphan && !was[c.Text] {
					continue
				}
				a, b := tf.Offset(c.Pos()), tf.Offset(c.End())
				ls := strings.LastIndex(after[:a], "\n") + 1
				if strings.TrimSpace(after[ls:a]) != "" {
					continue
				}
				le := strings.Index(after[b:], "\n")
				if le < 0 {
					continue
				}
				le += b + 1
				switch rest := strings.TrimSpace(after[b:le]); rest {
				case "":
					cut = append(cut, span{ls, le})
				case ")":
					// `/* heading */)`: only the comment goes, the parenthesis keeps its line.
					cut = append(cut, span{ls, b + strings.Index(after[b:le], ")")})
				}
			}
		}
	}
	sort.Slice(cut, func(i, j int) bool { return cut[i].a > cut[j].a })
	for _, c := range cut {
		after = after[:c.a] + after[c.b:]
	}
	if len(cut) > 0 {
		after = tidyImportBlanks(filename, after)
	}
	return after
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
