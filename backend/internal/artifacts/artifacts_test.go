package artifacts

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"sort"
	"testing"

	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

const repoDir = "testdata/repo" // fixture: testdata/repo/graphify-out/*

// names produced by the worker's extract + enrich that must be surfaced.
var wantSurfaced = []string{
	"GRAPH_REPORT.md",
	"graph.graphml",
	"graph.html",
	"graph.json",
	"graph.svg",
	"manifest.json",
	"repo-callflow.html",
}

func TestInventorySurfacesFormatsAndHidesInternals(t *testing.T) {
	items, err := Inventory(repoDir)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.Name)
		// dotfiles + the cache/ subdir must never appear.
		if it.Name == ".graphify_analysis.json" || it.Name == "cache" || it.Name == "ast.json" {
			t.Errorf("internal artifact leaked into inventory: %s", it.Name)
		}
		if it.Size <= 0 {
			t.Errorf("%s: size not set", it.Name)
		}
		if len(it.SHA256) != 64 {
			t.Errorf("%s: sha256 = %q (want 64 hex)", it.Name, it.SHA256)
		}
		if it.Path != OutDir+"/"+it.Name {
			t.Errorf("%s: path = %q", it.Name, it.Path)
		}
	}
	sort.Strings(got)
	if len(got) != len(wantSurfaced) {
		t.Fatalf("inventory names = %v, want %v", got, wantSurfaced)
	}
	for i := range wantSurfaced {
		if got[i] != wantSurfaced[i] {
			t.Fatalf("inventory names = %v, want %v", got, wantSurfaced)
		}
	}
}

func TestSelect(t *testing.T) {
	items, _ := Inventory(repoDir)

	// Default = curated set that exists.
	def := names(Select(items, nil, nil))
	for _, want := range DefaultNames {
		if !contains(def, want) {
			t.Errorf("default download missing %s (got %v)", want, def)
		}
	}
	if contains(def, "graph.graphml") {
		t.Errorf("graph.graphml should not be in the default set (opt-in)")
	}

	// include = explicit subset.
	inc := names(Select(items, []string{"graph.graphml", "graph.svg"}, nil))
	if len(inc) != 2 || !contains(inc, "graph.graphml") || !contains(inc, "graph.svg") {
		t.Errorf("include select = %v, want [graph.graphml graph.svg]", inc)
	}

	// exclude drops a default member.
	exc := names(Select(items, nil, []string{"manifest.json"}))
	if contains(exc, "manifest.json") {
		t.Errorf("exclude did not drop manifest.json: %v", exc)
	}
}

func TestMediaType(t *testing.T) {
	cases := map[string]string{
		"graph.json":      "application/json",
		"graph.html":      "text/html; charset=utf-8",
		"graph.graphml":   "application/graphml+xml",
		"graph.svg":       "image/svg+xml",
		"GRAPH_REPORT.md": "text/markdown; charset=utf-8",
	}
	for name, want := range cases {
		if got := MediaType(name); got != want {
			t.Errorf("MediaType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestZipContainsOnlyAllowlisted(t *testing.T) {
	items, _ := Inventory(repoDir)
	var buf bytes.Buffer
	if err := Zip(&buf, repoDir, items); err != nil {
		t.Fatalf("Zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	var entries []string
	for _, f := range zr.File {
		entries = append(entries, f.Name)
		if filepath.Base(f.Name) == ".graphify_analysis.json" || filepath.Base(f.Name) == "ast.json" {
			t.Errorf("zip leaked internal file: %s", f.Name)
		}
		if !bytes.HasPrefix([]byte(f.Name), []byte(OutDir+"/")) {
			t.Errorf("zip entry not under %s/: %s", OutDir, f.Name)
		}
	}
	if len(entries) != len(items) {
		t.Errorf("zip has %d entries, want %d", len(entries), len(items))
	}
}

func names(items []repository.Artifact) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
