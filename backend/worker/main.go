// Command graphify-worker is the NATS-driven graphify worker (spec §11, PRD-003).
// It consumes cloned events, runs `graphify extract --code-only` (unless the repo
// already ships a committed graphify-out/), records the artifact inventory, and
// publishes a graph-ready event. It also serves the shared status protocol.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/artifacts"
	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/events"
	"github.com/marcellodesales/graphify-service/backend/internal/graphify"
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
	bus, err := events.Connect(cfg.NATSURL, "urn:graphify-service:worker")
	if err != nil {
		return err
	}
	defer bus.Close()

	w := &worker{cfg: cfg, logger: logger, store: store, bus: bus}
	sub, err := bus.Subscribe(events.SubjectCloned, events.DurableGraphWorker, cfg.RunTimeout+time.Minute, w.handle)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	logger.Info("graphify worker subscribed", "subject", events.SubjectCloned)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           statushttp.Mux("worker", store, statushttp.Ready(func() error { return busReady(bus) })),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("status server", "error", err)
		}
	}()
	logger.Info("graphify worker status listening", "addr", cfg.HTTPAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("graphify worker shutdown")
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
		w.logger.Error("graphify: metadata read failed", "id", id, "error", err)
		if errors.Is(err, repository.ErrNotFound) {
			return nil // ack to avoid poison loop
		}
		return err // nak to retry transient errors
	}
	if m.Status == repository.StatusReady {
		// Already done. Re-publish in case the previous ready publish didn't land
		// before ack (idempotent via Nats-Msg-Id) so pollers/consumers still see it.
		return w.bus.Publish(events.SubjectGraphReady, "graph-ready:"+id+":"+m.ResolvedSHA,
			events.RepoEventData{RepositoryID: id, ResolvedSHA: m.ResolvedSHA})
	}
	if m.Status != repository.StatusCloned && m.Status != repository.StatusGraphifying {
		// cloned event but metadata isn't cloned yet — retry shortly.
		return fmt.Errorf("id %s not in cloned state (%s)", id, m.Status)
	}

	if _, err := w.store.Update(id, func(md *repository.Metadata) error {
		md.Status = repository.StatusGraphifying
		md.Attempts.Graphify++
		now := time.Now().UTC()
		md.Timestamps.GraphifyStartedAt = &now
		return nil
	}); err != nil {
		w.logger.Error("graphify: begin", "id", id, "error", err)
		return nil
	}
	_ = w.bus.Publish(events.SubjectGraphStarted, "graph-started:"+id+":"+m.ResolvedSHA,
		events.RepoEventData{RepositoryID: id, ResolvedSHA: m.ResolvedSHA})

	repoDir := w.store.Layout().RepositoryDir(id)

	// Short-circuit if the repo already ships a committed graphify-out/.
	if !m.Source.HasCommittedGraph {
		logTail, err := graphify.Extract(context.Background(), graphify.ExtractOptions{
			RepoDir:  repoDir,
			CodeOnly: w.cfg.CodeOnly,
			Timeout:  w.cfg.RunTimeout,
		})
		if err != nil {
			w.logger.Error("graphify extract failed", "id", id, "tail", logTail)
			return w.fail(id, "graphify", err.Error())
		}
	} else {
		w.logger.Info("using committed graphify-out", "id", id)
	}

	inv, err := artifacts.Inventory(repoDir)
	if err != nil {
		return w.fail(id, "graphify", fmt.Sprintf("inventory: %v", err))
	}
	if len(inv) == 0 {
		return w.fail(id, "graphify", "no artifacts produced under graphify-out")
	}

	if _, err := w.store.Update(id, func(md *repository.Metadata) error {
		md.Status = repository.StatusReady
		md.Stage = "complete"
		md.Artifacts = inv
		now := time.Now().UTC()
		md.Timestamps.GraphifyFinishedAt = &now
		return nil
	}); err != nil {
		return w.fail(id, "graphify", fmt.Sprintf("persist ready: %v", err))
	}

	w.logger.Info("ready", "id", id, "artifacts", len(inv))
	if err := w.bus.Publish(events.SubjectGraphReady, "graph-ready:"+id+":"+m.ResolvedSHA,
		events.RepoEventData{RepositoryID: id, ResolvedSHA: m.ResolvedSHA}); err != nil {
		return err
	}
	return nil
}

func (w *worker) fail(id, stage, msg string) error {
	w.logger.Error("graphify failed", "id", id, "stage", stage, "msg", msg)
	_, _ = w.store.Update(id, func(md *repository.Metadata) error {
		md.Status = repository.StatusFailed
		md.Stage = stage
		md.Failure = &repository.Failure{Stage: stage, Code: "graphify_failed", Message: msg, At: time.Now().UTC()}
		return nil
	})
	_ = w.bus.Publish(events.SubjectGraphFailed, "graph-failed:"+id, events.RepoEventData{RepositoryID: id, Message: msg})
	return nil
}

func busReady(bus *events.Bus) error {
	if !bus.Connected() {
		return errors.New("nats not connected")
	}
	return nil
}
