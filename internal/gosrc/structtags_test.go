package gosrc

import (
	"strings"
	"testing"
)

func TestFieldTagAt(t *testing.T) {
	src := "package p\n\ntype U struct {\n\tName string `json:\"user_name\" db:\"name,omitempty\"`\n\tAge  int\n\tBase `yaml:\"base\"`\n\tPlain\n}\n"
	for _, c := range []struct {
		id    string
		tag   string
		found bool
	}{
		{"Name", `json:"user_name" db:"name,omitempty"`, true},
		{"Age", "", true},
		{"Base", `yaml:"base"`, true},
		{"Plain", "", true},
		{"U", "", false},
	} {
		tag, found := FieldTagAt("x.go", src, strings.Index(src, c.id))
		if tag != c.tag || found != c.found {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.id, tag, found, c.tag, c.found)
		}
	}
}

func TestFormatTagAndNames(t *testing.T) {
	tag := `json:"user_name,omitempty" db:"user_name"`
	if got := FormatTag(tag); got != `json:"user_name,omitempty", db:"user_name"` {
		t.Errorf("FormatTag = %s", got)
	}
	if !TagNamesField(tag, "user_name") || TagNamesField(tag, "other") {
		t.Error("TagNamesField")
	}
	if got := FormatTag("free text"); got != "free text" {
		t.Errorf("FormatTag(free) = %s", got)
	}
}

func TestStructTagChanges(t *testing.T) {
	old := "type U struct {\n\tName string `json:\"a\"`\n\tAge int `json:\"age\"`\n\tKeep int `json:\"k\"`\n\tNone int\n}"
	nw := "type U struct {\n\tName string `json:\"b\"`\n\tYears int `json:\"age\"`\n\tKeep int `json:\"k\"`\n\tNone int `json:\"none\"`\n\tAdded int `json:\"added\"`\n}"
	var got []string
	for _, c := range StructTagChanges(old, nw) {
		got = append(got, c.Field+"|"+c.Old+"|"+c.New)
		if c.Gone && c.Field != "Age" {
			t.Errorf("unexpected Gone: %+v", c)
		}
	}
	want := "Name|json:\"a\"|json:\"b\" Age|json:\"age\"| None||json:\"none\""
	if strings.Join(got, " ") != want {
		t.Errorf("got %q, want %q", strings.Join(got, " "), want)
	}
	if StructTagChanges("type U int", nw) != nil {
		t.Error("a non-struct has no tags")
	}
}
