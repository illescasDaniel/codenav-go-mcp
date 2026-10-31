package edits

import "testing"

// Round 13 (item 10): inserted lines take the ending of the line they replace or follow, and
// untouched lines never change, whatever the mix (including a tie between CRLF and LF).
func TestRestoreEOLMixedLayouts(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		{
			"tie, insert in a CRLF function",
			"package p\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\nfunc B() int {\n\treturn 2\n}\n",
			"package p\n\nfunc A() int {\n\ta := 1\n\treturn a\n}\nfunc B() int {\n\treturn 2\n}\n",
			"package p\r\n\r\nfunc A() int {\r\n\ta := 1\r\n\treturn a\r\n}\nfunc B() int {\n\treturn 2\n}\n",
		},
		{
			"CRLF majority, insert into an LF line region",
			"a\r\nb\r\nc\r\nd\ne\r\n",
			"a\nb\nc\nd\nX\nY\ne\n",
			"a\r\nb\r\nc\r\nd\nX\nY\ne\r\n",
		},
		{
			"LF majority, insert after a CRLF line",
			"a\nb\nc\r\nd\ne\n",
			"a\nb\nc\nX\nd\ne\n",
			"a\nb\nc\r\nX\r\nd\ne\n",
		},
		{
			"insertion at the start takes the line below",
			"a\r\nb\nc\n",
			"X\na\nb\nc\n",
			"X\r\na\r\nb\nc\n",
		},
		{
			"insertion at the end takes the line above",
			"a\nb\nc\r\n",
			"a\nb\nc\nX\n",
			"a\nb\nc\r\nX\r\n",
		},
		{
			"last line without ending",
			"a\r\nb\nc",
			"a\nB\nB2\nc",
			"a\r\nB\nB2\nc",
		},
		{
			"replace several lines by more",
			"a\r\nb\nc\r\nd\n",
			"a\nX\nY\nZ\nd\n",
			"a\r\nX\nY\r\nZ\r\nd\n",
		},
	}
	for _, c := range cases {
		if got := RestoreEOL(c.new, c.old); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
