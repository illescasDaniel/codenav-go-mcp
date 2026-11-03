package gosrc

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// File is a parsed source text with helpers that answer questions by byte offset.
type File struct {
	fset *token.FileSet
	ast  *ast.File
	Src  string
}

func ParseFile(filename, src string) (*File, error) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return nil, err
	}
	return &File{fset, f, src}, nil
}

func (f *File) off(p token.Pos) int { return f.fset.Position(p).Offset }

// Param is one declared parameter (a grouped `a, b int` yields two).
type Param struct {
	Name, Type string
	Variadic   bool
	NameOff    int // offset of the name in the file (named parameters)
}

// Sig is the parameter list of a function declaration or interface method.
// Start/End delimit the text between the parentheses.
type Sig struct {
	Params     []Param
	Start, End int
	Unnamed    bool
}

func (f *File) sigOf(ft *ast.FuncType) *Sig {
	s := &Sig{Start: f.off(ft.Params.Opening) + 1, End: f.off(ft.Params.Closing)}
	for _, field := range ft.Params.List {
		typ := f.Src[f.off(field.Type.Pos()):f.off(field.Type.End())]
		_, variadic := field.Type.(*ast.Ellipsis)
		if len(field.Names) == 0 {
			s.Unnamed = true
			s.Params = append(s.Params, Param{Type: typ, Variadic: variadic})
		}
		for _, n := range field.Names {
			s.Params = append(s.Params, Param{Name: n.Name, Type: typ, Variadic: variadic, NameOff: f.off(n.Pos())})
		}
	}
	return s
}

// SigAt returns the parameter list of the function or interface method whose name starts at off.
func (f *File) SigAt(off int) (*Sig, error) {
	var found *ast.FuncType
	ast.Inspect(f.ast, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		switch d := n.(type) {
		case *ast.FuncDecl:
			if f.off(d.Name.Pos()) == off {
				found = d.Type
			}
		case *ast.InterfaceType:
			for _, m := range d.Methods.List {
				if ft, ok := m.Type.(*ast.FuncType); ok && len(m.Names) == 1 && f.off(m.Names[0].Pos()) == off {
					found = ft
				}
			}
		}
		return found == nil
	})
	if found == nil {
		return nil, toolerr.Inputf("that symbol is not a function or method declaration")
	}
	return f.sigOf(found), nil
}

// Call is a call expression's argument list. Start/End delimit the arguments (between the parentheses when empty).
type Call struct {
	Args       [][2]int
	Start, End int
	Ellipsis   bool // f(xs...)
}

func calleeIdent(e ast.Expr) *ast.Ident {
	switch t := e.(type) {
	case *ast.Ident:
		return t
	case *ast.SelectorExpr:
		return t.Sel
	case *ast.ParenExpr:
		return calleeIdent(t.X)
	case *ast.IndexExpr:
		return calleeIdent(t.X)
	case *ast.IndexListExpr:
		return calleeIdent(t.X)
	}
	return nil
}

// CallAt returns the call whose callee name starts at off, or nil when that use is not a call.
func (f *File) CallAt(off int) *Call {
	var out *Call
	ast.Inspect(f.ast, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || out != nil {
			return out == nil
		}
		if id := calleeIdent(c.Fun); id != nil && f.off(id.Pos()) == off {
			out = &Call{Ellipsis: c.Ellipsis.IsValid()}
			for _, a := range c.Args {
				out.Args = append(out.Args, [2]int{f.off(a.Pos()), f.off(a.End())})
			}
			if len(c.Args) == 0 {
				out.Start, out.End = f.off(c.Lparen)+1, f.off(c.Rparen)
			} else {
				out.Start, out.End = out.Args[0][0], out.Args[len(out.Args)-1][1]
			}
			return false
		}
		return true
	})
	return out
}

// Line returns the trimmed source line containing off.
func (f *File) Line(off int) string {
	b := strings.LastIndex(f.Src[:off], "\n") + 1
	e := strings.Index(f.Src[off:], "\n")
	if e < 0 {
		e = len(f.Src) - off
	}
	return strings.TrimSpace(f.Src[b : off+e])
}

// FuncAt returns the receiver type of the function declaration whose name starts at off
// (without * or type parameters; "" for a plain function). ok is false when none starts there.
func (f *File) FuncAt(off int) (recv string, ok bool) {
	for _, decl := range f.ast.Decls {
		d, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || f.off(d.Name.Pos()) != off {
			continue
		}
		if d.Recv != nil && len(d.Recv.List) == 1 {
			recv = recvName(d.Recv.List[0].Type)
		}
		return recv, true
	}
	return "", false
}

// Group is a parenthesised declaration block with several specs: const ( ... ).
type Group struct {
	Keyword    string // const, var or type
	Start, End int    // 0-based lines of the keyword and the closing parenthesis
	Iota       bool   // a spec uses iota
}

// Groups lists the parenthesised const/var/type blocks with more than one spec.
func (f *File) Groups() []Group {
	var out []Group
	for _, decl := range f.ast.Decls {
		d, ok := decl.(*ast.GenDecl)
		if !ok || d.Tok == token.IMPORT || !d.Lparen.IsValid() || len(d.Specs) < 2 {
			continue
		}
		g := Group{Keyword: d.Tok.String(), Start: f.fset.PositionFor(d.TokPos, false).Line - 1, End: f.fset.PositionFor(d.Rparen, false).Line - 1}
		ast.Inspect(d, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "iota" {
				g.Iota = true
			}
			return !g.Iota
		})
		out = append(out, g)
	}
	return out
}
