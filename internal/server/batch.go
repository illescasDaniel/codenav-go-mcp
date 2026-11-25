package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/illescasDaniel/codenav-go-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const batchMaxCalls = 25
const batchMaxChars = 20000

var batchEditID = regexp.MustCompile(`(?:Applied \(edit id [A-Za-z0-9_-]+\)|Preview id [A-Za-z0-9_-]+;)`)

type batchDispatcherKey struct{}

// batchDispatch makes the normal SDK dispatcher available to the batch handler.
// Each child is a fresh request with the original session and request metadata.
func batchDispatch(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			ctx = context.WithValue(ctx, batchDispatcherKey{}, tools.FriendlyValidation(next))
		}
		return next(ctx, method, req)
	}
}

type batchPrepared struct {
	name    string
	args    map[string]any
	problem string
	writes  bool
}

func prepareBatchCall(raw any, index int, registry map[string]bool) batchPrepared {
	obj, ok := raw.(map[string]any)
	if !ok {
		return batchPrepared{problem: fmt.Sprintf("Call %d must be an object like {\"tool\": \"outline\", \"args\": {\"file_path\": \"src/a.go\"}}.", index)}
	}
	rest := make(map[string]any, len(obj))
	for k, v := range obj {
		rest[k] = v
	}
	value := rest["tool"]
	delete(rest, "tool")
	if value == nil || value == "" {
		value = rest["name"]
		delete(rest, "name")
	}
	name, ok := value.(string)
	if !ok || name == "" {
		return batchPrepared{problem: "Call is missing \"tool\" (the tool name, e.g. \"outline\")."}
	}
	if name == "batch" {
		return batchPrepared{problem: "batch calls cannot be nested."}
	}
	writes, ok := registry[name]
	if !ok {
		names := make([]string, 0, len(registry))
		for n := range registry {
			names = append(names, n)
		}
		sort.Strings(names)
		return batchPrepared{problem: fmt.Sprintf("Unknown tool %q. Available: %s.", name, strings.Join(names, ", "))}
	}
	argValue, present := rest["args"]
	delete(rest, "args")
	var args map[string]any
	if !present || argValue == nil {
		args = rest
	} else {
		if len(rest) > 0 {
			keys := make([]string, 0, len(rest))
			for k := range rest {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return batchPrepared{problem: fmt.Sprintf("Put %s inside \"args\".", strings.Join(keys, ", "))}
		}
		args, ok = argValue.(map[string]any)
		if !ok {
			return batchPrepared{problem: fmt.Sprintf("\"args\" must be an object of %s parameters.", name)}
		}
	}
	return batchPrepared{name: name, args: args, writes: writes}
}

func batchLabel(c batchPrepared) string {
	keys := make([]string, 0, len(c.args))
	for k := range c.args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		b, _ := json.Marshal(c.args[k])
		v := string(b)
		if len(v) > 60 {
			v = "..."
		}
		parts = append(parts, k+"="+v)
	}
	label := c.name + "(" + strings.Join(parts, ", ") + ")"
	if len([]rune(label)) > 2000 {
		return string([]rune(label)[:2000]) + "...)"
	}
	return label
}

func truncateBatch(s string) string {
	if utf8.RuneCountInString(s) <= batchMaxChars {
		return s
	}
	runes := []rune(s)
	head := string(runes[:batchMaxChars])
	// The write status can follow a long file summary. Keep IDs visible even when
	// that summary is truncated, so callers can apply or undo the result.
	for _, id := range batchEditID.FindAllString(s, -1) {
		if !strings.Contains(head, id) {
			head += "\n" + id
			break
		}
	}
	return head + fmt.Sprintf("\n... [truncated %d characters; narrow this call]", len(runes)-batchMaxChars)
}

func registerBatch(s *mcp.Server, registry map[string]bool, readOnly bool) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"calls": map[string]any{"type": "array", "items": map[string]any{}}, "stop_on_error": map[string]any{"type": []string{"boolean", "null"}}}, "required": []string{"calls"}, "additionalProperties": false}
	s.AddTool(&mcp.Tool{Name: "batch", Description: "Run up to 25 tool calls sequentially in one request. Calls are not atomic; each write keeps its own safety checks and undo. stop_on_error defaults to true when a batch contains a write.", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: boolPtrBatch(!readOnly), OpenWorldHint: boolPtrBatch(false)}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := func(msg string, bad bool) *mcp.CallToolResult {
			return &mcp.CallToolResult{IsError: bad, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
		}
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(req.Params.Arguments, &outer); err != nil || outer == nil {
			return result("batch arguments must be an object.", true), nil
		}
		for k := range outer {
			if k != "calls" && k != "stop_on_error" {
				return result(fmt.Sprintf("batch has no parameter %q.", k), true), nil
			}
		}
		var calls []any
		decoder := json.NewDecoder(bytes.NewReader(outer["calls"]))
		decoder.UseNumber()
		if err := decoder.Decode(&calls); err != nil || len(calls) == 0 {
			return result("batch needs a non-empty \"calls\" list. Usage: batch(calls, [stop_on_error]).", true), nil
		}
		if len(calls) > batchMaxCalls {
			return result(fmt.Sprintf("batch takes at most %d calls, got %d. Split it into several batches.", batchMaxCalls, len(calls)), true), nil
		}
		prepared := make([]batchPrepared, len(calls))
		hasWrite := false
		for i, c := range calls {
			prepared[i] = prepareBatchCall(c, i+1, registry)
			hasWrite = hasWrite || prepared[i].writes
		}
		stop := hasWrite
		if v, ok := outer["stop_on_error"]; ok && string(v) != "null" {
			if err := json.Unmarshal(v, &stop); err != nil {
				return result("stop_on_error must be a boolean (true or false).", true), nil
			}
		}
		dispatch, ok := ctx.Value(batchDispatcherKey{}).(mcp.MethodHandler)
		if !ok {
			return result("batch dispatcher is unavailable.", true), nil
		}
		sections := make([]string, 0, len(calls)+1)
		failed, skipped := 0, 0
		halted := false
		for i, c := range prepared {
			if halted {
				skipped++
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if c.problem != "" {
				sections = append(sections, fmt.Sprintf("[%d/%d] ERROR\n%s", i+1, len(calls), truncateBatch(c.problem)))
				failed++
				halted = stop
				continue
			}
			b, _ := json.Marshal(c.args)
			child := &mcp.CallToolRequest{Session: req.Session, Extra: req.Extra, Params: &mcp.CallToolParamsRaw{Name: c.name, Arguments: b}}
			res, err := dispatch(ctx, "tools/call", child)
			out := ""
			bad := false
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				out = "Tool failed unexpectedly."
				bad = true
			} else if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
				bad = r.IsError
				var chunks []string
				for _, part := range r.Content {
					if t, ok := part.(*mcp.TextContent); ok {
						chunks = append(chunks, t.Text)
					}
				}
				out = strings.Join(chunks, "\n")
			} else {
				out = "Tool returned no result."
				bad = true
			}
			if bad {
				failed++
				halted = stop
				if c.writes {
					out = "NOT DONE\n" + out
				}
			}
			sections = append(sections, fmt.Sprintf("[%d/%d] %s\n%s", i+1, len(calls), batchLabel(c), truncateBatch(out)))
		}
		summary := fmt.Sprintf("batch: %d/%d ok", len(calls)-failed-skipped, len(calls))
		if failed > 0 {
			summary += fmt.Sprintf(", %d failed", failed)
		}
		if skipped > 0 {
			summary += fmt.Sprintf(", %d skipped after the first failure (stop_on_error)", skipped)
		}
		sections = append(sections, summary)
		return result(strings.Join(sections, "\n\n"), false), nil
	})
}
func boolPtrBatch(v bool) *bool { return &v }
