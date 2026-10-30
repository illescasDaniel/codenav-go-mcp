package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// FieldTagAt finds the struct field whose name identifier (or, for an embedded field, type identifier)
// starts at byte offset off. found reports that there is such a field; tag is its struct tag with the
// quotes removed ("" for none).
func FieldTagAt(filename, src string, off int) (tag string, found bool) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return "", false
	}
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || found {
			return !found
		}
		for _, fld := range st.Fields.List {
			var ids []*ast.Ident
			ids = append(ids, fld.Names...)
			if len(fld.Names) == 0 {
				if id := embeddedIdent(fld.Type); id != nil {
					ids = append(ids, id)
				}
			}
			for _, id := range ids {
				if fset.Position(id.Pos()).Offset == off {
					found, tag = true, fieldTag(fld)
				}
			}
		}
		return !found
	})
	return tag, found
}

func embeddedIdent(t ast.Expr) *ast.Ident {
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.SelectorExpr:
			return x.Sel
		case *ast.Ident:
			return x
		default:
			return nil
		}
	}
}

func fieldTag(fld *ast.Field) string {
	if fld.Tag == nil {
		return ""
	}
	if s, err := strconv.Unquote(fld.Tag.Value); err == nil {
		return s
	}
	return ""
}

// TagPair is one key:"value" of a struct tag.
type TagPair struct{ Key, Value string }

// ParseTag splits a struct tag into its key:"value" pairs (the convention reflect.StructTag follows).
// Text that is not in that form is returned as one pair with an empty Key.
func ParseTag(tag string) []TagPair {
	var out []TagPair
	rest := tag
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			return out
		}
		i := 0
		for i < len(rest) && rest[i] > ' ' && rest[i] != ':' && rest[i] != '"' && rest[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(rest) || rest[i] != ':' || rest[i+1] != '"' {
			return append(out, TagPair{Value: strings.TrimSpace(rest)})
		}
		key := rest[:i]
		rest = rest[i+1:]
		j := 1
		for j < len(rest) && rest[j] != '"' {
			if rest[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(rest) {
			return append(out, TagPair{Value: strings.TrimSpace(key + ":" + rest)})
		}
		val, err := strconv.Unquote(rest[:j+1])
		if err != nil {
			return append(out, TagPair{Value: strings.TrimSpace(key + ":" + rest)})
		}
		out = append(out, TagPair{key, val})
		rest = rest[j+1:]
	}
}

// FormatTag renders a tag for a message: `json:"user_name", db:"user_name"`.
func FormatTag(tag string) string {
	var parts []string
	for _, p := range ParseTag(tag) {
		if p.Key == "" {
			parts = append(parts, p.Value)
		} else {
			parts = append(parts, p.Key+":"+strconv.Quote(p.Value))
		}
	}
	return strings.Join(parts, ", ")
}

// TagNamesField: some tag value (its part before a comma) equals name.
func TagNamesField(tag, name string) bool {
	for _, p := range ParseTag(tag) {
		if p.Key != "" && strings.SplitN(p.Value, ",", 2)[0] == name {
			return true
		}
	}
	return false
}

// TagChange is a field of a struct type whose tag differs between two versions of its declaration.
type TagChange struct {
	Field    string
	Old, New string
	Gone     bool // the field has a tag in the old version and is not in the new one
}

// StructTagChanges compares the struct declared by oldDecl with the one in newDecl (each the source of
// a type declaration, with or without the `type` keyword) and lists the fields whose tag changed or
// that carried a tag and are gone (added fields are not reported). Nested anonymous structs are compared by dotted field path.
func StructTagChanges(oldDecl, newDecl string) []TagChange {
	o, n := structFields(oldDecl), structFields(newDecl)
	if o == nil || n == nil {
		return nil
	}
	var out []TagChange
	for _, k := range o.order {
		ot := o.tags[k]
		nt, ok := n.tags[k]
		switch {
		case !ok && ot != "":
			out = append(out, TagChange{Field: k, Old: ot, Gone: true})
		case ok && nt != ot:
			out = append(out, TagChange{Field: k, Old: ot, New: nt})
		}
	}
	return out
}

type fieldSet struct {
	order []string
	tags  map[string]string
}

// structFields reads the fields of the first struct type in a type declaration.
func structFields(decl string) *fieldSet {
	var f *ast.File
	for _, prefix := range []string{"", "type "} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n\n"+prefix+decl, parser.ParseComments)
		if err == nil {
			f = file
			break
		}
	}
	if f == nil {
		return nil
	}
	fs := &fieldSet{tags: map[string]string{}}
	var walk func(st *ast.StructType, prefix string)
	walk = func(st *ast.StructType, prefix string) {
		for _, fld := range st.Fields.List {
			var names []string
			for _, n := range fld.Names {
				names = append(names, n.Name)
			}
			if len(names) == 0 {
				if id := embeddedIdent(fld.Type); id != nil {
					names = append(names, id.Name)
				}
			}
			for _, nm := range names {
				k := prefix + nm
				fs.order = append(fs.order, k)
				fs.tags[k] = fieldTag(fld)
				if inner, ok := fld.Type.(*ast.StructType); ok {
					walk(inner, k+".")
				}
			}
		}
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && !found {
			if st, ok := ts.Type.(*ast.StructType); ok {
				found = true
				walk(st, "")
			}
			return false
		}
		return true
	})
	if !found {
		return nil
	}
	return fs
}
