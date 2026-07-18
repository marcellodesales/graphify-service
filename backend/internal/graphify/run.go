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
	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	args := []string{"extract", opts.RepoDir}
	if opts.CodeOnly {
		args = append(args, "--code-only")
	}
	if opts.Force {
		args = append(args, "--force")
	}

	cmd := exec.CommandContext(cctx, "graphify", args...)
	cmd.Dir = opts.RepoDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	logTail := tail(buf.String(), 16*1024)
	if err != nil {
		return logTail, fmt.Errorf("graphify extract: %v", err)
	}
	return logTail, nil
}

func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
