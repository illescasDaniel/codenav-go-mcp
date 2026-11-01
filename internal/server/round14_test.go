package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Regression tests for the fixes of round 14 (one test per fix).

// callRaw sends the arguments as written, so a number keeps its text (1e400 does not fit a Go float).
func callRaw(t *testing.T, cs *mcp.ClientSession, name, args string) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// Items 1 + 11: every integer parameter beyond 2^53 (1e400, 2^63, 2^53+1, 1e23) gets the same sentence,
// echoing the number as the caller wrote it.
func TestHugeNumbersAnswerAlikeAndEchoOriginal(t *testing.T) {
	cs, _ := session(t, nil)
	for _, n := range []string{"1e400", "-1e400", "9223372036854775808", "9007199254740993", "1e23", "99999999999999999999999"} {
		out := callRaw(t, cs, "hover", `{"file_path":"shapes/shapes.go","line":3,"column":`+n+`}`)
		mustHave(t, n, out, "column must be a whole number of reasonable size (got "+n+", which does not fit).", "Usage: hover(")
		for _, bad := range []string{"unmarshal", "validating", "json:", "9007199254740992", "e+23"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %q leaked:\n%s", n, bad, out)
			}
		}
	}
	// Small exponent forms stay valid.
	if out := callRaw(t, cs, "hover", `{"file_path":"shapes/shapes.go","line":3,"column":1e1}`); strings.Contains(out, "reasonable size") {
		t.Errorf("1e1 is a fine column:\n%s", out)
	}
}
