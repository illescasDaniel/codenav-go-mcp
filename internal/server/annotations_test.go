package server

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PROTO-003: the annotations say what each tool does. The eight tools that overwrite files are destructive and
// closed-world; the read tools and verify_changes are read-only and closed-world. On a read-only server the edit
// tools write nothing, so they are read-only there too. A new tool must be classified here.
func TestToolAnnotations(t *testing.T) {
	writeTools := map[string]bool{"edit": true, "edit_symbol": true, "rename_symbol": true, "change_signature": true,
		"move": true, "quick_fix": true, "apply_edit": true, "undo_edit": true}
	readTools := map[string]bool{"workspace": true, "hover": true, "definition": true, "references": true,
		"search_symbol": true, "diagnostics": true, "symbol_info": true, "outline": true, "callers": true,
		"implementations": true, "verify_changes": true}
	for _, readOnly := range []bool{false, true} {
		env := ""
		if readOnly {
			env = "1"
		}
		t.Run(map[bool]string{false: "writable", true: "read-only"}[readOnly], func(t *testing.T) {
			t.Setenv(EnvReadOnly, env)
			srv, mgr := New("test")
			t.Cleanup(mgr.Close)
			ct, st := mcp.NewInMemoryTransports()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)
			if _, err := srv.Connect(ctx, st, nil); err != nil {
				t.Fatal(err)
			}
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			res, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, tl := range res.Tools {
				seen[tl.Name] = true
				a := tl.Annotations
				if a == nil {
					t.Errorf("%s: no annotations", tl.Name)
					continue
				}
				if a.OpenWorldHint == nil || *a.OpenWorldHint {
					t.Errorf("%s: openWorldHint must be false, got %v", tl.Name, a.OpenWorldHint)
				}
				switch {
				case readTools[tl.Name] || (writeTools[tl.Name] && readOnly):
					if !a.ReadOnlyHint {
						t.Errorf("%s: readOnlyHint must be true", tl.Name)
					}
				case writeTools[tl.Name]:
					if a.ReadOnlyHint {
						t.Errorf("%s: readOnlyHint must be false", tl.Name)
					}
					if a.DestructiveHint == nil || !*a.DestructiveHint {
						t.Errorf("%s: destructiveHint must be true, got %v", tl.Name, a.DestructiveHint)
					}
					if a.IdempotentHint {
						t.Errorf("%s: idempotentHint must be false", tl.Name)
					}
				default:
					t.Errorf("%s: not classified in TestToolAnnotations", tl.Name)
				}
			}
			for name := range readTools {
				if !seen[name] {
					t.Errorf("read tool %s is not listed", name)
				}
			}
			for name := range writeTools {
				if !seen[name] && !(readOnly && (name == "apply_edit" || name == "undo_edit")) {
					t.Errorf("write tool %s is not listed", name)
				}
			}
		})
	}
}
