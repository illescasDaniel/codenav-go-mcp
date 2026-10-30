package diagdelta

import "testing"

func TestDiffPairsInfoAcrossFilesAndNames(t *testing.T) {
	before := []Entry{{Path: "a.go", Line: 3, Severity: 3, Code: "QF1", Message: `could use "oldName" here (line 3)`, SourceLine: "x"}}
	after := []Entry{{Path: "b/b.go", Line: 40, Severity: 3, Code: "QF1", Message: `could use "newName" here (line 40)`, SourceLine: "y"}}
	d := Diff(before, after)
	if len(d.New) != 0 || len(d.Fixed) != 0 || d.Unchanged != 1 {
		t.Fatalf("an info diagnostic that only moved must pair up: %+v", d)
	}
}

func TestDiffNeverPairsErrorsLoosely(t *testing.T) {
	before := []Entry{{Path: "a.go", Line: 3, Severity: 1, Message: `undefined: "x"`}}
	after := []Entry{{Path: "b.go", Line: 3, Severity: 1, Message: `undefined: "y"`}}
	d := Diff(before, after)
	if len(d.NewErrors()) != 1 || len(d.FixedErrors()) != 1 {
		t.Fatalf("errors must stay new/fixed: %+v", d)
	}
	after = []Entry{{Path: "b.go", Line: 3, Severity: 1, Message: `undefined: "x"`}}
	if d := Diff(before, after); len(d.NewErrors()) != 1 {
		t.Fatalf("the same error in another file is a new error: %+v", d)
	}
}
