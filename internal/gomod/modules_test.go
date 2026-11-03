package gomod

import (
	"reflect"
	"strings"
	"testing"
)

// BUG-QF-002: without -e, go list -m -json all fails outright when one module of the graph cannot be read (offline,
// not in the module cache), so the build list was unknown and an offer from outside it was kept. The -e listing
// still names every module, the unreadable one with an error and no directory; parseBuildList reads that output.
func TestParseBuildListKeepsModulesAroundAnUnreadableOne(t *testing.T) {
	out := `{
	"Path": "example.com/qf",
	"Dir": "/work/qf"
}
{
	"Path": "gopkg.in/check.v1",
	"Error": {
		"Err": "module lookup disabled by GOPROXY=off"
	}
}
{
	"Path": "gopkg.in/yaml.v3",
	"Dir": "/cache/gopkg.in/yaml.v3@v3.0.1"
}
`
	mods, ok := parseBuildList(strings.NewReader(out))
	want := []Module{
		{Path: "example.com/qf", Dir: "/work/qf"},
		{Path: "gopkg.in/check.v1"},
		{Path: "gopkg.in/yaml.v3", Dir: "/cache/gopkg.in/yaml.v3@v3.0.1"},
	}
	if !ok || !reflect.DeepEqual(mods, want) {
		t.Errorf("parseBuildList = %+v, %v; want %+v, true", mods, ok, want)
	}
}

// Nothing listed, or output that is not the listing, is no build list: the caller keeps every offer.
func TestParseBuildListNothingListed(t *testing.T) {
	for _, out := range []string{"", "go: go.mod file not found\n", `{"Path": "example.com/qf"`} {
		if _, ok := parseBuildList(strings.NewReader(out)); ok {
			t.Errorf("parseBuildList(%q) reported a build list", out)
		}
	}
}
