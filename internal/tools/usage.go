package tools

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// usages maps a tool name to a one-line parameter summary ("move(to_file, [name], [file_path], ...)").
var usages = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// registerUsage records the parameters of tool name from its input struct: required ones bare, the
// others in brackets.
func registerUsage[In any](name string) {
	var in In
	t := reflect.TypeOf(in)
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	var req, opt []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "-" {
			continue
		}
		n, rest, _ := strings.Cut(tag, ",")
		if n == "" {
			n = t.Field(i).Name
		}
		if strings.Contains(rest, "omitempty") || strings.Contains(rest, "omitzero") {
			opt = append(opt, "["+n+"]")
		} else {
			req = append(req, n)
		}
	}
	usages.Lock()
	usages.m[name] = name + "(" + strings.Join(append(req, opt...), ", ") + ")"
	usages.Unlock()
}

var missingRE = regexp.MustCompile(`required: missing properties: \[([^\]]*)\]`)

// FriendlyValidation turns the schema validator's "required: missing properties" refusal into a hint that
// names the missing parameters and shows the tool's usage, the way the name-based tools already answer.
func FriendlyValidation(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		ctr, ok := res.(*mcp.CallToolResult)
		if method != "tools/call" || err != nil || !ok || ctr == nil || !ctr.IsError || len(ctr.Content) != 1 {
			return res, err
		}
		tc, ok := ctr.Content[0].(*mcp.TextContent)
		if !ok {
			return res, err
		}
		m := missingRE.FindStringSubmatch(tc.Text)
		if m == nil {
			return res, err
		}
		var names []string
		for _, n := range strings.Fields(m[1]) {
			names = append(names, strings.Trim(n, `"`))
		}
		msg := "Missing required parameter(s): " + strings.Join(names, ", ") + "."
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
			usages.Lock()
			u := usages.m[params.Name]
			usages.Unlock()
			if u != "" {
				msg += " Usage: " + u + " (parameters in brackets are optional)."
			}
		}
		tc.Text = msg
		return res, err
	}
}
