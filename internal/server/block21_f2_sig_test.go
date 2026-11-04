package server

import (
	"strings"
	"testing"
)

// Regression (SIG-001 follow-up): two interfaces in packages with the same name (a/store and b/store) are named by
// their directories in the note, not as store.Store and store.Store.
func TestBlock21F2_SameNamePackagesLabelledByDirectory(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21sig/a/store/store.go", "package store\n\ntype Store interface{ Load(k string) string }\n\ntype Mem struct{}\n\nfunc (Mem) Load(k string) string { return k }\n\nvar _ Store = Mem{}\n\nfunc UseA(s Store) string { return s.Load(\"x\") }\n")
	writeFile(t, root, "b21sig/b/store/store.go", "package store\n\ntype Store interface{ Load(k string) string }\n\ntype Mem struct{}\n\nfunc (Mem) Load(k string) string { return k }\n\nvar _ Store = Mem{}\n\nfunc UseB(s Store) string { return s.Load(\"y\") }\n")
	out := call(t, cs, "change_signature", map[string]any{"name": "Store.Load", "file_path": "b21sig/a/store/store.go",
		"add": []map[string]any{{"name": "opts", "type": "int", "value": "0"}}})
	mustHave(t, "same-name packages", out, "implements both b21sig/a/store.Store and b21sig/b/store.Store.")
	if strings.Contains(out, "store.Store and store.Store") {
		t.Fatalf("the note must not read store.Store and store.Store:\n%s", out)
	}
}
