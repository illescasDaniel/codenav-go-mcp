package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// Replacement replaces src[Start:End] with Text (a pure deletion has empty Text).
type Replacement struct {
	Start, End int
	Text       string
}

// RemoveUnused returns the replacements that remove the unused local variable whose declaring
// identifier starts at byte offset off, keeping every side effect of its initializer:
//
//   - `x := expr` / `var x T = expr` with a side-effect-free expr (identifiers, literals, selectors,
//     composite literals and conversions of such, func literals): the statement is deleted.
//   - the same with a call or receive: it stays as an expression statement (`x := f()` -> `f()`); any
//     other expression with a call in it becomes `_ = expr`.
//   - `a, x := f()`: x becomes `_`, and `:=` becomes `=` when no new variable would be left on the left.
//   - members of a `var (...)` group are deleted from it (the group goes when it empties); `var a, x int`
//     loses the name; `var a, x = f()` gets a `_`.
//   - an if/for/switch initializer is dropped (or kept as an expression statement for a call), a range
//     variable is dropped or blanked, a type-switch binding or select-case receive loses its `v :=`.
//
// Parameters and results, labelled statements and anything else that cannot be removed without
// guessing give an error, so no fix is offered. The text is left to gofmt; callers convert line endings.
func RemoveUnused(src string, off int, name string) ([]Replacement, error) {
	fset := token.NewFileSet()
	// Object resolution stays on: it tells new variables of a := from redeclared ones.
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		return nil, toolerr.Inputf("not valid Go: %s", syntaxText(err))
	}
	tf := fset.File(f.Pos())
	if off < 0 || off+len(name) > tf.Size() {
		return nil, toolerr.Inputf("no identifier at offset %d", off)
	}
	pos := tf.Pos(off)
	path, _ := astutil.PathEnclosingInterval(f, pos, pos+token.Pos(len(name)))
	id, ok := ast.Node(nil), false
	if len(path) > 0 {
		id, ok = path[0], true
	}
	ident, isIdent := id.(*ast.Ident)
	if !ok || !isIdent || ident.Name != name || ident.Pos() != pos || len(path) < 3 {
		return nil, toolerr.Inputf("%s is not the declaring identifier", name)
	}
	r := &remover{src: src, tf: tf, name: name, id: ident}
	switch n := path[1].(type) {
	case *ast.AssignStmt:
		if n.Tok != token.DEFINE {
			return nil, toolerr.Inputf("%s is not declared here", name)
		}
		return r.assign(n, path[2])
	case *ast.ValueSpec:
		gd, ok := path[2].(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || len(path) < 4 {
			return nil, toolerr.Inputf("%s is not a local variable", name)
		}
		ds, ok := path[3].(*ast.DeclStmt)
		if !ok {
			return nil, toolerr.Inputf("%s is not a local variable", name)
		}
		return r.valueSpec(n, gd, ds)
	case *ast.RangeStmt:
		return r.rng(n)
	case *ast.Field, *ast.FieldList:
		return nil, toolerr.Inputf("%s is a parameter or result, not a local variable declaration", name)
	}
	return nil, toolerr.Inputf("%s is not a local variable declaration that can be removed", name)
}

type remover struct {
	src  string
	tf   *token.File
	name string
	id   *ast.Ident
}

func (r *remover) off(p token.Pos) int { return r.tf.Offset(p) }
func (r *remover) text(a, b ast.Node) string {
	return r.src[r.off(a.Pos()):r.off(b.End())]
}
func (r *remover) blank() Replacement {
	return Replacement{r.off(r.id.Pos()), r.off(r.id.End()), "_"}
}

// pureBuiltins are predeclared functions without side effects.
var pureBuiltins = map[string]bool{"len": true, "cap": true, "make": true, "new": true, "min": true, "max": true, "real": true, "imag": true, "complex": true}

// stmtBuiltins are the predeclared functions that cannot stand alone as a statement.
var noStmtBuiltins = map[string]bool{"append": true, "cap": true, "complex": true, "imag": true, "len": true, "make": true, "new": true, "real": true, "min": true, "max": true}

var predeclaredTypes = setOf("bool", "byte", "complex64", "complex128", "error", "float32", "float64", "int", "int8", "int16", "int32", "int64", "rune", "string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "any")

func setOf(s ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isTypeExpr: a syntactic type, as in the conversion []byte(s).
func isTypeExpr(e ast.Expr) bool {
	switch e := unparen(e).(type) {
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	case *ast.StarExpr:
		return isTypeExpr(e.X) || func() bool { id, ok := unparen(e.X).(*ast.Ident); return ok && predeclaredTypes[id.Name] }()
	case *ast.Ident:
		return predeclaredTypes[e.Name]
	}
	return false
}

// pure: evaluating e has no side effect (identifiers, literals, selectors, composite literals without
// calls, conversions and side-effect-free builtins of such).
func pure(e ast.Expr) bool {
	switch e := e.(type) {
	case nil:
		return true
	case *ast.Ident, *ast.BasicLit, *ast.FuncLit:
		return true
	case *ast.ParenExpr:
		return pure(e.X)
	case *ast.SelectorExpr:
		return pure(e.X)
	case *ast.UnaryExpr:
		return e.Op != token.ARROW && pure(e.X)
	case *ast.BinaryExpr:
		return pure(e.X) && pure(e.Y)
	case *ast.KeyValueExpr:
		return pure(e.Key) && pure(e.Value)
	case *ast.CompositeLit:
		for _, el := range e.Elts {
			if !pure(el) {
				return false
			}
		}
		return true
	case *ast.CallExpr:
		if e.Ellipsis.IsValid() {
			return false
		}
		fun := unparen(e.Fun)
		okFun := isTypeExpr(fun)
		if id, ok := fun.(*ast.Ident); ok && pureBuiltins[id.Name] {
			okFun = true
		}
		if !okFun {
			return false
		}
		for _, a := range e.Args {
			if !pure(a) && !isTypeExpr(a) {
				return false
			}
		}
		return true
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	}
	return false
}

// asStmt is the source of e as a statement that keeps its effects: e itself for a call or receive,
// `_ = e` for anything else.
func (r *remover) asStmt(e ast.Expr) string {
	txt := r.text(e, e)
	switch x := unparen(e).(type) {
	case *ast.UnaryExpr:
		if x.Op == token.ARROW {
			return txt
		}
	case *ast.CallExpr:
		fun := unparen(x.Fun)
		if id, ok := fun.(*ast.Ident); ok && (noStmtBuiltins[id.Name] || predeclaredTypes[id.Name]) {
			break
		}
		if isTypeExpr(fun) {
			break
		}
		return txt
	}
	return "_ = " + txt
}

// del deletes src[start:end]. A statement alone on its lines takes them with it (and a trailing
// comment); one that shares a line takes the ";" after it.
func (r *remover) del(start, end int) Replacement {
	ls := strings.LastIndexByte(r.src[:start], '\n') + 1
	le := len(r.src)
	if i := strings.IndexByte(r.src[end:], '\n'); i >= 0 {
		le = end + i
	}
	rest := strings.TrimSpace(r.src[end:le])
	if strings.TrimSpace(r.src[ls:start]) == "" && (rest == "" || strings.HasPrefix(rest, "//")) {
		if le < len(r.src) {
			le++
		}
		return Replacement{ls, le, ""}
	}
	e := end
	for e < len(r.src) && (r.src[e] == ' ' || r.src[e] == '\t') {
		e++
	}
	if e < len(r.src) && r.src[e] == ';' {
		e++
		for e < len(r.src) && (r.src[e] == ' ' || r.src[e] == '\t') {
			e++
		}
		return Replacement{start, e, ""}
	}
	// `{ x := 1 }`: take one side of the blanks so no double space is left
	if start > 0 && (r.src[start-1] == ' ' || r.src[start-1] == '\t') {
		for end < len(r.src) && (r.src[end] == ' ' || r.src[end] == '\t') {
			end++
		}
	}
	return Replacement{start, end, ""}
}

// newVars counts the variables a := statement declares (the others are redeclared), skipping skip.
func newVars(a *ast.AssignStmt, skip *ast.Ident) int {
	n := 0
	for _, l := range a.Lhs {
		if id, ok := l.(*ast.Ident); ok && id != skip && id.Name != "_" && id.Obj != nil && id.Obj.Decl == a {
			n++
		}
	}
	return n
}

// blankInDefine replaces the identifier of a multi-variable := with `_`, and := with = when no new
// variable is left.
func (r *remover) blankInDefine(a *ast.AssignStmt) []Replacement {
	out := []Replacement{r.blank()}
	if newVars(a, r.id) == 0 {
		p := r.off(a.TokPos)
		out = append(out, Replacement{p, p + len(":="), "="})
	}
	return out
}

func (r *remover) assign(a *ast.AssignStmt, parent ast.Node) ([]Replacement, error) {
	if len(a.Lhs) > 1 {
		return r.blankInDefine(a), nil
	}
	if len(a.Rhs) != 1 {
		return nil, toolerr.Inputf("cannot remove %s from this statement", r.name)
	}
	rhs := a.Rhs[0]
	start, end := r.off(a.Pos()), r.off(a.End())
	switch p := parent.(type) {
	case *ast.BlockStmt:
		return r.dropOrKeep(a, rhs, start, end), nil
	case *ast.CaseClause:
		if containsStmt(p.Body, a) {
			return r.dropOrKeep(a, rhs, start, end), nil
		}
	case *ast.CommClause:
		if p.Comm == ast.Stmt(a) { // case v := <-c:  ->  case <-c:
			return []Replacement{{start, r.off(rhs.Pos()), ""}}, nil
		}
		if containsStmt(p.Body, a) {
			return r.dropOrKeep(a, rhs, start, end), nil
		}
	case *ast.TypeSwitchStmt:
		if p.Assign == ast.Stmt(a) { // switch v := x.(type)  ->  switch x.(type)
			return []Replacement{{start, r.off(rhs.Pos()), ""}}, nil
		}
		if p.Init == ast.Stmt(a) {
			return r.initOf(a, rhs, start, end, r.off(p.Assign.Pos())), nil
		}
	case *ast.IfStmt:
		if p.Init == ast.Stmt(a) {
			return r.initOf(a, rhs, start, end, r.off(p.Cond.Pos())), nil
		}
	case *ast.SwitchStmt:
		if p.Init == ast.Stmt(a) {
			next := r.off(p.Body.Lbrace)
			if p.Tag != nil {
				next = r.off(p.Tag.Pos())
			}
			return r.initOf(a, rhs, start, end, next), nil
		}
	case *ast.ForStmt:
		if p.Init == ast.Stmt(a) {
			if pure(rhs) {
				return []Replacement{{start, end, ""}}, nil // for ; cond; post {
			}
			return []Replacement{{start, end, r.asStmt(rhs)}}, nil
		}
	}
	return nil, toolerr.Inputf("%s is declared by a statement that cannot be removed safely (a labelled statement, or an unusual position)", r.name)
}

// dropOrKeep deletes a statement `x := rhs` whose rhs has no effect, else keeps rhs as a statement.
func (r *remover) dropOrKeep(a *ast.AssignStmt, rhs ast.Expr, start, end int) []Replacement {
	if pure(rhs) {
		return []Replacement{r.del(start, end)}
	}
	return []Replacement{{start, end, r.asStmt(rhs)}}
}

// initOf drops an if/switch initializer (up to nextStart, which swallows the ";") or keeps its effect.
func (r *remover) initOf(a *ast.AssignStmt, rhs ast.Expr, start, end, nextStart int) []Replacement {
	if pure(rhs) {
		return []Replacement{{start, nextStart, ""}}
	}
	return []Replacement{{start, end, r.asStmt(rhs)}}
}

func (r *remover) valueSpec(s *ast.ValueSpec, gd *ast.GenDecl, ds *ast.DeclStmt) ([]Replacement, error) {
	idx := -1
	for i, n := range s.Names {
		if n == r.id {
			idx = i
		}
	}
	if idx < 0 {
		return nil, toolerr.Inputf("%s is not declared here", r.name)
	}
	whole := len(gd.Specs) == 1
	removeSpec := func() Replacement {
		if whole {
			return r.del(r.off(ds.Pos()), r.off(ds.End()))
		}
		return r.del(r.off(s.Pos()), r.off(s.End()))
	}
	if len(s.Names) > 1 {
		if len(s.Values) == 0 { // var a, x int  ->  var a int
			if idx > 0 {
				return []Replacement{{r.off(s.Names[idx-1].End()), r.off(r.id.End()), ""}}, nil
			}
			return []Replacement{{r.off(r.id.Pos()), r.off(s.Names[1].Pos()), ""}}, nil
		}
		return []Replacement{r.blank()}, nil
	}
	if len(s.Values) == 0 {
		return []Replacement{removeSpec()}, nil
	}
	if len(s.Values) != 1 {
		return nil, toolerr.Inputf("cannot remove %s from this declaration", r.name)
	}
	v := s.Values[0]
	switch {
	case pure(v):
		return []Replacement{removeSpec()}, nil
	case whole:
		return []Replacement{{r.off(ds.Pos()), r.off(ds.End()), r.asStmt(v)}}, nil
	}
	return []Replacement{r.blank()}, nil // inside a group: var _ T = f()
}

func (r *remover) rng(n *ast.RangeStmt) ([]Replacement, error) {
	if n.Tok != token.DEFINE {
		return nil, toolerr.Inputf("%s is not declared by this range clause", r.name)
	}
	switch r.id {
	case n.Value:
		return []Replacement{{r.off(n.Key.End()), r.off(n.Value.End()), ""}}, nil // for i, x := range  ->  for i := range
	case n.Key:
		if n.Value == nil { // for i := range xs  ->  for range xs
			return []Replacement{{r.off(n.Key.Pos()), r.off(n.Range), ""}}, nil
		}
		return []Replacement{r.blank()}, nil
	}
	return nil, toolerr.Inputf("%s is not declared by this range clause", r.name)
}
