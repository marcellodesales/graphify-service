// Package artifacts enumerates, filters, and packages graphify output files
// (PRD-003). Only non-dotfile regular files under graphify-out/ are ever
// surfaced — .graphify_* internal state, .git, and symlinks are excluded.
package artifacts

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// OutDir is the conventional graph output directory produced by graphify.
const OutDir = "graphify-out"

// DefaultNames is the curated default download set (PRD-003).
var DefaultNames = []string{"graph.json", "graph.html", "GRAPH_REPORT.md", "manifest.json"}

// Inventory scans <repoDir>/graphify-out and returns the allowlisted artifacts
// (name, relative path, media type, size, sha256), sorted by name. Dotfiles
// (e.g. .graphify_*), symlinks, and subdirectories are skipped.
func Inventory(repoDir string) ([]repository.Artifact, error) {
	outDir := filepath.Join(repoDir, OutDir)
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts: read %s: %w", OutDir, err)
	}
	var items []repository.Artifact
	for _, e := range entries {
		name := e.Name()
		if !allowedName(name) || !e.Type().IsRegular() {
			continue
		}
		full := filepath.Join(outDir, name)
		sum, size, err := hashFile(full)
		if err != nil {
			return nil, err
		}
		items = append(items, repository.Artifact{
			Name:      name,
			Path:      OutDir + "/" + name,
			MediaType: MediaType(name),
			Size:      size,
			SHA256:    sum,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// Select returns the subset of items to download given optional include/exclude
// name lists. With neither, it returns DefaultNames ∩ items. include/exclude are
// matched by base name; unknown names are ignored (never a path escape).
func Select(items []repository.Artifact, include, exclude []string) []repository.Artifact {
	inSet := toSet(include)
	exSet := toSet(exclude)

	var chosen []repository.Artifact
	for _, it := range items {
		switch {
		case len(inSet) > 0:
			if !inSet[it.Name] {
				continue
			}
		case len(exSet) > 0:
			if exSet[it.Name] {
				continue
			}
		default:
			if !isDefault(it.Name) {
				continue
			}
		}
		chosen = append(chosen, it)
	}
	return chosen
}

// Zip streams a zip archive of items (resolved under repoDir) to w. Every path
// is re-validated to stay within <repoDir>/graphify-out and symlinks are refused.
func Zip(w io.Writer, repoDir string, items []repository.Artifact) error {
	outDir, err := filepath.Abs(filepath.Join(repoDir, OutDir))
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, it := range items {
		if !allowedName(it.Name) {
			continue
		}
		full := filepath.Join(outDir, it.Name)
		abs, err := filepath.Abs(full)
		if err != nil || !within(outDir, abs) {
			continue
		}
		fi, err := os.Lstat(abs)
		if err != nil || !fi.Mode().IsRegular() { // refuse symlinks/dirs
			continue
		}
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: it.Path, Method: zip.Deflate}
		hdr.SetMode(0o644)
		zf, err := zw.CreateHeader(hdr)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(zf, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return nil
}

// MediaType returns a best-effort content type for an artifact name.
func MediaType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		return "application/json"
	case ".html":
		return "text/html; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// allowedName rejects dotfiles (.graphify_*, .git…) and path separators.
func allowedName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return false
	}
	return true
}

func isDefault(name string) bool {
	for _, d := range DefaultNames {
		if d == name {
			return true
		}
	}
	return false
}

func toSet(xs []string) map[string]bool {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			m[x] = true
		}
	}
	return m
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
