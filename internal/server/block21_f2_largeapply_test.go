package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// b21f2Snapshot reads every rel path under root (the bytes that are on disk now).
func b21f2Snapshot(t *testing.T, root string, rels []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = string(b)
	}
	return out
}

// Regression (T1 follow-up): the whole apply_edit(allow_large) flow, with real gopls. A rename over the file limit
// is held with a preview id; apply_edit without allow_large refuses it again with the same id; apply_edit with
// allow_large writes all 46 files; undo_edit restores them byte for byte.
func TestBlock21F2_LargeRenameApplyEndToEnd(t *testing.T) {
	cs, root := session(t, nil)
	const users = 45
	writeFile(t, root, "lg/shared.go", "package lg\n\nfunc Shared() {}\n")
	rels := []string{"lg/shared.go"}
	for i := 0; i < users; i++ {
		rel := fmt.Sprintf("lg/use%02d.go", i)
		writeFile(t, root, rel, fmt.Sprintf("package lg\n\nfunc Use%02d() { Shared() }\n", i))
		rels = append(rels, rel)
	}
	before := b21f2Snapshot(t, root, rels)

	// Over the limit of 40 files: held as a preview, nothing written.
	out, isErr := callIsError(t, cs, "rename_symbol", map[string]any{"name": "lg.Shared", "new_name": "Common"})
	if !isErr {
		t.Fatalf("an over-limit rename must be a refusal (isError):\n%s", out)
	}
	mustHave(t, "rename refused", out, "touches 46 files (limit 40)", "Preview id", "allow_large=true")
	if got := b21f2Snapshot(t, root, rels); !reflect.DeepEqual(got, before) {
		t.Fatal("a refused over-limit rename wrote files")
	}
	id := previewID(t, out)

	// Without allow_large the same preview is refused again, under the same id: no new preview.
	out2, isErr2 := callIsError(t, cs, "apply_edit", map[string]any{"id": id})
	if !isErr2 {
		t.Fatalf("apply_edit without allow_large must refuse an over-limit preview:\n%s", out2)
	}
	if got := previewID(t, out2); got != id {
		t.Fatalf("apply_edit issued id %s, want the same preview %s:\n%s", got, id, out2)
	}
	if got := b21f2Snapshot(t, root, rels); !reflect.DeepEqual(got, before) {
		t.Fatal("a refused apply_edit wrote files")
	}

	// With allow_large the preview is written: 46 files change, and the reply says Applied.
	out3, isErr3 := callIsError(t, cs, "apply_edit", map[string]any{"id": id, "allow_large": true})
	if isErr3 || !strings.Contains(out3, "Applied") {
		t.Fatalf("apply_edit(allow_large=true) must write the preview (isError=false, Applied):\n%s", out3)
	}
	after := b21f2Snapshot(t, root, rels)
	changed := 0
	for _, rel := range rels {
		if after[rel] != before[rel] {
			changed++
		}
	}
	if changed != users+1 {
		t.Fatalf("%d files changed on disk, want all %d", changed, users+1)
	}
	if !strings.Contains(after["lg/shared.go"], "func Common()") {
		t.Fatalf("the declaration was not renamed:\n%s", after["lg/shared.go"])
	}

	// undo_edit restores every file byte for byte.
	if out := call(t, cs, "undo_edit", map[string]any{}); !strings.Contains(out, "Reverted") {
		t.Fatalf("undo_edit did not revert the large apply:\n%s", out)
	}
	if got := b21f2Snapshot(t, root, rels); !reflect.DeepEqual(got, before) {
		t.Fatal("undo_edit did not restore the files byte for byte")
	}
}
