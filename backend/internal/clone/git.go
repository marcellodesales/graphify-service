// Package clone performs secure, shallow Git clones for the clone worker
// (spec §9, PRD-002). Git is invoked via os/exec — never through a shell.
package clone

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/artifacts"
	"github.com/marcellodesales/graphify-service/backend/internal/giturl"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// Options configures a clone.
type Options struct {
	Repo       giturl.Repo
	Selector   repository.Selector
	TmpDir     string // clone target (must not exist yet)
	SSHKeyPath string // optional; enables GIT_SSH_COMMAND
	KnownHosts string // optional; UserKnownHostsFile
	Timeout    time.Duration
}

// Result reports what was cloned.
type Result struct {
	ResolvedSHA       string
	DefaultBranch     string
	HasCommittedGraph bool
	GraphOutPath      string
}

// Run clones per the selector into opts.TmpDir and returns the resolved commit.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	url := opts.Repo.Canonical
	switch opts.Selector.Type {
	case repository.SelectorSHA:
		if err := fetchSHA(cctx, opts, url); err != nil {
			return Result{}, err
		}
	case repository.SelectorRef:
		if err := run(cctx, opts, "", "clone", "--depth", "1", "--single-branch", "--no-tags",
			"--branch", opts.Selector.Value, url, opts.TmpDir); err != nil {
			return Result{}, err
		}
	default: // default branch
		if err := run(cctx, opts, "", "clone", "--depth", "1", "--single-branch", "--no-tags",
			url, opts.TmpDir); err != nil {
			return Result{}, err
		}
	}

	sha, err := out(cctx, opts, opts.TmpDir, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	branch, _ := out(cctx, opts, opts.TmpDir, "rev-parse", "--abbrev-ref", "HEAD")

	res := Result{ResolvedSHA: strings.TrimSpace(sha), DefaultBranch: strings.TrimSpace(branch)}
	if fi, err := os.Stat(filepath.Join(opts.TmpDir, artifacts.OutDir)); err == nil && fi.IsDir() {
		res.HasCommittedGraph = true
		res.GraphOutPath = artifacts.OutDir
	}
	return res, nil
}

// fetchSHA implements the controlled SHA-only fetch (spec §9.4).
func fetchSHA(ctx context.Context, opts Options, url string) error {
	if err := os.MkdirAll(opts.TmpDir, 0o750); err != nil {
		return err
	}
	steps := [][]string{
		{"init", opts.TmpDir},
		{"-C", opts.TmpDir, "remote", "add", "origin", url},
		{"-C", opts.TmpDir, "fetch", "--depth", "1", "--no-tags", "origin", opts.Selector.Value},
		{"-C", opts.TmpDir, "checkout", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if err := run(ctx, opts, "", args...); err != nil {
			return fmt.Errorf("the remote may not allow fetching this commit directly; supply a branch/tag containing it: %w", err)
		}
	}
	return nil
}

func run(ctx context.Context, opts Options, _ string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv(opts)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func out(ctx context.Context, opts Options, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv(opts)
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return string(b), nil
}

func gitEnv(opts Options) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if opts.SSHKeyPath != "" {
		ssh := "ssh -i " + opts.SSHKeyPath + " -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes"
		if opts.KnownHosts != "" {
			ssh += " -o UserKnownHostsFile=" + opts.KnownHosts
		}
		env = append(env, "GIT_SSH_COMMAND="+ssh)
	}
	return env
}
