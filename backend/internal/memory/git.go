package memory

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// git identity used for memory commits. These are non-secret, service-owned
// values; commits are internal bookkeeping, not user-attributed history.
const (
	gitUserName  = "graphify-service"
	gitUserEmail = "graphify-service@localhost"
)

// InitRepo initializes the memory directory as a git repository if it is not
// already one. It is idempotent.
func InitRepo(ctx context.Context, dir string, timeout time.Duration) error {
	if _, err := gitRun(ctx, dir, timeout, "rev-parse", "--git-dir"); err == nil {
		return nil // already a repo
	}
	if _, err := gitRun(ctx, dir, timeout, "init", "-q"); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	return nil
}

// Commit stages everything git tracks (respecting .gitignore) and records a
// commit, then returns the new HEAD SHA. --allow-empty ensures the ref always
// advances on a mutation even when no tracked file changed, so callers can rely
// on the returned SHA as a fresh, monotonic memory ref.
func Commit(ctx context.Context, dir, message string, timeout time.Duration) (string, error) {
	if _, err := gitRun(ctx, dir, timeout, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}
	// -c sets identity for this invocation only, avoiding any dependency on the
	// container's global git config.
	if _, err := gitRun(ctx, dir, timeout,
		"-c", "user.name="+gitUserName,
		"-c", "user.email="+gitUserEmail,
		"commit", "--allow-empty", "-q", "-m", message,
	); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	return HeadSHA(ctx, dir, timeout)
}

// HeadSHA returns the current HEAD commit SHA.
func HeadSHA(ctx context.Context, dir string, timeout time.Duration) (string, error) {
	out, err := gitRun(ctx, dir, timeout, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// gitRun executes git in dir with a timeout and returns combined stdout.
// It shells out via exec (never a shell), mirroring the clone package.
func gitRun(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		tail := buf.String()
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(tail))
	}
	return buf.String(), nil
}
