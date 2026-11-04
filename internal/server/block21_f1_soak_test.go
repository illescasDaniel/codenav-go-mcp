package server

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// b21f1Sample is what a soak run measures: open descriptors of this process and of its gopls child,
// goroutines, and the live heap after a collection. goplsFds is -1 when the child cannot be found (no /proc,
// or no gopls child yet).
type b21f1Sample struct {
	fds, goroutines, goplsFds int
	heap                      uint64
}

// b21f1HeapBound is how much the live heap (after a collection) may grow over a run. The measured growth is
// far below it; growth past it is a leak, not noise.
const b21f1HeapBound = 16 << 20

func b21f1Count(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	return len(ents)
}

// b21f1GoplsPid finds the gopls child of this process through /proc/self/task/*/children.
func b21f1GoplsPid() int {
	tasks, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil {
		return 0
	}
	for _, f := range tasks {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(b)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil && strings.TrimSpace(string(comm)) == "gopls" {
				return pid
			}
		}
	}
	return 0
}

func b21f1Take() b21f1Sample {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := b21f1Sample{fds: b21f1Count("/proc/self/fd"), goroutines: runtime.NumGoroutine(), goplsFds: -1, heap: ms.HeapAlloc}
	if pid := b21f1GoplsPid(); pid != 0 {
		s.goplsFds = b21f1Count(fmt.Sprintf("/proc/%d/fd", pid))
	}
	return s
}

// SCALE/LEAK-015: a long session of reads and edit previews must not keep growing descriptors or goroutines.
// N is 40 by default (a smoke test that takes well under a second); CODENAV_SOAK=1 runs 200 calls of each kind.
// Samples are taken after one warm-up of each call (the first calls start gopls and fill its caches), then every
// quarter of the run and at the end. The bounds are not loosened for a leak: growth is the finding.
func TestBlock21F1_SoakFdsThreadsBounded(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs Linux /proc")
	}
	n := 40
	if os.Getenv("CODENAV_SOAK") == "1" {
		n = 200
	}
	began := time.Now()
	cs, _ := session(t, nil)
	// calls runs one call of each kind in turn and checks that its answer is the real one (a refusal would
	// make the soak measure nothing).
	calls := func(i int) {
		var out, want string
		switch i % 3 {
		case 0:
			out, want = call(t, cs, "symbol_info", map[string]any{"name": "Square"}), "Square"
		case 1:
			out, want = call(t, cs, "references", map[string]any{"file_path": "shapes/shapes.go", "line": 4, "column": 6}), "shapes.go:4:6"
		default:
			out, want = call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "return s.Side * s.Side",
				"new_string": "return s.Side * s.Side * 1", "apply": false}), "shapes.go"
		}
		if !strings.Contains(out, want) {
			t.Fatalf("call %d answered without %q:\n%s", i, want, out)
		}
	}
	for i := 0; i < 3; i++ {
		calls(i)
	}
	first := b21f1Take()
	samples := []b21f1Sample{first}
	every := n / 4
	for i := 0; i < n; i++ {
		calls(i)
		if (i+1)%every == 0 && i+1 < n {
			samples = append(samples, b21f1Take())
		}
	}
	last := b21f1Take()
	samples = append(samples, last)
	t.Logf("run of %d calls of each kind took %v", n, time.Since(began).Round(time.Millisecond))
	for i, s := range samples {
		t.Logf("sample %d: fds=%d goroutines=%d goplsFds=%d heap=%d", i, s.fds, s.goroutines, s.goplsFds, s.heap)
	}
	if first.fds < 0 || last.fds < 0 {
		t.Fatal("cannot count this process's descriptors")
	}
	if growth := last.fds - first.fds; growth > 3 {
		t.Errorf("descriptors of this process grew by %d over %d calls of each kind (bound 3)", growth, n)
	}
	if growth := last.goroutines - first.goroutines; growth > 10 {
		t.Errorf("goroutines grew by %d over %d calls of each kind (bound 10)", growth, n)
	}
	if first.goplsFds >= 0 && last.goplsFds >= 0 {
		if growth := last.goplsFds - first.goplsFds; growth > 3 {
			t.Errorf("gopls descriptors grew by %d over %d calls of each kind (bound 3)", growth, n)
		}
	} else {
		t.Logf("gopls descriptors not checked: no gopls child of this process was found (first=%d last=%d)", first.goplsFds, last.goplsFds)
	}
	if growth := int64(last.heap) - int64(first.heap); growth > b21f1HeapBound {
		t.Errorf("live heap grew by %d bytes over %d calls of each kind (bound %d)", growth, n, int64(b21f1HeapBound))
	}
}
