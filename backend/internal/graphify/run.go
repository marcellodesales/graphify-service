// Package graphify runs the graphify CLI against a cloned repository (spec §11,
// PRD-002/003). The graphify binary is present in the worker image (built FROM
// the graphify runtime).
package graphify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ExtractOptions configures a graphify extraction.
type ExtractOptions struct {
	RepoDir  string
	CodeOnly bool // --code-only: local AST, no LLM key
	Force    bool // --force
	Timeout  time.Duration
}

// Extract runs `graphify extract <repoDir> [--code-only] [--force]`, writing
// graphify-out/ under repoDir. Returns a bounded tail of combined output.
func Extract(ctx context.Context, opts ExtractOptions) (string, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Minute
	}
	args := []string{"extract", opts.RepoDir}
	if opts.CodeOnly {
		args = append(args, "--code-only")
	}
	if opts.Force {
		args = append(args, "--force")
	}
	out, err := run(ctx, opts.RepoDir, opts.Timeout, args...)
	if err != nil {
		return out, fmt.Errorf("graphify extract: %v", err)
	}
	return out, nil
}

// EnrichResult reports which enrichment steps failed (best-effort; never fatal).
type EnrichResult struct {
	Failed []string
}

// Enrich runs the offline export steps after Extract to produce the UI-ready
// artifact set (PRD-003): GRAPH_REPORT.md + graph.html (cluster-only), graph.graphml,
// <repo>-callflow.html, graph.svg. All are best-effort and offline (no LLM key —
// cluster-only falls back to placeholder community names). graph.json already
// exists from Extract, so a failed step is logged, not fatal.
func Enrich(ctx context.Context, repoDir string, timeout time.Duration) EnrichResult {
	if timeout <= 0 {
		timeout = 60 * time.Minute
	}
	steps := [][]string{
		{"cluster-only", repoDir},            // GRAPH_REPORT.md, graph.html, community labels
		{"export", "graphml", repoDir},       // graph.graphml (Gephi/yEd/Cytoscape)
		{"export", "callflow-html", repoDir}, // <repo>-callflow.html (Mermaid)
		{"export", "svg", repoDir},           // graph.svg
	}
	var res EnrichResult
	for _, args := range steps {
		if _, err := run(ctx, repoDir, timeout, args...); err != nil {
			res.Failed = append(res.Failed, strings.Join(args, " "))
		}
	}
	return res
}

// run invokes the graphify CLI (no shell) in dir with a timeout, returning a
// bounded tail of combined stdout/stderr.
func run(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "graphify", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return tail(buf.String(), 16*1024), err
}

func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
