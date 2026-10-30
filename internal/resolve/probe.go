package resolve

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// ProbeDir is the directory below the root where a probe file lives. The file exists only as a
// gopls overlay while a lookup runs; nothing is ever written there.
const ProbeDir = ".codenav_probe"

// ErrNoProbe: the name has no package part to look up outside the workspace.
var ErrNoProbe = errors.New("not a package-qualified name")

// ProbeError is a package-qualified name that was looked up outside the workspace and not found.
type ProbeError struct{ Msg string }

func (e *ProbeError) Error() string { return e.Msg }

// probeTarget is one reading of a name: an import path and the symbol path inside the package.
type probeTarget struct {
	importPath string
	rest       []string // Name, or Type.Member
}

var identRE = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*$`)

// probeTargets reads query as <package>.<Name> or <package>.<Type>.<Member>. A query with a "/" names
// the package by import path (encoding/json.Decoder.Decode, gopkg.in/yaml.v3.Marshal: the dots after
// the last "/" may belong to the path, so each split is a candidate). A short qualifier (json.Marshal)
// maps to the imports of that name seen in the workspace (known: qualifier -> import paths) and to the
// standard library.
func probeTargets(query string, known map[string][]string) []probeTarget {
	var out []probeTarget
	seen := map[string]bool{}
	add := func(p string, rest []string) {
		if len(rest) < 1 || len(rest) > 2 || p == "" {
			return
		}
		for _, r := range rest {
			if !identRE.MatchString(r) {
				return
			}
		}
		if k := p + "|" + strings.Join(rest, "."); !seen[k] {
			seen[k] = true
			out = append(out, probeTarget{p, rest})
		}
	}
	if i := strings.LastIndex(query, "/"); i >= 0 {
		prefix, parts := query[:i], strings.Split(query[i+1:], ".")
		for k := 1; k < len(parts); k++ {
			add(prefix+"/"+strings.Join(parts[:k], "."), parts[k:])
		}
		return out
	}
	parts := strings.Split(query, ".")
	if len(parts) < 2 {
		return nil
	}
	for _, p := range known[parts[0]] {
		add(p, parts[1:])
	}
	if spec, _ := gosrc.StdlibImport(parts[0]); spec != "" {
		add(strings.Trim(spec, `"`), parts[1:])
	}
	return out
}

// Probe resolves a package-qualified name that is not in the workspace (a standard-library or
// dependency symbol) by asking gopls about a scratch file in <root>/.codenav_probe that imports the
// package and mentions the symbol. The file is a gopls overlay only and is gone when Probe returns.
func Probe(ctx context.Context, c *lsp.Client, root, query string, known map[string][]string) (Symbol, error) {
	targets := probeTargets(strings.TrimSpace(query), known)
	if len(targets) == 0 {
		return Symbol{}, ErrNoProbe
	}
	fullPath := strings.Contains(query, "/")
	var hits []Symbol
	var reasons []string
	for _, t := range targets {
		sym, reason, err := probeOne(ctx, c, root, t)
		if err != nil {
			return Symbol{}, err
		}
		if reason != "" {
			reasons = append(reasons, reason)
			continue
		}
		hits = append(hits, sym)
		if fullPath {
			break
		}
	}
	switch len(hits) {
	case 0:
		return Symbol{}, &ProbeError{strings.Join(reasons, "; ")}
	case 1:
		return hits[0], nil
	}
	var lines []string
	for _, h := range hits {
		lines = append(lines, fmt.Sprintf("  %s.%s  (package %s)", path.Base(h.ContainerName), h.Name, h.ContainerName))
	}
	return Symbol{}, toolerr.Inputf("%q matches in %d packages; use the full import path (e.g. %s.%s):\n%s", query, len(hits), hits[0].ContainerName, strings.Join(strings.Split(query, ".")[1:], "."), strings.Join(lines, "\n"))
}

func (t probeTarget) noSymbol() string {
	base := path.Base(t.importPath)
	suffix := ""
	if base != t.importPath {
		suffix = " (" + t.importPath + ")"
	}
	return fmt.Sprintf("package %s declares no %s%s", base, strings.Join(t.rest, "."), suffix)
}

// probeSource builds the probe file for t and the 1-based positions of the identifier to resolve,
// best reading first.
func probeSource(t probeTarget) (src string, pos [][2]int) {
	var lines []string
	// at registers the position of the last occurrence of name on the line just added.
	at := func(line, name string) {
		lines = append(lines, line)
		pos = append(pos, [2]int{len(lines) + 4, strings.LastIndex(line, name) + 1})
	}
	if len(t.rest) == 1 {
		at("var _ = x."+t.rest[0], t.rest[0])
		at("type _ = x."+t.rest[0], t.rest[0])
	} else {
		typ, mem := t.rest[0], t.rest[1]
		// v.M finds fields, promoted ones and methods of either receiver kind; (*T).M is the second try.
		at(fmt.Sprintf("var _ = func(v x.%s) { _ = v.%s }", typ, mem), mem)
		at(fmt.Sprintf("var _ = (*x.%s).%s", typ, mem), mem)
	}
	return fmt.Sprintf("package probe\n\nimport x %q\n\n%s\n", t.importPath, strings.Join(lines, "\n")), pos
}

// probeOne asks gopls where t is declared. reason is why not (empty on success).
func probeOne(ctx context.Context, c *lsp.Client, root string, t probeTarget) (sym Symbol, reason string, err error) {
	src, pos := probeSource(t)
	file := filepath.Join(root, ProbeDir, "p.go")
	name := t.rest[len(t.rest)-1]
	display := t.importPath + "." + strings.Join(t.rest, ".")
	err = c.Overlay(ctx, map[string]string{file: src}, func() error {
		for _, p := range pos {
			locs, derr := c.Definition(ctx, file, p[0], p[1])
			if derr != nil {
				return derr
			}
			if len(locs) > 0 {
				sym = Symbol{Name: name, URI: locs[0].URI, Line: locs[0].Range.Start.Line, Column: locs[0].Range.Start.Character, ContainerName: t.importPath}
				sym.Kind = kindAt(ctx, c, locs[0])
				return nil
			}
		}
		reason = probeReason(ctx, c, file, t, display)
		return nil
	})
	return sym, reason, err
}

// probeReason explains an unresolved probe from the diagnostics gopls gives the scratch file.
func probeReason(ctx context.Context, c *lsp.Client, file string, t probeTarget, display string) string {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	diags, _ := c.PullDiagnostics(dctx, lsp.PathToURI(file))
	for _, d := range diags {
		if strings.Contains(d.Message, "no required module") || strings.Contains(d.Message, "could not import") {
			return fmt.Sprintf("package %q cannot be loaded: no required module provides it (or it is not in the standard library); add it to go.mod (go get) or check the import path", t.importPath)
		}
	}
	for _, d := range diags {
		msg := strings.SplitN(d.Message, "\n", 2)[0]
		if strings.HasPrefix(msg, "undefined: x.") || strings.Contains(msg, "has no field or method") {
			return t.noSymbol()
		}
		return fmt.Sprintf("%s: %s", display, strings.ReplaceAll(msg, "x.", path.Base(t.importPath)+"."))
	}
	return t.noSymbol()
}

// kindAt finds the LSP symbol kind of the declaration at loc through its file's outline (0 if unknown).
func kindAt(ctx context.Context, c *lsp.Client, loc lsp.Location) int {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	syms, err := c.DocumentSymbol(dctx, lsp.URIToPath(loc.URI))
	if err != nil {
		return 0
	}
	var find func(list []lsp.Symbol) int
	find = func(list []lsp.Symbol) int {
		for i := range list {
			s := &list[i]
			if s.SelectionRange != nil && s.SelectionRange.Start == loc.Range.Start {
				return s.Kind
			}
			for _, ch := range s.Children {
				if k := find([]lsp.Symbol{*ch}); k != 0 {
					return k
				}
			}
		}
		return 0
	}
	return find(syms)
}
