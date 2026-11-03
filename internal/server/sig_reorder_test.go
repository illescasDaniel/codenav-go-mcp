package server

import (
	"strings"
	"testing"
)

// SIG-002: a reorder that runs two arguments with calls or channel receives in another order changes the values
// they produce, so the write is held as a preview that apply_edit writes on request.
func TestChangeSignatureReorderHoldsCallArguments(t *testing.T) {
	for _, c := range []struct{ name, run string }{
		{"identical calls", "Pair(next(), next())"},
		{"distinct calls", "Pair(next(), next()+100)"},
	} {
		const head = "package rsig\n\nvar n int\n\nfunc next() int { n++; return n }\n\nfunc Pair(a, b int) int { return a*10 + b }\n\nfunc Run() int { return "
		pair := head + c.run + " }\n"
		cs, root := session(t, nil)
		writeFile(t, root, "rsig/pair.go", pair)
		out := call(t, cs, "change_signature", map[string]any{"name": "rsig.Pair", "reorder": []string{"b", "a"}})
		mustHave(t, c.name+": held", out, "Not written", "changes the order in which 1 call(s) evaluate their arguments", "rsig/pair.go:", "apply_edit")
		if readFile(t, root, "rsig/pair.go") != pair {
			t.Fatalf("%s: a held reorder must not write", c.name)
		}
		id := previewID(t, out)
		if out := call(t, cs, "apply_edit", map[string]any{"id": id}); !strings.Contains(out, "Applied") {
			t.Fatalf("%s: apply_edit must write the held reorder:\n%s", c.name, out)
		}
		if got := readFile(t, root, "rsig/pair.go"); !strings.Contains(got, "func Pair(b, a int) int") {
			t.Fatalf("%s: apply_edit must reorder the declaration:\n%s", c.name, got)
		}
	}
}

// Arguments without calls or receives are reordered as before, with no hold.
func TestChangeSignatureReorderOfPlainArgumentsIsNotHeld(t *testing.T) {
	const src = "package rsig2\n\nfunc Pair(a, b int) int { return a*10 + b }\n\nfunc Run(x int) int { return Pair(x, 1) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "rsig2/pair.go", src)
	out := call(t, cs, "change_signature", map[string]any{"name": "rsig2.Pair", "reorder": []string{"b", "a"}})
	mustHave(t, "plain reorder", out, "Applied")
	if strings.Contains(out, "Not written") {
		t.Fatalf("plain arguments must not hold the reorder:\n%s", out)
	}
	if got := readFile(t, root, "rsig2/pair.go"); !strings.Contains(got, "Pair(1, x)") {
		t.Fatalf("the call must be reordered:\n%s", got)
	}
}

// SIG-004: the comments inside a rewritten call stay with their arguments: a comment after an argument's comma
// stays on that line, a comment on its own line moves with the argument after it, and a comment after the last
// argument's comma stays after that argument.
func TestChangeSignatureKeepsCommentsInsideCall(t *testing.T) {
	const call1 = "package csig\n\nfunc Multi(a, b, c int) int { return a + b + c }\n\nfunc Run() int {\n\treturn Multi(\n\t\t1, // first\n\t\t// second is below\n\t\t2,\n\t\t3, // trailing\n\t)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "csig/multi.go", call1)
	out := call(t, cs, "change_signature", map[string]any{"name": "csig.Multi", "add": []any{map[string]any{"name": "z", "type": "int", "value": "0"}}})
	mustHave(t, "add keeps comments", out, "Applied")
	mustHave(t, "add keeps comments in the file", readFile(t, root, "csig/multi.go"),
		"1, // first", "// second is below\n\t\t2,", "3, // trailing", "\t\t0,")

	const call2 = "package csig2\n\nfunc Multi(a, b, c int) int { return a + b + c }\n\nfunc Run() int {\n\treturn Multi(\n\t\t1, // one\n\t\t// before two\n\t\t2,\n\t\t3, // three\n\t)\n}\n"
	cs, root = session(t, nil)
	writeFile(t, root, "csig2/multi.go", call2)
	out = call(t, cs, "change_signature", map[string]any{"name": "csig2.Multi", "reorder": []string{"c", "a", "b"}})
	mustHave(t, "reorder keeps comments", out, "Applied")
	mustHave(t, "reorder keeps each comment with its argument", readFile(t, root, "csig2/multi.go"),
		"3, // three\n\t\t1, // one\n\t\t// before two\n\t\t2,")
}

// SIG-006: a method expression (*T).M(t, args...) passes the receiver as argument 0, so an added parameter at
// position 0 goes after the receiver. A call through a value (t.M(2)) is an ordinary method call.
func TestChangeSignatureRewritesMethodExpression(t *testing.T) {
	const src = "package msig\n\ntype T struct{}\n\nfunc (t *T) M(a int) int { return a }\n\ntype U struct{ *T }\n\nfunc Run() int {\n\tt := &T{}\n\tu := U{t}\n\treturn (*T).M(t, 1) + t.M(2) + u.T.M(3)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "msig/method.go", src)
	out := call(t, cs, "change_signature", map[string]any{"name": "T.M", "file_path": "msig/method.go", "add": []any{map[string]any{"name": "c", "type": "int", "value": "9", "position": 0}}})
	mustHave(t, "method expression", out, "Applied")
	// u.T is a field: its method call is an ordinary call, not a method expression.
	mustHave(t, "method expression rewritten", readFile(t, root, "msig/method.go"), "(*T).M(t, 9, 1)", "t.M(9, 2)", "u.T.M(9, 3)")

	// A variadic method: the receiver of (*T).V(t, 1, 2) is not a parameter, and u.T.V(1, 2) is an ordinary call.
	const vsrc = "package vsig\n\ntype T struct{}\n\nfunc (t *T) V(a int, xs ...int) int { return a + len(xs) }\n\ntype U struct{ *T }\n\nfunc Run() int {\n\tt := &T{}\n\tu := U{t}\n\treturn (*T).V(t, 1, 2) + u.T.V(1, 2)\n}\n"
	cs, root = session(t, nil)
	writeFile(t, root, "vsig/variadic.go", vsrc)
	out = call(t, cs, "change_signature", map[string]any{"name": "T.V", "file_path": "vsig/variadic.go", "add": []any{map[string]any{"name": "c", "type": "int", "value": "9", "position": 0}}})
	mustHave(t, "variadic method expression", out, "Applied")
	mustHave(t, "variadic rewritten", readFile(t, root, "vsig/variadic.go"), "(*T).V(t, 9, 1, 2)", "u.T.V(9, 1, 2)")
}

// SIG-006: a variable that shadows a type name is a value, not a type: counter.Inc(1, 2) is an ordinary method call,
// so the added parameter goes before the arguments instead of after a receiver.
func TestChangeSignatureShadowedTypeNameIsNotMethodExpression(t *testing.T) {
	const shadow = "package shadow\n\ntype counter struct{ n int }\n\nfunc (c *counter) Inc(xs ...int) int { return len(xs) + c.n }\n\nfunc Run() int {\n\tcounter := &counter{}\n\treturn counter.Inc(1, 2)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "shadow/counter.go", shadow)
	out := call(t, cs, "change_signature", map[string]any{"name": "counter.Inc", "file_path": "shadow/counter.go", "add": []any{map[string]any{"name": "n", "type": "int", "value": "9", "position": 0}}})
	mustHave(t, "shadowed call rewritten", out, "Applied")
	mustHave(t, "shadowed call is an ordinary call", readFile(t, root, "shadow/counter.go"), "counter.Inc(9, 1, 2)")
}

// Comments of a removed argument go with it: the rewritten call drops them and the output names them.
func TestChangeSignatureDroppedCommentIsNoted(t *testing.T) {
	const src = "package dsig\n\nfunc Multi(a, b, c int) int { return a + c }\n\nfunc Run() int {\n\treturn Multi(\n\t\t1, // first\n\t\t// second is below\n\t\t2,\n\t\t3, // trailing\n\t)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "dsig/multi.go", src)
	out := call(t, cs, "change_signature", map[string]any{"name": "dsig.Multi", "remove": []string{"b"}})
	mustHave(t, "dropped comment", out, "Applied", "comment(s) inside a call were dropped with a removed argument", "// second is below")
	got := readFile(t, root, "dsig/multi.go")
	mustHave(t, "kept comments", got, "1, // first", "3, // trailing")
	if strings.Contains(got, "second is below") {
		t.Fatalf("the comment of the removed argument must not stay:\n%s", got)
	}
}

// A reorder whose rewritten arguments have the same texts in the same order leaves the call as written, comments
// and layout included.
func TestChangeSignatureSameTextReorderLeavesCallAlone(t *testing.T) {
	const same = "package same\n\nfunc Pair(a, b int) int { return a*10 + b }\n\nfunc Run(x int) int {\n\treturn Pair(x, // keep\n\t\tx)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "same/pair.go", same)
	out := call(t, cs, "change_signature", map[string]any{"name": "same.Pair", "reorder": []string{"b", "a"}})
	mustHave(t, "same-text reorder", out, "Updated 0 call site(s)")
	if got := readFile(t, root, "same/pair.go"); !strings.Contains(got, "return Pair(x, // keep\n\t\tx)") {
		t.Fatalf("the call must stay as written:\n%s", got)
	}
}

// A line comment after the last argument's comma must not end the line the rewritten call's closing parenthesis
// is on: the reordered call is valid Go and keeps both comments.
func TestChangeSignatureReorderWithTrailingCommentsIsValid(t *testing.T) {
	const tail = "package tailc\n\nfunc Pair(a, b int) int { return a*10 + b }\n\nfunc Run() int {\n\treturn Pair(1, // c\n\t\t2, /* x */)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "tailc/pair.go", tail)
	out := call(t, cs, "change_signature", map[string]any{"name": "tailc.Pair", "reorder": []string{"b", "a"}})
	mustHave(t, "tail comments", out, "Applied")
	mustHave(t, "tail comments kept", readFile(t, root, "tailc/pair.go"), "/* x */", "// c")
}

// SIG-007: a parameter named like a local declared in the outermost block of the body is refused, naming both.
// A := that also declares another new variable assigns to the parameter and compiles, so it is not refused.
func TestChangeSignatureRefusesParameterClashingWithLocal(t *testing.T) {
	const clash = "package clash\n\nfunc Clash(a int) int { b := a; return b }\n\nfunc RunClash() int { return Clash(1) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "clash/clash.go", clash)
	out := call(t, cs, "change_signature", map[string]any{"name": "clash.Clash", "add": []any{map[string]any{"name": "b", "type": "int", "value": "0"}}})
	mustHave(t, "clash refused", out, `parameter "b" would clash with the local "b" declared at clash/clash.go:3`)
	if readFile(t, root, "clash/clash.go") != clash {
		t.Fatal("a refused clash must not write")
	}

	const two = "package clash2\n\nfunc two() (int, error) { return 1, nil }\n\nfunc Two(a int) int {\n\tb, err := two()\n\tif err != nil {\n\t\treturn 0\n\t}\n\treturn a + b\n}\n\nfunc RunTwo() int { return Two(1) }\n"
	cs, root = session(t, nil)
	writeFile(t, root, "clash2/two.go", two)
	out = call(t, cs, "change_signature", map[string]any{"name": "clash2.Two", "add": []any{map[string]any{"name": "b", "type": "int", "value": "0"}}})
	if strings.Contains(out, "would clash") {
		t.Fatalf("a := that declares another new variable must not be refused:\n%s", out)
	}
}

// SIG-007: a parameter named like a package-level variable that the body uses is refused: the parameter would take
// that use over. A use that resolves to a local of the body (here the := of clash2) is not a capture.
func TestChangeSignatureRefusesParameterCapturingPackageVar(t *testing.T) {
	const capt = "package capture\n\nvar b = 3\n\nfunc F(a int) int { return a + b }\n\nfunc RunF() int { return F(1) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "capture/capture.go", capt)
	out := call(t, cs, "change_signature", map[string]any{"name": "capture.F", "add": []any{map[string]any{"name": "b", "type": "int", "value": "0"}}})
	mustHave(t, "capture refused", out, `parameter "b" would take over the "b" used at capture/capture.go:5`)
	if readFile(t, root, "capture/capture.go") != capt {
		t.Fatal("a refused capture must not write")
	}
}
