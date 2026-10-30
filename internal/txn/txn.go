// Package txn writes an EditPlan to disk safely: guards, atomic apply with rollback, undo.
package txn

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

const DefaultMaxFiles = 40

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
		return toolerr.Inputf("%s is outside the workspace %s", path, root)
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

func (g WriteGuard) CheckPlan(p *edits.EditPlan, allowLarge bool) error {
	for _, c := range p.Changes {
		if err := g.CheckPath(c.Path); err != nil {
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
	if len(p.Changes) > limit && !allowLarge {
		return toolerr.Inputf("the edit touches %d files (limit %d); review the preview and pass allow_large=true to proceed", len(p.Changes), limit)
	}
	return nil
}

// Applied is a written edit that can be undone.
type Applied struct {
	ID            string
	Plan          *edits.EditPlan
	AppliedHashes map[string]string
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
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
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	_ = os.Chmod(name, mode)
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func writeChange(c edits.FileChange) error {
	if c.New == nil {
		if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeAtomic(c.Path, c.NewBytes())
}

func restore(c edits.FileChange) error {
	if c.Old == nil {
		if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeAtomic(c.Path, c.OldBytes())
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

	mu      sync.Mutex
	entries []*Applied
	max     int
}

func NewJournal(g WriteGuard) *Journal { return &Journal{Guard: g, max: 20} }

// Rebind re-targets at another workspace; earlier edits cannot be undone from here.
func (j *Journal) Rebind(g WriteGuard) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Guard, j.entries = g, nil
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
	if err := j.CheckFresh(p); err != nil {
		return nil, err
	}
	var written []edits.FileChange
	for _, c := range p.Changes {
		if err := writeChange(c); err != nil {
			var stuck []string
			for i := len(written) - 1; i >= 0; i-- {
				if restore(written[i]) != nil {
					stuck = append(stuck, edits.RelativeName(written[i].Path, j.Guard.Root))
				}
			}
			msg := fmt.Sprintf("writing %s failed (%v); ", edits.RelativeName(c.Path, j.Guard.Root), err)
			if len(stuck) > 0 {
				msg += "COULD NOT RESTORE: " + strings.Join(stuck, ", ")
			} else {
				msg += "no changes were kept"
			}
			return nil, toolerr.Inputf("%s", msg)
		}
		written = append(written, c)
	}
	for _, c := range p.Changes {
		if c.Kind() == "delete" {
			pruneEmptyDirs(c.Path, j.Guard.Root)
		}
	}
	e := &Applied{ID: newID(), Plan: p, AppliedHashes: map[string]string{}}
	for _, c := range p.Changes {
		e.AppliedHashes[c.Path] = edits.FileHash(c.Path)
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
			return nil, toolerr.Inputf("unknown edit id %q: no edits were applied by this server", id)
		}
		return nil, toolerr.Inputf("nothing to undo: no edits were applied by this server")
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
			return nil, toolerr.Inputf("unknown edit id %q (known: %s)", id, strings.Join(known, ", "))
		}
	}
	e := j.entries[idx]
	var stale []string
	for path, want := range e.AppliedHashes {
		if edits.FileHash(path) != want {
			stale = append(stale, edits.RelativeName(path, j.Guard.Root))
		}
	}
	if len(stale) > 0 {
		return nil, toolerr.Inputf("cannot undo %s: changed since it was applied: %s", e.ID, strings.Join(stale[:min(5, len(stale))], ", "))
	}
	inv := e.Plan.Inverse()
	if err := j.Guard.CheckWritesEnabled(readOnlyHint); err != nil {
		return nil, err
	}
	if err := j.Guard.CheckPlan(inv, true); err != nil {
		return nil, err
	}
	var written []edits.FileChange
	for _, c := range inv.Changes {
		if err := writeChange(c); err != nil {
			for i := len(written) - 1; i >= 0; i-- {
				_ = restore(written[i])
			}
			return nil, toolerr.Inputf("undo of %s failed (%v); nothing was reverted", e.ID, err)
		}
		written = append(written, c)
	}
	for _, c := range inv.Changes {
		if c.Kind() == "delete" {
			pruneEmptyDirs(c.Path, j.Guard.Root)
		}
	}
	j.entries = append(j.entries[:idx], j.entries[idx+1:]...)
	return e, nil
}

// PlanStore holds previewed plans waiting to be applied by id.
type PlanStore struct {
	mu    sync.Mutex
	ids   []string
	plans map[string]*edits.EditPlan
	used  map[string]bool // ids already applied
}

func NewPlanStore() *PlanStore {
	return &PlanStore{plans: map[string]*edits.EditPlan{}, used: map[string]bool{}}
}

func (s *PlanStore) Add(p *edits.EditPlan) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newID()
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
		if s.used[id] {
			return nil, toolerr.Inputf("preview %q was already applied; use undo_edit to revert it, or run the edit again", id)
		}
		return nil, toolerr.Inputf("unknown or expired preview id %q (pending: %s)", id, strings.Join(s.ids, ", "))
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

// MarkApplied drops the plan and remembers that its id was consumed.
func (s *PlanStore) MarkApplied(id string) {
	s.Discard(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used[id] = true
}

func (s *PlanStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans, s.ids, s.used = map[string]*edits.EditPlan{}, nil, map[string]bool{}
}
