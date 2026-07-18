// Command graphify-cloner is the NATS-driven clone worker (spec §9, PRD-002).
// It consumes clone-requested events, performs a secure shallow clone into the
// shared repos volume, records resolution metadata, and publishes a cloned
// event. It also serves the shared status protocol (/status/{id}).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/clone"
	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/events"
	"github.com/marcellodesales/graphify-service/backend/internal/giturl"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
	"github.com/marcellodesales/graphify-service/backend/internal/statushttp"
	"github.com/marcellodesales/graphify-service/backend/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger := telemetry.NewLogger(cfg.LogLevel)
	store, err := repository.NewStore(cfg.ReposRoot)
	if err != nil {
		return err
	}
	bus, err := events.Connect(cfg.NATSURL, "urn:graphify-service:cloner")
	if err != nil {
		return err
	}
	defer bus.Close()

	w := &worker{cfg: cfg, logger: logger, store: store, bus: bus}
	sub, err := bus.Subscribe(events.SubjectCloneRequested, events.DurableCloneWorker, w.handle)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	logger.Info("clone worker subscribed", "subject", events.SubjectCloneRequested)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           statushttp.Mux("cloner", store, statushttp.Ready(func() error { return busReady(bus) })),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("status server", "error", err)
		}
	}()
	logger.Info("clone worker status listening", "addr", cfg.HTTPAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("clone worker shutdown")
	return nil
}

type worker struct {
	cfg    config.Config
	logger *slog.Logger
	store  *repository.Store
	bus    *events.Bus
}

func (w *worker) handle(data events.RepoEventData) error {
	id := data.RepositoryID
	m, err := w.store.Get(id)
	if err != nil {
		w.logger.Error("clone: metadata not found", "id", id, "error", err)
		return nil // ack to avoid poison loop
	}

	// Idempotency: already past cloning → ack duplicate.
	switch m.Status {
	case repository.StatusCloned, repository.StatusGraphifying, repository.StatusReady:
		return nil
	}

	if _, err := w.store.Update(id, func(md *repository.Metadata) error {
		if md.Status != repository.StatusQueued && md.Status != repository.StatusCloning {
			return fmt.Errorf("unexpected status %q", md.Status)
		}
		md.Status = repository.StatusCloning
		md.Attempts.Clone++
		now := time.Now().UTC()
		md.Timestamps.CloneStartedAt = &now
		return nil
	}); err != nil {
		w.logger.Error("clone: begin", "id", id, "error", err)
		return nil
	}

	if err := os.MkdirAll(w.store.Layout().TmpRoot(), 0o750); err != nil {
		return w.fail(id, "clone", fmt.Sprintf("tmp root: %v", err))
	}
	tmp, err := os.MkdirTemp(w.store.Layout().TmpRoot(), id+"-")
	if err != nil {
		return w.fail(id, "clone", fmt.Sprintf("tmp dir: %v", err))
	}
	_ = os.RemoveAll(tmp) // git clone needs the target absent

	opts := clone.Options{
		Repo:     giturl.Repo{Canonical: m.Source.NormalizedURL, Transport: giturl.Transport(m.Source.Transport)},
		Selector: m.Selector,
		TmpDir:   tmp,
		Timeout:  w.cfg.CloneTimeout,
	}
	if m.Source.SSHKeyRef != "" {
		opts.SSHKeyPath = filepath.Join(w.cfg.SSHRoot, m.Source.SSHKeyRef)
		opts.KnownHosts = w.cfg.KnownHosts
	}

	res, err := clone.Run(context.Background(), opts)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return w.fail(id, "clone", err.Error())
	}

	repoDir := w.store.Layout().RepositoryDir(id)
	_ = os.RemoveAll(repoDir)
	if err := os.Rename(tmp, repoDir); err != nil {
		_ = os.RemoveAll(tmp)
		return w.fail(id, "clone", fmt.Sprintf("publish clone: %v", err))
	}

	m2, err := w.store.Update(id, func(md *repository.Metadata) error {
		md.Status = repository.StatusCloned
		md.ResolvedSHA = res.ResolvedSHA
		md.Source.DefaultBranch = res.DefaultBranch
		md.Source.HasCommittedGraph = res.HasCommittedGraph
		md.Source.GraphOutPath = res.GraphOutPath
		now := time.Now().UTC()
		md.Timestamps.CloneFinishedAt = &now
		return nil
	})
	if err != nil {
		return w.fail(id, "clone", fmt.Sprintf("persist cloned: %v", err))
	}

	w.logger.Info("cloned", "id", id, "sha", res.ResolvedSHA, "branch", res.DefaultBranch, "committedGraph", res.HasCommittedGraph)
	if err := w.bus.Publish(events.SubjectCloned, "repository-cloned:"+id+":"+m2.ResolvedSHA, events.RepoEventData{
		RepositoryID: id,
		ResolvedSHA:  m2.ResolvedSHA,
	}); err != nil {
		return err // metadata is durably 'cloned'; redelivery re-publishes
	}
	return nil
}

func (w *worker) fail(id, stage, msg string) error {
	w.logger.Error("clone failed", "id", id, "stage", stage, "msg", msg)
	_, _ = w.store.Update(id, func(md *repository.Metadata) error {
		md.Status = repository.StatusFailed
		md.Stage = stage
		md.Failure = &repository.Failure{Stage: stage, Code: "clone_failed", Message: msg, At: time.Now().UTC()}
		return nil
	})
	_ = w.bus.Publish(events.SubjectCloneFailed, "clone-failed:"+id, events.RepoEventData{RepositoryID: id, Message: msg})
	return nil // terminal; ack
}

func busReady(bus *events.Bus) error {
	if !bus.Connected() {
		return errors.New("nats not connected")
	}
	return nil
}
