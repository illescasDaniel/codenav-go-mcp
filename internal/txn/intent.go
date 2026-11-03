package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// StateDirEnv overrides the directory that holds the intent logs (tests, unusual setups).
const StateDirEnv = "CODENAV_GO_MCP_STATE_DIR"

const intentFile = "intent.json"

// DefaultLogDir is where the intent log of a workspace lives: in the user cache, never in the tree.
func DefaultLogDir(root string) string {
	base := os.Getenv(StateDirEnv)
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(cache, "codenav-go-mcp")
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, hex.EncodeToString(sum[:]))
}

type intentFileEntry struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	BaseHash string `json:"base_hash"` // "" = absent before
	NewHash  string `json:"new_hash"`  // "" = absent after
	Orig     string `json:"orig,omitempty"`
}

type intent struct {
	ID          string            `json:"id"`
	PID         int               `json:"pid"`
	Op          string            `json:"op"` // "apply" or "undo"
	Description string            `json:"description"`
	Started     time.Time         `json:"started"`
	State       string            `json:"state"` // "writing", "rolling back"
	Files       []intentFileEntry `json:"files"`
}

// intentLog is a started log; a nil *intentLog (logging unavailable) is a valid no-op.
type intentLog struct {
	dir string
	in  intent
}

func (l *intentLog) save() error {
	b, err := json.MarshalIndent(l.in, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(l.dir, intentFile), b, nil)
}

func (l *intentLog) setState(state string) {
	if l == nil {
		return
	}
	l.in.State = state
	_ = l.save()
}

// done forgets the log after a write that succeeded or was rolled back completely.
func (l *intentLog) done() {
	if l == nil {
		return
	}
	_ = os.RemoveAll(filepath.Join(l.dir, "orig"))
	_ = os.Remove(filepath.Join(l.dir, intentFile))
}

func processAlive(pid int) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

// beginIntent records what is about to be written (plan, base hashes, original contents) before the
// first write. It never fails the edit: when the log cannot be written it returns nil and a note.
func beginIntent(dir, op string, p *edits.EditPlan) (*intentLog, string) {
	if dir == "" {
		return nil, "the write-ahead log is unavailable (no user cache directory); a crash mid-edit could not be reported"
	}
	if old, ok := readIntent(dir); ok && processAlive(old.PID) {
		return nil, "another codenav-go-mcp process is writing in this workspace; this edit is not logged"
	}
	archiveIntent(dir) // an earlier unfinished entry is kept aside, not overwritten
	l := &intentLog{dir: dir, in: intent{ID: newID(), PID: os.Getpid(), Op: op, Description: p.Description, Started: time.Now(), State: "writing"}}
	fail := func(err error) (*intentLog, string) {
		_ = os.RemoveAll(filepath.Join(dir, "orig"))
		_ = os.Remove(filepath.Join(dir, intentFile))
		return nil, fmt.Sprintf("the write-ahead log could not be written (%v); this edit is not logged", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "orig"), 0o755); err != nil {
		return fail(err)
	}
	for i, c := range p.Changes {
		fe := intentFileEntry{Path: c.Path, Kind: c.Kind(), BaseHash: edits.FileHash(c.Path)}
		if nb := c.NewBytes(); nb != nil {
			fe.NewHash = edits.HashBytes(nb)
		}
		// Keep the bytes now on disk (not the plan's, which may differ in BOM or EOL).
		if b, err := os.ReadFile(c.Path); err == nil {
			fe.Orig = strconv.Itoa(i)
			if err := os.WriteFile(filepath.Join(dir, "orig", fe.Orig), b, 0o600); err != nil {
				return fail(err)
			}
		}
		l.in.Files = append(l.in.Files, fe)
	}
	if err := l.save(); err != nil {
		return fail(err)
	}
	return l, ""
}

func readIntent(dir string) (intent, bool) {
	var in intent
	b, err := os.ReadFile(filepath.Join(dir, intentFile))
	if err != nil || json.Unmarshal(b, &in) != nil {
		return in, false
	}
	return in, true
}

// archiveIntent moves a leftover entry to recovered-<id>/ so it is reported once and kept.
func archiveIntent(dir string) (string, bool) {
	in, ok := readIntent(dir)
	if _, err := os.Stat(filepath.Join(dir, intentFile)); err != nil {
		return "", false
	}
	id := in.ID
	if !ok || id == "" {
		id = newID()
	}
	dst := filepath.Join(dir, "recovered-"+id)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", false
	}
	if os.Rename(filepath.Join(dir, intentFile), filepath.Join(dst, intentFile)) != nil {
		return "", false
	}
	_ = os.Rename(filepath.Join(dir, "orig"), filepath.Join(dst, "orig"))
	return dst, true
}

// Leftover describes an interrupted write found in dir ("" if none) and sets it aside so it is
// reported only once. It is skipped while the process that wrote it is still running.
func Leftover(dir, root string) string {
	if dir == "" {
		return ""
	}
	in, ok := readIntent(dir)
	if ok && processAlive(in.PID) {
		return ""
	}
	dst, moved := archiveIntent(dir)
	if !moved {
		return ""
	}
	if !ok {
		return fmt.Sprintf("an earlier run left an unreadable write log; it was kept in %s", dst)
	}
	var lines, restore []string
	for _, f := range in.Files {
		rel := edits.RelativeName(f.Path, root)
		cur := edits.FileHash(f.Path)
		state := "modified by something else"
		switch {
		case cur == f.BaseHash && cur == f.NewHash:
			state = "unchanged"
		case cur == f.BaseHash:
			state = "still the original"
		case cur == f.NewHash:
			state = "already has the new content"
		}
		lines = append(lines, fmt.Sprintf("  %s (%s; was %s, new %s): %s", rel, f.Kind, short(f.BaseHash), short(f.NewHash), state))
		switch {
		case f.Orig != "":
			restore = append(restore, fmt.Sprintf("cp %s %s", filepath.Join(dst, "orig", f.Orig), f.Path))
		case f.BaseHash == "":
			restore = append(restore, "rm "+f.Path)
		}
		// A kill between writeAtomic's temp file and its rename leaves that temp file; the log does not name it.
		for _, tmp := range leftoverTemps(f.Path) {
			lines = append(lines, fmt.Sprintf("  %s (temporary file of the interrupted write)", edits.RelativeName(tmp, root)))
			restore = append(restore, "rm "+tmp)
		}
	}
	verb := "applying"
	if in.Op == "undo" {
		verb = "undoing"
	}
	return fmt.Sprintf("an earlier run was interrupted while %s %q (started %s), so these files may be half-written:\n%s\nThe original contents were saved in %s. To roll back, run:\n  %s\n(or review them with git diff; hashes are the first 12 characters of sha256)",
		verb, in.Description, in.Started.Format(time.RFC3339), strings.Join(lines, "\n"), filepath.Join(dst, "orig"), strings.Join(restore, "\n  "))
}

var tempIDRE = regexp.MustCompile(`^[0-9a-f]+$`)

// leftoverTemps lists the temp files writeAtomic names "." + base + "." + id + ".tmp" beside path. Only a name of
// that exact shape counts, so an unrelated file is never offered for removal.
func leftoverTemps(path string) []string {
	dir, base := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	prefix, suffix := "."+base+".", ".tmp"
	var out []string
	for _, e := range entries {
		n := e.Name()
		if len(n) <= len(prefix)+len(suffix) || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		if id := n[len(prefix) : len(n)-len(suffix)]; tempIDRE.MatchString(id) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

func short(h string) string {
	if h == "" {
		return "absent"
	}
	return h[:12]
}
