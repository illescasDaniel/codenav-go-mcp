package lsp

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startFixture(t *testing.T) (*Client, string) {
	t.Helper()
	g, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not installed")
	}
	root, _ := filepath.Abs("../../testdata/fixture")
	c := New(Options{Root: root, Command: []string{g}, Language: "go", InitOptions: map[string]any{"symbolScope": "workspace"}})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	return c, root
}

func TestOverlayBreaksImporter(t *testing.T) {
	c, root := startFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	appURI, err := c.EnsureOpen("app/app.go")
	if err != nil {
		t.Fatal(err)
	}
	if d, err := c.PullDiagnostics(ctx, appURI); err != nil || len(d) != 0 {
		t.Fatalf("baseline diagnostics = %v, %v", d, err)
	}
	shapes := filepath.Join(root, "shapes/shapes.go")
	text, _ := readDocumentText(shapes)
	changed := strings.Replace(text, "func Scale(s Shape, k float64, unused int)", "func Scale(s Shape, k float64)", 1)
	err = c.Overlay(ctx, map[string]string{shapes: changed}, func() error {
		time.Sleep(1500 * time.Millisecond) // importers' diagnostics are pushed ~1s after the change
		d, err := c.PullDiagnostics(ctx, appURI)
		if err != nil {
			return err
		}
		if len(d) == 0 {
			t.Errorf("expected an error in app.go after changing Scale's signature in the overlay")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if d, _ := c.PullDiagnostics(ctx, appURI); len(d) != 0 {
		t.Errorf("overlay not undone: %v", d)
	}
}

func TestOverlayNewFile(t *testing.T) {
	c, root := startFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	newFile := filepath.Join(root, "shapes/extra.go")
	uri := PathToURI(newFile)
	err := c.Overlay(ctx, map[string]string{newFile: "package shapes\n\nvar Broken int = \"x\"\n"}, func() error {
		d, err := c.PullDiagnostics(ctx, uri)
		if err != nil {
			return err
		}
		if len(d) == 0 {
			t.Errorf("expected a type error in the overlay-only file")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
