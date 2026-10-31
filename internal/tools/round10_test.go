package tools

import (
	"strings"
	"testing"
)

func TestShortQueryAndArticle(t *testing.T) {
	if got := shortQuery(strings.Repeat("a", 200)); len([]rune(got)) != 83 || !strings.HasSuffix(got, "...") {
		t.Errorf("shortQuery: %q", got)
	}
	if shortQuery("abc") != "abc" {
		t.Error("short queries stay as they are")
	}
	if withArticle("interface") != "an interface" || withArticle("struct") != "a struct" {
		t.Errorf("withArticle: %q %q", withArticle("interface"), withArticle("struct"))
	}
}
