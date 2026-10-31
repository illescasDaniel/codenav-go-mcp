package gosrc

import (
	"go/format"
	"testing"
)

func TestDropOrphanImportCommentsRound11(t *testing.T) {
	for _, c := range []struct{ name, before, after, want string }{
		{
			"multi-line block comment before a removed import, line comment on its line: nothing glued",
			"package p\n\nimport (\n\t\"os\"\n\t/* multi\n\tline */ \"errors\" // end\n)\n\nvar _ = os.Args\n",
			"package p\n\nimport (\n\t\"os\"\n\t/* multi\n\tline */// end\n)\n\nvar _ = os.Args\n",
			"package p\n\nimport (\n\t\"os\"\n)\n\nvar _ = os.Args\n",
		},
		{
			"comment on the removed import's line, heading of the next kept import stays",
			"package p\n\nimport (\n\t\"os\"\n\t/* multi\n\tline */ \"errors\"\n\t// end\n\t\"fmt\"\n)\n\nvar _, _ = os.Args, fmt.Sprint\n",
			"package p\n\nimport (\n\t\"os\"\n\t/* multi\n\tline */\n\t// end\n\t\"fmt\"\n)\n\nvar _, _ = os.Args, fmt.Sprint\n",
			"package p\n\nimport (\n\t\"os\"\n\t// end\n\t\"fmt\"\n)\n\nvar _, _ = os.Args, fmt.Sprint\n",
		},
		{
			"heading separated by blank lines whose whole group disappeared",
			"package p\n\nimport (\n\t\"os\"\n\n\t// Heading A\n\n\t// Heading B\n\t\"strings\"\n)\n\nvar _ = os.Args\n",
			"package p\n\nimport (\n\t\"os\"\n\n\t// Heading A\n\n\t// Heading B\n)\n\nvar _ = os.Args\n",
			"package p\n\nimport (\n\t\"os\"\n)\n\nvar _ = os.Args\n",
		},
		{
			"every import of the declaration removed: its comments do not move to file level",
			"package p\n\nimport (\n\t/* all gone */\n\t\"errors\" // tail\n)\n\nfunc F() {}\n",
			"package p\n\n/* all gone */\n// tail\n\nfunc F() {}\n",
			"package p\n\nfunc F() {}\n",
		},
		{
			"doc comment of a removed single import",
			"package p\n\n// import doc\nimport \"errors\"\n\nfunc F() {}\n",
			"package p\n\n// import doc\n\nfunc F() {}\n",
			"package p\n\nfunc F() {}\n",
		},
		{
			"a note set apart from the imports stays",
			"package p\n\nimport (\n\t\"fmt\"\n\t\"os\" // tail\n\n\t// kept note\n)\n\nvar _ = fmt.Sprint\n",
			"package p\n\nimport (\n\t\"fmt\"\n\t// tail\n\n\t// kept note\n)\n\nvar _ = fmt.Sprint\n",
			"package p\n\nimport (\n\t\"fmt\"\n\t// kept note\n)\n\nvar _ = fmt.Sprint\n",
		},
	} {
		got := DropOrphanImportComments("p.go", c.before, c.after)
		if fm, err := format.Source([]byte(got)); err != nil {
			t.Errorf("%s: result does not parse: %v\n%s", c.name, err, got)
		} else if string(fm) != c.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", c.name, fm, c.want)
		}
	}
}

func TestTidyImportBlanks(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"blank after paren", "package p\n\nimport (\n\n\t\"fmt\"\n)\n", "package p\n\nimport (\n\t\"fmt\"\n)\n"},
		{"blank before paren", "package p\n\nimport (\n\t\"fmt\"\n\n)\n", "package p\n\nimport (\n\t\"fmt\"\n)\n"},
		{"runs collapse to one", "package p\n\nimport (\n\t\"fmt\"\n\n\n\n\t\"os\"\n)\n", "package p\n\nimport (\n\t\"fmt\"\n\n\t\"os\"\n)\n"},
		{"single blank between groups stays", "package p\n\nimport (\n\t\"fmt\"\n\n\t\"os\"\n)\n", "package p\n\nimport (\n\t\"fmt\"\n\n\t\"os\"\n)\n"},
		{"unparenthesised import untouched", "package p\n\nimport \"fmt\"\n\n\nvar _ = fmt.Sprint\n", "package p\n\nimport \"fmt\"\n\n\nvar _ = fmt.Sprint\n"},
		{"code after the block untouched", "package p\n\nimport (\n\t\"fmt\"\n)\n\n\nvar _ = fmt.Sprint\n", "package p\n\nimport (\n\t\"fmt\"\n)\n\n\nvar _ = fmt.Sprint\n"},
		{"unparsable source returned as is", "package p\n\nimport (\n\n\n", "package p\n\nimport (\n\n\n"},
	} {
		if got := tidyImportBlanks("x.go", c.in); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

// Round 12: removing an import and its comment must not leave a blank line above the heading of the next
// import (there was none before); blank lines that were already there stay.
func TestDropOrphanImportCommentsNoNewBlankBeforeHeading(t *testing.T) {
	before := "package p\n\nimport (\n\t\"fmt\"\n\t/* multi\n\t   line */\n\t\"errors\"\n\t// end\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n"
	gopls := "package p\n\nimport (\n\t\"fmt\"\n\n\t// end\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n"
	want := "package p\n\nimport (\n\t\"fmt\"\n\t// end\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n"
	if got := DropOrphanImportComments("p.go", before, gopls); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	kept := "package p\n\nimport (\n\t\"fmt\"\n\n\t// end\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n"
	if got := DropOrphanImportComments("p.go", kept, kept); got != kept {
		t.Errorf("a blank line that was already there stays:\n%s", got)
	}
}
