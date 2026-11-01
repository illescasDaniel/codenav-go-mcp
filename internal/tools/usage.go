package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// usages maps a tool name to a one-line parameter summary ("move(to_file, [name], [file_path], ...)").
var usages = struct {
	sync.Mutex
	m map[string]string
	// ints maps a tool name to its integer-typed top-level parameters.
	ints map[string]map[string]bool
}{m: map[string]string{}, ints: map[string]map[string]bool{}}

// registerUsage records the parameters of tool name from its input struct: required ones bare, the
// others in brackets.
func registerUsage[In any](name string) {
	var in In
	t := reflect.TypeOf(in)
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	var req, opt []string
	ints := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			n, rest, _ := strings.Cut(tag, ",")
			if f.Anonymous && n == "" && f.Type.Kind() == reflect.Struct {
				walk(f.Type) // an embedded struct contributes its own fields
				continue
			}
			if n == "" {
				n = f.Name
			}
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				ints[n] = true
			}
			if strings.Contains(rest, "omitempty") || strings.Contains(rest, "omitzero") {
				opt = append(opt, "["+n+"]")
			} else {
				req = append(req, n)
			}
		}
	}
	walk(t)
	usages.Lock()
	usages.m[name] = name + "(" + strings.Join(append(req, opt...), ", ") + ")"
	usages.ints[name] = ints
	usages.Unlock()
}

var (
	typeErrRE   = regexp.MustCompile(`validating /properties/([^:]*): type: (.*) has type "(\w+)", want (?:one of )?(.*)$`)
	extraPropRE = regexp.MustCompile(`(?:validating /properties/([^:]*): )?unexpected additional properties \[([^\]]*)\]`)
	wantTypeRE  = regexp.MustCompile(`[a-z]+`)
)

var typeArticle = map[string]string{
	"integer": "an integer", "number": "a number", "string": "a string", "boolean": "a boolean (true or false)",
	"array": "an array", "object": "an object", "null": "null",
}

// friendlyTypeError rewrites the schema validator's type and unknown-property refusals into a sentence
// naming the parameter ("line must be an integer (got string "5")"); "" when the text is not one.
func friendlyTypeError(text string) string {
	if m := typeErrRE.FindStringSubmatch(text); m != nil {
		path := strings.Split(m[1], "/")
		param, item := path[0], len(path) > 1 && path[len(path)-1] == "items"
		var want []string
		for _, w := range wantTypeRE.FindAllString(m[4], -1) {
			if w != "null" && typeArticle[w] != "" {
				want = append(want, typeArticle[w])
			}
		}
		if len(want) == 0 {
			return ""
		}
		got := m[3]
		val := m[2]
		desc := got
		switch got {
		case "string":
			desc = "string " + strconvQuote(val)
		case "integer", "number", "boolean":
			desc = got + " " + val
		}
		subject := param
		if item {
			subject = "each item of " + param
		}
		return fmt.Sprintf("%s must be %s (got %s).", subject, strings.Join(want, " or "), desc)
	}
	if m := extraPropRE.FindStringSubmatch(text); m != nil {
		var names []string
		for _, n := range strings.Split(m[2], ",") {
			names = append(names, strings.Trim(strings.TrimSpace(n), `"`))
		}
		list := strings.Join(names, ", ")
		if m[1] != "" {
			param := strings.Split(m[1], "/")[0]
			msg := fmt.Sprintf("unknown field %s in an item of %s.", list, param)
			for _, n := range names {
				if n == "default" || n == "default_value" {
					msg += ` Go has no default values: use "value", the argument written at each existing call.`
				}
			}
			return msg
		}
		return fmt.Sprintf("unknown parameter(s): %s.", list)
	}
	return ""
}

// decodeErrRE: the JSON decoder's refusal of a value that does not fit the Go field ("json: cannot unmarshal
// number 9223372036854775808 into Go struct field tools.refsIn.line of type int").
var decodeErrRE = regexp.MustCompile(`json: cannot unmarshal (?:(\w+) )?(.*?) into Go struct field (?:[\w\[\]{} ]+\.)*(\w+) of type (\w+)`)

var numberTokenRE = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)

// friendlyDecodeError rewrites such a refusal into a sentence naming the parameter; "" when text is not one.
func friendlyDecodeError(text string) string {
	m := decodeErrRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	kind, param, goType := m[1], m[3], m[4]
	val := strings.TrimSuffix(strings.Trim(m[2], `"`), "} overflows")
	val = strings.TrimSuffix(val, " overflows")
	if num := numberTokenRE.FindString(val); num != "" && num != val {
		// The decoder echoes the rest of the JSON object after a number that overflows: keep the number
		// only, and since it was printed rounded to a float, do not echo digits that are not the caller's.
		val = "a number too large"
		if kind == "number" || kind == "" {
			return fmt.Sprintf("%s must be a whole number of reasonable size (got %s, which does not fit).", param, val)
		}
	}
	if len(val) > 40 {
		val = val[:40] + "..."
	}
	if strings.HasPrefix(goType, "int") || strings.HasPrefix(goType, "uint") {
		if kind == "" || kind == "number" {
			return fmt.Sprintf("%s must be a whole number of reasonable size (got %s, which does not fit).", param, val)
		}
		return fmt.Sprintf("%s must be an integer (got %s %s).", param, kind, val)
	}
	return fmt.Sprintf("%s has the wrong type (got %s).", param, strings.TrimSpace(kind+" "+val))
}

func strconvQuote(s string) string { return strconv.Quote(s) }

var missingRE = regexp.MustCompile(`(?:validating /properties/([^:]*): )?required: missing properties: \[([^\]]*)\]`)

// itemPathRE: "add/items/1" (or "add/items") in a validator path, naming an element of an array parameter.
var itemPathRE = regexp.MustCompile(`^(\w+)/items(?:/(\d+))?`)

// FriendlyValidation turns the schema validator's "required: missing properties" refusal into a hint that
// names the missing parameters and shows the tool's usage, the way the name-based tools already answer.
func FriendlyValidation(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && method == "tools/call" {
			params.Arguments = dropNulls(params.Arguments)
			if msg := hugeIntegerArg(params.Name, params.Arguments); msg != "" {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
			}
		}
		res, err := next(ctx, method, req)
		ctr, ok := res.(*mcp.CallToolResult)
		if method != "tools/call" || err != nil || !ok || ctr == nil || !ctr.IsError || len(ctr.Content) != 1 {
			return res, err
		}
		tc, ok := ctr.Content[0].(*mcp.TextContent)
		if !ok {
			return res, err
		}
		usage := ""
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
			usages.Lock()
			usage = usages.m[params.Name]
			usages.Unlock()
		}
		m := missingRE.FindStringSubmatch(tc.Text)
		if m == nil {
			msg := friendlyTypeError(tc.Text)
			if msg == "" {
				msg = friendlyDecodeError(tc.Text)
			}
			if msg != "" {
				if usage != "" {
					msg += " Usage: " + usage + " (parameters in brackets are optional)."
				}
				tc.Text = msg
			}
			return res, err
		}
		var names []string
		for _, n := range strings.Fields(m[2]) {
			names = append(names, strings.Trim(n, `"`))
		}
		msg := "Missing required parameter(s): " + strings.Join(names, ", ") + "."
		if im := itemPathRE.FindStringSubmatch(m[1]); im != nil {
			where := "an item of " + im[1]
			if im[2] != "" {
				where = fmt.Sprintf("%s[%s]", im[1], im[2])
			} else if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				if i := firstItemMissing(params.Arguments, im[1], names); i >= 0 {
					where = fmt.Sprintf("%s[%d]", im[1], i)
				}
			}
			msg = fmt.Sprintf("%s is missing %s.", where, strings.Join(names, ", "))
		}
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

// maxSafeInt is the largest integer a JSON number carries exactly through the float64 the schema validator
// reads it as (2^53): beyond it the digits the caller wrote are not the ones the tool would act on.
const maxSafeInt = 1 << 53

// hugeIntegerArg answers, for an integer parameter whose JSON number is beyond +-2^53 (or does not even fit
// a float64, like 1e400), the one message every out-of-range number gets, echoing the number as written;
// "" when no integer argument is that large.
func hugeIntegerArg(tool string, raw json.RawMessage) string {
	usages.Lock()
	ints := usages.ints[tool]
	usage := usages.m[tool]
	usages.Unlock()
	var args map[string]json.RawMessage
	if len(ints) == 0 || json.Unmarshal(raw, &args) != nil {
		return ""
	}
	names := make([]string, 0, len(args))
	for n := range args {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tok := strings.TrimSpace(string(args[n]))
		if !ints[n] || numberTokenRE.FindString(tok) != tok || tok == "" {
			continue
		}
		f, _, err := big.ParseFloat(tok, 10, 512, big.ToNearestEven)
		if err == nil && f.IsInt() && new(big.Float).Abs(f).Cmp(big.NewFloat(maxSafeInt)) <= 0 || err == nil && !f.IsInt() {
			continue
		}
		if len(tok) > 40 {
			tok = tok[:40] + "..."
		}
		msg := fmt.Sprintf("%s must be a whole number of reasonable size (got %s, which does not fit).", n, tok)
		if usage != "" {
			msg += " Usage: " + usage + " (parameters in brackets are optional)."
		}
		return msg
	}
	return ""
}

// dropNulls removes top-level arguments whose value is JSON null, so an explicit null counts as
// "not given": a required parameter then gets the friendly missing-parameter answer and an optional
// one its default, instead of a schema type error.
func dropNulls(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return raw
	}
	changed := false
	for k, v := range m {
		if string(bytes.TrimSpace(v)) == "null" {
			delete(m, k)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// firstItemMissing: the index of the first element of the array argument param that lacks one of the
// named properties, or -1.
func firstItemMissing(raw json.RawMessage, param string, names []string) int {
	var m map[string]json.RawMessage
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || json.Unmarshal(m[param], &items) != nil {
		return -1
	}
	for i, item := range items {
		for _, n := range names {
			if _, ok := item[n]; !ok {
				return i
			}
		}
	}
	return -1
}
