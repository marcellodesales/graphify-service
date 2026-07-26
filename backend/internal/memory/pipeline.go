package memory

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/artifacts"
	"github.com/marcellodesales/graphify-service/backend/internal/clone"
	"github.com/marcellodesales/graphify-service/backend/internal/giturl"
	"github.com/marcellodesales/graphify-service/backend/internal/graphify"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// IngestOptions carries the per-run knobs the ingestion + merge steps need,
// decoupled from the config package so this package stays importable by workers
// and tests without pulling in configuration.
type IngestOptions struct {
	CloneTimeout time.Duration
	RunTimeout   time.Duration
	CodeOnly     bool   // graphify extract --code-only (local AST, no LLM key)
	SSHKeyPath   string // absolute path to the deploy key for a private git resource
	KnownHosts   string // optional SSH known_hosts file
}

// WriteScaffold writes the committed, human-readable files that live at the root
// of a memory's git repo: README.md and .gitignore. It is idempotent (overwrites
// with current content) and safe to call on every mutation.
func WriteScaffold(l Layout, m Memory) error {
	dir := l.MemoryDir(m.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("memory: scaffold dir: %w", err)
	}
	// Raw clones and uploads are deliberately gitignored: they are large, may
	// carry their own nested .git, and are reproducible working data. Only the
	// manifest and the merged graphify-out/ are versioned.
	gitignore := "# graphify-service memory — raw working data is not versioned.\n" +
		"# Only memory.json (manifest) and graphify-out/ (merged graph) are committed,\n" +
		"# so the git HEAD SHA (GraphRef) is a stable pointer to the graph state.\n" +
		"/git/\n/files/\n/.tmp/\n"
	if err := os.WriteFile(l.GitignorePath(m.ID), []byte(gitignore), 0o644); err != nil {
		return fmt.Errorf("memory: write .gitignore: %w", err)
	}
	if err := os.WriteFile(l.ReadmePath(m.ID), []byte(renderReadme(m)), 0o644); err != nil {
		return fmt.Errorf("memory: write README: %w", err)
	}
	return nil
}

// renderReadme produces the committed README for a memory.
func renderReadme(m Memory) string {
	var b strings.Builder
	name := m.Name
	if strings.TrimSpace(name) == "" {
		name = m.ID
	}
	fmt.Fprintf(&b, "# Memory: %s\n\n", name)
	if strings.TrimSpace(m.Description) != "" {
		fmt.Fprintf(&b, "%s\n\n", m.Description)
	}
	fmt.Fprintf(&b, "- **ID:** `%s`\n", m.ID)
	fmt.Fprintf(&b, "- **Status:** `%s`\n\n", m.Status)
	b.WriteString("A *memory* is a user-defined collection of sources (one or more git ")
	b.WriteString("repositories plus uploaded files such as PDF/doc/markdown) that graphify ")
	b.WriteString("extracts independently and then merges into one unified knowledge graph.\n\n")
	b.WriteString("## Layout\n\n")
	b.WriteString("```\n")
	b.WriteString("memory.json      # the manifest (sources, status, artifacts)\n")
	b.WriteString("graphify-out/    # the merged unified graph (committed)\n")
	b.WriteString("  graph.json     # networkx node-link JSON, cross-source correlated\n")
	b.WriteString("git/             # raw clones (gitignored)\n")
	b.WriteString("files/           # raw uploads (gitignored)\n")
	b.WriteString("```\n\n")
	b.WriteString("The HEAD commit SHA of this repository is returned as the memory's ")
	b.WriteString("`graphRef` on every mutation.\n")
	return b.String()
}

// IngestGit clones a git resource into the memory's git/ tree and graphify-extracts
// it, returning the resource updated with its resolved SHA and graph paths. It
// reconstructs the clone target from the resource's non-secret Source metadata;
// the caller supplies any SSH key path via opts (never persisted in the manifest).
func IngestGit(ctx context.Context, opts IngestOptions, l Layout, memID string, r Resource) (Resource, error) {
	if err := os.MkdirAll(l.TmpRoot(), 0o750); err != nil {
		return r, fmt.Errorf("memory: tmp root: %w", err)
	}
	tmp, err := os.MkdirTemp(l.TmpRoot(), memID+"-")
	if err != nil {
		return r, fmt.Errorf("memory: tmp dir: %w", err)
	}
	_ = os.RemoveAll(tmp) // git clone needs the target absent

	res, err := clone.Run(ctx, clone.Options{
		Repo: giturl.Repo{
			Canonical: r.Source.NormalizedURL,
			Transport: giturl.Transport(r.Source.Transport),
		},
		Selector:   r.Selector,
		TmpDir:     tmp,
		SSHKeyPath: opts.SSHKeyPath,
		KnownHosts: opts.KnownHosts,
		Timeout:    opts.CloneTimeout,
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return r, fmt.Errorf("memory: clone: %w", err)
	}

	dest := l.GitResourceDir(memID, r.Source.Host, r.Source.OwnerPath, r.Source.Repository)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		_ = os.RemoveAll(tmp)
		return r, fmt.Errorf("memory: git dest parent: %w", err)
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return r, fmt.Errorf("memory: publish clone: %w", err)
	}

	// Skip extraction when the repo already ships a committed graphify-out/.
	if !res.HasCommittedGraph {
		if logTail, err := graphify.Extract(ctx, graphify.ExtractOptions{
			RepoDir:  dest,
			CodeOnly: opts.CodeOnly,
			Timeout:  opts.RunTimeout,
		}); err != nil {
			return r, fmt.Errorf("memory: graphify extract: %v (%s)", err, logTail)
		}
	}

	relPath := filepath.Join("git", r.Source.Host, filepath.FromSlash(r.Source.OwnerPath), r.Source.Repository)
	r.ResolvedSHA = res.ResolvedSHA
	r.Source.DefaultBranch = res.DefaultBranch
	r.Source.HasCommittedGraph = res.HasCommittedGraph
	r.Source.GraphOutPath = res.GraphOutPath
	r.RelPath = relPath
	r.GraphOutPath = filepath.Join(relPath, artifacts.OutDir)
	r.Status = repository.StatusReady
	r.Stage = "extracted"
	r.Failure = nil
	return r, nil
}

// IngestFile graphify-extracts an already-uploaded file resource. The uploaded
// bytes must already live at files/<resourceID>/ (written synchronously by the
// API handler); graphify's detect.py classifies and converts the file type
// (PDF/docx/xlsx/md/…) — this is extraction, NOT vector RAG.
func IngestFile(ctx context.Context, opts IngestOptions, l Layout, memID string, r Resource) (Resource, error) {
	dir := l.FileResourceDir(memID, r.ID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return r, fmt.Errorf("memory: file resource dir missing: %s", dir)
	}
	if logTail, err := graphify.Extract(ctx, graphify.ExtractOptions{
		RepoDir:  dir,
		CodeOnly: opts.CodeOnly,
		Timeout:  opts.RunTimeout,
	}); err != nil {
		return r, fmt.Errorf("memory: graphify extract: %v (%s)", err, logTail)
	}
	relPath := filepath.Join("files", r.ID)
	r.RelPath = relPath
	r.GraphOutPath = filepath.Join(relPath, artifacts.OutDir)
	r.Status = repository.StatusReady
	r.Stage = "extracted"
	r.Failure = nil
	return r, nil
}

// Assemble merges every ready resource graph into the memory's unified graph at
// graphify-out/graph.json, runs the cross-source correlation pass in place, then
// enriches (best-effort) and returns the artifact inventory.
//
// graphify's merge-graphs tags each input by its parent directory name, so the
// resource graphs (all named .../graphify-out/graph.json) are first copied into
// per-label staging directories under .tmp/ — the label becoming the `repo`
// attribute the correlator keys on. A single ready resource is copied directly
// (there is nothing to correlate across one source).
func Assemble(ctx context.Context, timeout time.Duration, l Layout, m Memory) ([]repository.Artifact, error) {
	memDir := l.MemoryDir(m.ID)

	var ready []Resource
	for _, r := range m.Resources {
		if r.GraphOutPath == "" {
			continue
		}
		gj := filepath.Join(memDir, r.GraphOutPath, "graph.json")
		if fi, err := os.Stat(gj); err == nil && fi.Mode().IsRegular() {
			ready = append(ready, r)
		}
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("memory: no resource graphs available to merge")
	}

	if err := os.MkdirAll(l.TmpRoot(), 0o750); err != nil {
		return nil, fmt.Errorf("memory: tmp root: %w", err)
	}
	stageRoot, err := os.MkdirTemp(l.TmpRoot(), "merge-")
	if err != nil {
		return nil, fmt.Errorf("memory: merge stage: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	inputs := make([]string, 0, len(ready))
	for _, r := range ready {
		labelDir := filepath.Join(stageRoot, resourceLabel(r))
		if err := os.MkdirAll(labelDir, 0o750); err != nil {
			return nil, fmt.Errorf("memory: stage dir: %w", err)
		}
		dst := filepath.Join(labelDir, "graph.json")
		if err := copyFile(filepath.Join(memDir, r.GraphOutPath, "graph.json"), dst); err != nil {
			return nil, fmt.Errorf("memory: stage graph: %w", err)
		}
		inputs = append(inputs, dst)
	}

	outDir := l.GraphOutDir(m.ID)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, fmt.Errorf("memory: graph out dir: %w", err)
	}
	out := filepath.Join(outDir, "graph.json")

	if len(inputs) >= 2 {
		if logTail, err := graphify.Merge(ctx, inputs, out, timeout); err != nil {
			return nil, fmt.Errorf("memory: merge-graphs: %v (%s)", err, logTail)
		}
	} else if err := copyFile(inputs[0], out); err != nil {
		return nil, fmt.Errorf("memory: copy single graph: %w", err)
	}

	// Best-effort cross-source correlation, written back only if it changed.
	if raw, err := os.ReadFile(out); err == nil {
		if merged, res, cerr := Correlate(raw); cerr == nil && (res.HubsAdded > 0 || res.LinksAdded > 0) {
			_ = os.WriteFile(out, merged, 0o644)
		}
	}

	// Best-effort UI-ready formats (graph.html, GRAPH_REPORT.md, graphml, svg).
	// Never fatal — graph.json already exists.
	_ = graphify.Enrich(ctx, memDir, timeout)

	inv, err := artifacts.Inventory(memDir)
	if err != nil {
		return nil, fmt.Errorf("memory: inventory: %w", err)
	}
	if len(inv) == 0 {
		return nil, fmt.Errorf("memory: no artifacts produced under graphify-out")
	}
	return inv, nil
}

// resourceLabel derives a merge tag / directory segment for a resource: the git
// repo name or the upload's base name, sanitized and suffixed with a short slice
// of the resource ID so two sources sharing a name never collide (which would
// otherwise collapse their graphs during merge and defeat correlation).
func resourceLabel(r Resource) string {
	var base string
	switch r.Kind {
	case KindGit:
		base = r.Source.Repository
	case KindFile:
		base = strings.TrimSuffix(r.FileName, filepath.Ext(r.FileName))
		if base == "" {
			base = r.FileName
		}
	}
	base = sanitizeLabel(base)
	if base == "" {
		base = "source"
	}
	short := r.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return base + "-" + short
}

// sanitizeLabel reduces s to a single safe path segment ([A-Za-z0-9._-]).
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// copyFile copies src to dst (truncating dst), creating parent dirs as needed.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
