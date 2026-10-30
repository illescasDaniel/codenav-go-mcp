package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// Insertion is text to insert at a byte offset of the source it was computed for.
type Insertion struct {
	Offset int
	Text   string
}

// MarkUsed returns the insertions that silence "declared and not used" for the local variable whose
// declaring identifier starts at byte offset off: `_ = name` right after the declaring statement, or,
// when the variable is bound by an if/for/switch header, a range clause, a select case or a type
// switch, as the first statement of the body (of every clause for a switch). Only that one
// declaration is touched, so a shadowed variable of the same name elsewhere is left alone. Text
// uses "\n" and tabs; callers convert line endings.
func MarkUsed(src string, off int, name string) ([]Insertion, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, toolerr.Inputf("not valid Go: %v", err)
	}
	tf := fset.File(f.Pos())
	if off < 0 || off+len(name) > tf.Size() {
		return nil, toolerr.Inputf("no identifier at offset %d", off)
	}
	pos := tf.Pos(off)
	path, _ := astutil.PathEnclosingInterval(f, pos, pos+token.Pos(len(name)))
	if len(path) == 0 {
		return nil, toolerr.Inputf("no identifier at offset %d", off)
	}
	if id, ok := path[0].(*ast.Ident); !ok || id.Name != name || id.Pos() != pos {
		return nil, toolerr.Inputf("%s is not the declaring identifier", name)
	}
	m := &marker{src: src, tf: tf, name: name}
	use := "_ = " + name
	for i := 1; i < len(path); i++ {
		switch n := path[i].(type) {
		case *ast.FuncType, *ast.FieldList:
			return nil, toolerr.Inputf("%s is a parameter or result, not a local variable declaration", name)
		case *ast.RangeStmt:
			if n.Tok != token.DEFINE || n.Body == nil {
				return nil, toolerr.Inputf("%s is not declared by this range clause", name)
			}
			return m.first(n.Body.List, n.Body.Lbrace, n.Body.Rbrace, use), nil
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				return nil, toolerr.Inputf("%s is not declared here", name)
			}
			return m.afterOrIn(n, path[i+1:], use)
		case *ast.DeclStmt:
			return m.afterOrIn(n, path[i+1:], use)
		case ast.Stmt:
			return nil, toolerr.Inputf("%s is not declared by a statement of its own", name)
		}
	}
	return nil, toolerr.Inputf("cannot find the statement declaring %s", name)
}

type marker struct {
	src  string
	tf   *token.File
	name string
}

func (m *marker) off(p token.Pos) int { return m.tf.Offset(p) }

// afterOrIn places the use for the declaring statement stmt, whose ancestors (innermost first) are up.
func (m *marker) afterOrIn(stmt ast.Stmt, up []ast.Node, use string) ([]Insertion, error) {
	for len(up) > 0 {
		l, ok := up[0].(*ast.LabeledStmt)
		if !ok {
			break
		}
		stmt, up = l, up[1:]
	}
	if len(up) == 0 {
		return nil, toolerr.Inputf("cannot find where the statement declaring %s sits", m.name)
	}
	switch p := up[0].(type) {
	case *ast.BlockStmt:
		return []Insertion{m.after(stmt, use)}, nil
	case *ast.CaseClause:
		if p.Body != nil && containsStmt(p.Body, stmt) {
			return []Insertion{m.after(stmt, use)}, nil
		}
	case *ast.CommClause:
		if containsStmt(p.Body, stmt) {
			return []Insertion{m.after(stmt, use)}, nil
		}
		return m.first(p.Body, p.Colon, token.NoPos, use), nil
	case *ast.IfStmt:
		return m.first(p.Body.List, p.Body.Lbrace, p.Body.Rbrace, use), nil
	case *ast.ForStmt:
		return m.first(p.Body.List, p.Body.Lbrace, p.Body.Rbrace, use), nil
	case *ast.SwitchStmt:
		return m.eachClause(p.Body, use)
	case *ast.TypeSwitchStmt:
		return m.eachClause(p.Body, use)
	}
	return nil, toolerr.Inputf("cannot place `%s` for this declaration of %s", use, m.name)
}

func containsStmt(list []ast.Stmt, s ast.Stmt) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (m *marker) eachClause(body *ast.BlockStmt, use string) ([]Insertion, error) {
	var out []Insertion
	for _, s := range body.List {
		if c, ok := s.(*ast.CaseClause); ok {
			out = append(out, m.first(c.Body, c.Colon, token.NoPos, use)...)
		}
	}
	if len(out) == 0 {
		return nil, toolerr.Inputf("the switch has no clause to hold `%s`", use)
	}
	return out, nil
}

// lineIndent is the leading blank run of the line holding byte offset o.
func (m *marker) lineIndent(o int) string {
	start := strings.LastIndexByte(m.src[:o], '\n') + 1
	end := start
	for end < len(m.src) && (m.src[end] == ' ' || m.src[end] == '\t') {
		end++
	}
	return m.src[start:end]
}

// lineEnd is the offset where the line holding o ends, before its "\n" or "\r\n".
func (m *marker) lineEnd(o int) int {
	i := strings.IndexByte(m.src[o:], '\n')
	if i < 0 {
		return len(m.src)
	}
	e := o + i
	if e > 0 && m.src[e-1] == '\r' {
		e--
	}
	return e
}

// restBlank: from o to the end of its line there is nothing but blanks and perhaps a // comment.
func (m *marker) restBlank(o int) bool {
	rest := strings.TrimSpace(m.src[o:m.lineEnd(o)])
	return rest == "" || strings.HasPrefix(rest, "//")
}

// after puts the use on a line of its own below stmt (after its trailing comment), or behind a ";"
// when other code follows on the statement's last line.
func (m *marker) after(stmt ast.Stmt, use string) Insertion {
	end := m.off(stmt.End())
	if m.restBlank(end) {
		return Insertion{m.lineEnd(end), "\n" + m.lineIndent(m.off(stmt.Pos())) + use}
	}
	return Insertion{end, "; " + use}
}

// first puts the use before the first statement of a body or clause; open is its "{" or ":" and
// closeBrace its "}" (NoPos for a clause).
func (m *marker) first(list []ast.Stmt, open, closeBrace token.Pos, use string) []Insertion {
	oo := m.off(open)
	if len(list) > 0 {
		so := m.off(list[0].Pos())
		if m.tf.Line(list[0].Pos()) == m.tf.Line(open) {
			return []Insertion{{so, use + "; "}}
		}
		return []Insertion{{so, use + "\n" + m.lineIndent(so)}}
	}
	ind := m.lineIndent(oo)
	if closeBrace.IsValid() && m.tf.Line(closeBrace) == m.tf.Line(open) {
		return []Insertion{{oo + 1, "\n" + ind + "\t" + use + "\n" + ind}}
	}
	if m.restBlank(oo + 1) {
		return []Insertion{{m.lineEnd(oo), "\n" + ind + "\t" + use}}
	}
	return []Insertion{{oo + 1, " " + use}}
}
