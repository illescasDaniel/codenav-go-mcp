// Package txn writes an EditPlan to disk safely: guards, atomic apply with rollback, undo.
package txn

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

const DefaultMaxFiles = 40

// maxNameLen is NAME_MAX on Linux and most file systems: the longest legal file name, in bytes.
const maxNameLen = 255

// memoryNote says where previews and undo history live: only in the running server process.
const memoryNote = "previews and undo history live in memory in this server process and are gone after a restart"

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WriteGuard says what the write tools may touch.
type WriteGuard struct {
	Root     string
	ReadOnly bool
	MaxFiles int
	// Extra holds language-specific refusals (suffix, generated files, ...).
	Extra func(c edits.FileChange) error
	// NeverWrite are absolute directories no write may enter (GOROOT, GOMODCACHE).
	NeverWrite []string
}

func (g WriteGuard) CheckWritesEnabled(hint string) error {
	if g.ReadOnly {
		return toolerr.Inputf("write tools are disabled (%s is set); previews are still available with apply=false", hint)
	}
	return nil
}

func (g WriteGuard) CheckPath(path string) error {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return toolerr.Inputf("%s is a symlink; edit its target inside the workspace instead", edits.RelativeName(path, g.Root))
	}
	root, err := filepath.EvalSymlinks(g.Root)
	if err != nil {
		root = g.Root
	}
	resolved := path
	// Resolve through the deepest existing ancestor so symlinks leaving the workspace are caught.
	dir, base := filepath.Split(filepath.Clean(path))
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			rest, _ := filepath.Rel(d, filepath.Join(dir, base))
			resolved = filepath.Join(r, rest)
			break
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return toolerr.Inputf("%s", outsideMessage(path, g.Root, root))
	}
	// A symlinked directory inside the workspace would write the real file under another path
	// than the one the language server indexes. (A symlinked component of the root itself is fine:
	// both relative paths agree.)
	if given, err := filepath.Rel(g.Root, filepath.Clean(path)); err == nil && !strings.HasPrefix(given, "..") && given != rel {
		return toolerr.Inputf("%s goes through a symlinked directory; use %s", filepath.ToSlash(given), filepath.ToSlash(rel))
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range parts[:len(parts)-1] {
		if exclude.IsDirName(p) {
			return toolerr.Inputf("%s is inside %q, which the write tools never edit", filepath.ToSlash(rel), p)
		}
	}
	for _, d := range g.NeverWrite {
		if r, err := filepath.Rel(d, resolved); err == nil && !strings.HasPrefix(r, "..") {
			return toolerr.Inputf("%s is inside %s, which the write tools never edit", filepath.ToSlash(rel), d)
		}
	}
	return nil
}

// outsideMessage explains a write that would leave the workspace: the edit needs to change a file that
// lies in another module, outside the root the server navigates.
func outsideMessage(path, given, root string) string {
	shown := path
	if rel, err := filepath.Rel(given, path); err == nil {
		shown = filepath.ToSlash(rel)
	}
	msg := fmt.Sprintf("this edit would change %s, which is outside the workspace (a reference or declaration in a module or directory above the workspace root)", shown)
	for d := filepath.Dir(root); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
			return msg + fmt.Sprintf("; a go.work in %s covers it - set CODENAV_GO_MCP_WORKSPACE to %s so that the whole workspace is navigated", d, d)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return msg + "; point CODENAV_GO_MCP_WORKSPACE at a directory containing every module involved"
}

// checkWritable refuses a write over an existing file whose owner write bit is clear. The write is a rename, which
// the mode does not stop, so the bit is checked here: a file made read-only was made so on purpose.
func checkWritable(path, root string) error {
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&0o200 != 0 {
		return nil
	}
	return toolerr.Inputf("%s is read-only (mode %04o); make it writable (chmod u+w) first", edits.RelativeName(path, root), st.Mode().Perm())
}

func (g WriteGuard) CheckPlan(p *edits.EditPlan, allowLarge bool) error {
	for _, c := range p.Changes {
		if err := g.CheckPath(c.Path); err != nil {
			return err
		}
		if err := checkWritable(c.Path, g.Root); err != nil {
			return err
		}
		if g.Extra != nil {
			if err := g.Extra(c); err != nil {
				return err
			}
		}
	}
	limit := g.MaxFiles
	if limit == 0 {
		limit = DefaultMaxFiles
	}
	if g.OverLimit(p, allowLarge) {
		return toolerr.Inputf("the edit touches %d files (limit %d); pass allow_large=true to proceed", len(p.Changes), g.FileLimit())
	}
	return nil
}

// FileLimit is the most files one write may touch.
func (g WriteGuard) FileLimit() int {
	if g.MaxFiles == 0 {
		return DefaultMaxFiles
	}
	return g.MaxFiles
}

// OverLimit reports whether p touches more files than one write may, without allowLarge.
func (g WriteGuard) OverLimit(p *edits.EditPlan, allowLarge bool) bool {
	return len(p.Changes) > g.FileLimit() && !allowLarge
}

// LargeRefusal is the refusal for a plan over the file limit: it names the preview id that apply_edit takes.
func LargeRefusal(count, limit int, id string) string {
	return fmt.Sprintf("the edit touches %d files (limit %d). Preview id %s; call apply_edit(id=%q, allow_large=true) to write it anyway.", count, limit, id, id)
}

// Applied is a written edit that can be undone.
type Applied struct {
	ID            string
	Plan          *edits.EditPlan
	AppliedHashes map[string]string
	// CreatedDirs are the directories the edit created (deepest first): undo removes them once they are empty.
	CreatedDirs []string
}

// writeAtomic replaces path with data through a temp file beside it, renamed over path, so a crash leaves the old
// or the new file. An existing file keeps its mode. A new file gets mode when given, else 0644 less the umask, as
// os.WriteFile(path, data, 0644) gives (the kernel applies the umask when the temp file is created).
func writeAtomic(path string, data []byte, mode *os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, name, err := createTemp(path)
	if err != nil {
		return err
	}
	fail := func(err error) error { _ = tmp.Close(); _ = os.Remove(name); return err }
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if st, err := os.Stat(path); err == nil {
		if err := os.Chmod(name, st.Mode().Perm()); err != nil {
			return fail(err)
		}
	} else if mode != nil {
		if err := os.Chmod(name, *mode); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// tempName is the base name of a temp file beside a file named base: "." + base + "." + id + ".tmp" (the shape
// leftoverTemps recognises), or a short fixed form when that would be longer than a legal name.
func tempName(base, id string) string {
	if n := "." + base + "." + id + ".tmp"; len(n) <= maxNameLen {
		return n
	}
	return ".codenav-" + id + ".tmp"
}

// createTemp creates the temp file of writeAtomic beside path (os.CreateTemp would always give 0600). It is
// created with 0644, so a new file ends up 0644 less the umask, as os.WriteFile gives it.
func createTemp(path string) (*os.File, string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	var err error
	for range 10 {
		name := filepath.Join(dir, tempName(base, newID()))
		var f *os.File
		if f, err = os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
			return f, name, nil
		}
		if !os.IsExist(err) {
			break
		}
	}
	return nil, "", err
}

// cause is the reason in a file error without the path and the temp name, which make a long answer.
func cause(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

func writeChange(c edits.FileChange) error {
	if c.New == nil {
		if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeAtomic(c.Path, c.NewBytes(), c.Mode)
}

func restore(c edits.FileChange) error {
	if c.Old == nil {
		if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeAtomic(c.Path, c.OldBytes(), c.Mode)
}

// createdDirs lists the directories that writing p creates, deepest first within each chain, so that undo
// removes only what the edit made. Directories that exist already, the workspace root included, are not listed.
func createdDirs(p *edits.EditPlan, root string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range p.Changes {
		if c.New == nil {
			continue
		}
		for d := filepath.Dir(c.Path); d != root && strings.HasPrefix(d, root) && !seen[d]; d = filepath.Dir(d) {
			if _, err := os.Lstat(d); err == nil || !os.IsNotExist(err) {
				break
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func pruneEmptyDirs(path, stop string) {
	for d := filepath.Dir(path); d != stop && strings.HasPrefix(d, stop); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			return
		}
	}
}

// Journal applies plans and remembers the last few so they can be undone.
type Journal struct {
	Guard WriteGuard

	// LogDir holds the intent log (see beginIntent); "" disables it.
	LogDir string
	// Notify receives out-of-band messages (a degraded log, dropped history); may be nil.
	Notify func(msg string)

	mu         sync.Mutex
	entries    []*Applied
	undone     map[string]bool   // ids of edits reverted by Undo
	dropped    map[string]string // ids of edits dropped by a workspace switch -> the switch
	droppedIDs []string          // the same ids, oldest first (see remember)
	max        int
}

// maxDropped caps how many dropped ids are remembered for the "dropped by" answers; the oldest are forgotten.
const maxDropped = 200

// remember records id as dropped with reason, keeping the newest maxDropped ids. m is initialised here.
func remember(m map[string]string, order *[]string, id, reason string) {
	if m == nil {
		return
	}
	if _, seen := m[id]; !seen {
		*order = append(*order, id)
	}
	m[id] = reason
	for len(*order) > maxDropped {
		delete(m, (*order)[0])
		*order = (*order)[1:]
	}
}

func NewJournal(g WriteGuard) *Journal {
	return &Journal{Guard: g, max: 20, LogDir: DefaultLogDir(g.Root)}
}

func (j *Journal) notify(msg string) {
	if j.Notify != nil && msg != "" {
		j.Notify(msg)
	}
}

// Rebind re-targets at another workspace; earlier edits cannot be undone from here, which it says.
func (j *Journal) Rebind(g WriteGuard) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n := len(j.entries); n > 0 {
		j.notify(fmt.Sprintf("the workspace changed to %s: the undo history of %d edit(s) in %s was dropped and cannot be reverted with undo_edit", g.Root, n, j.Guard.Root))
	}
	if j.dropped == nil {
		j.dropped = map[string]string{}
	}
	for _, e := range j.entries {
		remember(j.dropped, &j.droppedIDs, e.ID, "workspace switch to "+g.Root)
	}
	j.Guard, j.entries, j.undone, j.LogDir = g, nil, nil, DefaultLogDir(g.Root)
}

// CheckPending reports (through Notify) a write that an earlier run left unfinished.
func (j *Journal) CheckPending() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if msg := Leftover(j.LogDir, j.Guard.Root); msg != "" {
		j.notify(msg)
	}
}

// Test seams.
var (
	restoreFn       = restore
	afterWritesHook func()
	// beforeWriteHook runs just before each file is written (after the lock and the freshness check).
	beforeWriteHook func(path string)
)

// checkUnchanged re-hashes the file right before it is written: the freshness check ran earlier and
// another process may have touched the file since.
func checkUnchanged(p *edits.EditPlan, c edits.FileChange) error {
	if beforeWriteHook != nil {
		beforeWriteHook(c.Path)
	}
	if want, ok := p.BaseHashes[c.Path]; ok && edits.FileHash(c.Path) != want {
		return fmt.Errorf("the file changed on disk just before it was written, by another process or editor")
	}
	return nil
}

// rollback undoes the written changes, newest first, and names those that could not be restored.
func (j *Journal) rollback(written []edits.FileChange) (stuck []string) {
	for i := len(written) - 1; i >= 0; i-- {
		if restoreFn(written[i]) != nil {
			stuck = append(stuck, edits.RelativeName(written[i].Path, j.Guard.Root))
		}
	}
	return stuck
}

func (j *Journal) History() []*Applied {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*Applied, len(j.entries))
	for i, e := range j.entries {
		out[len(out)-1-i] = e
	}
	return out
}

func (j *Journal) CheckFresh(p *edits.EditPlan) error {
	if err := j.checkFreshFiles(p); err != nil {
		return err
	}
	return j.checkFreshConfig(p)
}

// checkFreshConfig refuses a plan simulated under a Go configuration that has since changed: a go.mod, go.sum or
// go.work edit (or a GOWORK/GOFLAGS/GOTOOLCHAIN change) makes its type check stale even when every file it writes
// is unchanged. A plan without a recorded fingerprint is not checked.
func (j *Journal) checkFreshConfig(p *edits.EditPlan) error {
	if p.ConfigFP == "" || gomod.Fingerprint(j.Guard.Root) == p.ConfigFP {
		return nil
	}
	if !p.Previewed {
		return toolerr.Inputf("Go config changed while the edit was being applied (go.mod, go.sum, go.work or the Go environment). Run the refactoring again.")
	}
	return toolerr.Inputf("Go config changed since the preview (go.mod, go.sum, go.work or the Go environment). Run the refactoring again.")
}

// checkFreshFiles refuses a plan whose files changed since it was made.
func (j *Journal) checkFreshFiles(p *edits.EditPlan) error {
	var stale []string
	for path, want := range p.BaseHashes {
		if edits.FileHash(path) != want {
			stale = append(stale, edits.RelativeName(path, j.Guard.Root))
		}
	}
	if len(stale) > 0 {
		more := ""
		if len(stale) > 5 {
			more = fmt.Sprintf(" (+%d more)", len(stale)-5)
			stale = stale[:5]
		}
		if !p.Previewed {
			return toolerr.Inputf("changed while the edit was being applied (another writer got there first): %s%s. Run the refactoring again.", strings.Join(stale, ", "), more)
		}
		return toolerr.Inputf("changed since the preview was made: %s%s. Run the refactoring again.", strings.Join(stale, ", "), more)
	}
	return nil
}

func (j *Journal) Apply(p *edits.EditPlan, allowLarge bool, readOnlyHint string) (*Applied, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.Guard.CheckWritesEnabled(readOnlyHint); err != nil {
		return nil, err
	}
	if err := j.Guard.CheckPlan(p, allowLarge); err != nil {
		return nil, err
	}
	unlock, lnote, err := j.lockWrites()
	if err != nil {
		return nil, err
	}
	defer unlock()
	j.notify(lnote)
	if err := j.CheckFresh(p); err != nil {
		return nil, err
	}
	for i := range p.Changes {
		if st, err := os.Stat(p.Changes[i].Path); err == nil {
			m := st.Mode().Perm()
			p.Changes[i].Mode = &m
		}
	}
	created := createdDirs(p, j.Guard.Root)
	ilog, note := beginIntent(j.LogDir, "apply", p)
	j.notify(note)
	var written []edits.FileChange
	for _, c := range p.Changes {
		err := checkUnchanged(p, c)
		if err == nil {
			err = writeChange(c)
		}
		if err != nil {
			ilog.setState("rolling back")
			stuck := j.rollback(written)
			msg := fmt.Sprintf("writing %s failed (%v); ", edits.RelativeName(c.Path, j.Guard.Root), cause(err))
			if len(stuck) > 0 {
				msg += "COULD NOT RESTORE: " + strings.Join(stuck, ", ")
			} else {
				msg += "no changes were kept"
				ilog.done()
			}
			return nil, toolerr.Inputf("%s", msg)
		}
		written = append(written, c)
	}
	ilog.done()
	if afterWritesHook != nil {
		afterWritesHook()
	}
	for _, c := range p.Changes {
		if c.Kind() == "delete" {
			pruneEmptyDirs(c.Path, j.Guard.Root)
		}
	}
	e := &Applied{ID: newID(), Plan: p, AppliedHashes: map[string]string{}, CreatedDirs: created}
	for _, c := range p.Changes {
		// Hash what we wrote, not what is on disk now: a concurrent writer must not look like ours.
		e.AppliedHashes[c.Path] = ""
		if nb := c.NewBytes(); nb != nil {
			e.AppliedHashes[c.Path] = edits.HashBytes(nb)
		}
	}
	j.entries = append(j.entries, e)
	if len(j.entries) > j.max {
		j.entries = j.entries[1:]
	}
	return e, nil
}

// Undo reverts an applied edit (latest by default); refuses when its files changed afterwards.
func (j *Journal) Undo(id, readOnlyHint string) (*Applied, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.entries) == 0 {
		if id != "" {
			if j.undone[id] {
				return nil, toolerr.Inputf("edit %q was already undone; no other edits are left to undo", id)
			}
			if reason, ok := j.dropped[id]; ok {
				return nil, droppedEdit(id, reason)
			}
			return nil, toolerr.Inputf("unknown edit id %q: no edits were applied by this server (or all were undone); pass the id an applied edit reported, or run an edit first; %s", id, memoryNote)
		}
		return nil, toolerr.Inputf("nothing to undo: no edits were applied by this server; %s", memoryNote)
	}
	idx := len(j.entries) - 1
	if id != "" {
		idx = -1
		var known []string
		for i, e := range j.entries {
			known = append(known, e.ID)
			if e.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			if reason, ok := j.dropped[id]; ok {
				return nil, droppedEdit(id, reason)
			}
			msg := fmt.Sprintf("unknown edit id %q (known: %s", id, strings.Join(known, ", "))
			var gone []string
			for u := range j.undone {
				gone = append(gone, u)
			}
			sort.Strings(gone)
			if len(gone) > 0 {
				msg += "; already undone: " + strings.Join(gone, ", ")
			}
			return nil, toolerr.Inputf("%s); pass one of the known ids, or omit id to undo the latest edit; %s", msg, memoryNote)
		}
	}
	e := j.entries[idx]
	unlock, lnote, err := j.lockWrites()
	if err != nil {
		return nil, err
	}
	defer unlock()
	j.notify(lnote)
	var stale []string
	for path, want := range e.AppliedHashes {
		if edits.FileHash(path) != want {
			stale = append(stale, edits.RelativeName(path, j.Guard.Root))
		}
	}
	if len(stale) > 0 {
		msg := fmt.Sprintf("cannot undo %s: changed since it was applied: %s", e.ID, strings.Join(stale[:min(5, len(stale))], ", "))
		// A later edit that touched the same files is the usual cause: say which to undo first.
		var later []string
		for i := len(j.entries) - 1; i > idx; i-- {
			for path := range e.AppliedHashes {
				if _, ok := j.entries[i].AppliedHashes[path]; ok {
					later = append(later, fmt.Sprintf("%s (%s)", j.entries[i].ID, j.entries[i].Plan.Description))
					break
				}
			}
		}
		if len(later) > 0 {
			msg += "; later edit(s) touched the same files, undo them first (latest first): " + strings.Join(later, ", ")
		}
		return nil, toolerr.Inputf("%s", msg)
	}
	inv := e.Plan.Inverse()
	if err := j.Guard.CheckWritesEnabled(readOnlyHint); err != nil {
		return nil, err
	}
	if err := j.Guard.CheckPlan(inv, true); err != nil {
		return nil, err
	}
	ilog, note := beginIntent(j.LogDir, "undo", inv)
	j.notify(note)
	var written []edits.FileChange
	for _, c := range inv.Changes {
		err := checkUnchanged(inv, c)
		if err == nil {
			err = writeChange(c)
		}
		if err != nil {
			ilog.setState("rolling back")
			stuck := j.rollback(written)
			msg := fmt.Sprintf("undo of %s failed (%v); ", e.ID, cause(err))
			if len(stuck) > 0 {
				msg += "COULD NOT RESTORE: " + strings.Join(stuck, ", ")
			} else {
				msg += "nothing was reverted"
				ilog.done()
			}
			return nil, toolerr.Inputf("%s", msg)
		}
		written = append(written, c)
	}
	ilog.done()
	// Only the directories the edit created: a directory the user made stays, empty or not.
	for _, d := range e.CreatedDirs {
		_ = os.Remove(d)
	}
	j.entries = append(j.entries[:idx], j.entries[idx+1:]...)
	if j.undone == nil {
		j.undone = map[string]bool{}
	}
	j.undone[e.ID] = true
	return e, nil
}

// droppedEdit refuses an undo of an edit that a workspace switch dropped from the undo history.
func droppedEdit(id, reason string) error {
	return toolerr.Inputf("edit %q was dropped by the %s; undo_edit cannot revert it in this workspace", id, reason)
}

// WasUndone reports whether the edit with this id was reverted by Undo.
func (j *Journal) WasUndone(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.undone[id]
}

// PlanStore holds previewed plans waiting to be applied by id.
type PlanStore struct {
	mu    sync.Mutex
	ids   []string
	plans map[string]*edits.EditPlan
	used  map[string]string // preview ids already applied -> the edit id they became
	// dropped holds the preview ids that Clear dropped -> the reason it gave; droppedIDs keeps them oldest first.
	dropped    map[string]string
	droppedIDs []string
}

func NewPlanStore() *PlanStore {
	return &PlanStore{plans: map[string]*edits.EditPlan{}, used: map[string]string{}, dropped: map[string]string{}}
}

func (s *PlanStore) Add(p *edits.EditPlan) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(p)
}

// PreviewID is the id of p in the store: its id when it is stored already, else the id of a new preview.
func (s *PlanStore) PreviewID(p *edits.EditPlan) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, q := range s.plans {
		if q == p {
			return id
		}
	}
	return s.addLocked(p)
}

func (s *PlanStore) addLocked(p *edits.EditPlan) string {
	id := newID()
	p.Previewed = true
	s.plans[id] = p
	s.ids = append(s.ids, id)
	if len(s.ids) > 20 {
		delete(s.plans, s.ids[0])
		s.ids = s.ids[1:]
	}
	return id
}

func (s *PlanStore) Get(id string) (*edits.EditPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		if eid, done := s.used[id]; done {
			return nil, toolerr.Inputf("preview %q was already applied as edit %s; undo_edit(id=%q) reverts it", id, eid, eid)
		}
		if reason, gone := s.dropped[id]; gone {
			return nil, toolerr.Inputf("preview %q was dropped by the %s; run the refactoring again in this workspace for a fresh preview", id, reason)
		}
		if len(s.ids) == 0 {
			return nil, toolerr.Inputf("unknown or expired preview id %q (no previews are pending); run edit with apply=false first to get a preview id, or use undo_edit with an applied edit id; %s", id, memoryNote)
		}
		return nil, toolerr.Inputf("unknown or expired preview id %q (pending: %s); run edit with apply=false first to get a preview id, or use undo_edit with an applied edit id; %s", id, strings.Join(s.ids, ", "), memoryNote)
	}
	return p, nil
}

func (s *PlanStore) Discard(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.plans, id)
	for i, x := range s.ids {
		if x == id {
			s.ids = append(s.ids[:i], s.ids[i+1:]...)
			break
		}
	}
}

// AppliedAs is the edit id a consumed preview id became.
func (s *PlanStore) AppliedAs(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	eid, ok := s.used[id]
	return eid, ok
}

// MarkApplied drops the plan and remembers that its id was consumed, and as which edit.
func (s *PlanStore) MarkApplied(id, editID string) {
	s.Discard(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used[id] = editID
}

// Clear drops every pending preview. reason says why (for example "workspace switch to <root>"); a later
// apply_edit of a dropped id says so.
func (s *PlanStore) Clear(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped == nil {
		s.dropped = map[string]string{}
	}
	for _, id := range s.ids {
		remember(s.dropped, &s.droppedIDs, id, reason)
	}
	s.plans, s.ids, s.used = map[string]*edits.EditPlan{}, nil, map[string]string{}
}
