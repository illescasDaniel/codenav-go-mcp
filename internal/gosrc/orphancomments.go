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
	if len(was) == 0 {
		return after
	}
	fset = token.NewFileSet()
	af, err := parser.ParseFile(fset, filename, after, parser.ParseComments)
	if af == nil || err != nil {
		return after
	}
	owned := map[*ast.CommentGroup]bool{}
	for _, d := range importDecls(af) {
		for _, sp := range d.Specs {
			s := sp.(*ast.ImportSpec)
			owned[s.Doc], owned[s.Comment] = true, true
		}
	}
	type span struct{ a, b int }
	var cut []span
	tf := fset.File(af.Pos())
	for _, d := range importDecls(af) {
		if !d.Lparen.IsValid() {
			continue
		}
		for _, g := range af.Comments {
			if g.Pos() < d.Lparen || g.End() > d.Rparen || owned[g] {
				continue
			}
			for _, c := range g.List {
				if !was[c.Text] {
					continue
				}
				a, b := tf.Offset(c.Pos()), tf.Offset(c.End())
				ls := strings.LastIndex(after[:a], "\n") + 1
				le := strings.Index(after[b:], "\n")
				if le < 0 {
					continue
				}
				le += b + 1
				if strings.TrimSpace(after[ls:a]) == "" && strings.TrimSpace(after[b:le]) == "" {
					cut = append(cut, span{ls, le})
				}
			}
		}
	}
	sort.Slice(cut, func(i, j int) bool { return cut[i].a > cut[j].a })
	for _, c := range cut {
		after = after[:c.a] + after[c.b:]
	}
	return after
}
