// Command graphify-memory-worker is the NATS-driven worker for the memory
// abstraction (multi-source unified graph). It runs two durable consumers:
//
//   - resource.requested → clone/stage the source, run `graphify extract`, and
//     persist the resource's own graphify-out/graph.json. When every resource in
//     the memory is ready it publishes merge.requested.
//   - merge.requested   → merge every ready resource graph into the memory's
//     unified graphify-out/graph.json (graphify merge-graphs + cross-source
//     correlation + enrich), commit the memory repo, and publish merge.ready.
//
// It also serves the shared status protocol (/status/{id}) resolved against the
// memory store. Mirrors the single-repo cloner/worker drivers: idempotent on
// redelivery, ack poison messages, nak transient errors, best-effort commits.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/events"
	"github.com/marcellodesales/graphify-service/backend/internal/memory"
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
	memStore, err := memory.NewStore(cfg.MemoriesRoot())
	if err != nil {
		return err
	}
	bus, err := events.Connect(cfg.NATSURL, "urn:graphify-service:memory-worker")
	if err != nil {
		return err
	}
	defer bus.Close()

	w := &worker{cfg: cfg, logger: logger, memStore: memStore, bus: bus}

	// Ingest one resource at a time; a clone+extract can be long, so give the
	// consumer a generous ack window over the clone timeout.
	resSub, err := bus.SubscribeMemory(events.SubjectMemoryResourceRequested, events.DurableMemoryResourceWorker,
		cfg.CloneTimeout+w.runTimeout()+time.Minute, w.handleResource)
	if err != nil {
		return err
	}
	defer resSub.Unsubscribe()

	mergeSub, err := bus.SubscribeMemory(events.SubjectMemoryMergeRequested, events.DurableMemoryMergeWorker,
		w.runTimeout()+time.Minute, w.handleMerge)
	if err != nil {
		return err
	}
	defer mergeSub.Unsubscribe()
	logger.Info("memory worker subscribed",
		"resource_subject", events.SubjectMemoryResourceRequested,
		"merge_subject", events.SubjectMemoryMergeRequested)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           statushttp.MemoryMux("memory-worker", memStore, statushttp.Ready(func() error { return busReady(bus) })),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("status server", "error", err)
		}
	}()
	logger.Info("memory worker status listening", "addr", cfg.HTTPAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("memory worker shutdown")
	return nil
}

type worker struct {
	cfg      config.Config
	logger   *slog.Logger
	memStore *memory.Store
	bus      *events.Bus
}

// runTimeout is the per-extract/merge budget: the memory-specific timeout if set,
// else the shared run timeout.
func (w *worker) runTimeout() time.Duration {
	if w.cfg.MemoryTimeout > 0 {
		return w.cfg.MemoryTimeout
	}
	return w.cfg.RunTimeout
}

// handleResource ingests a single source (git clone + extract, or file extract),
// persists it, and requests a merge once the whole memory is ready.
func (w *worker) handleResource(data events.MemoryEventData) error {
	id, rid := data.MemoryID, data.ResourceID
	m, err := w.memStore.Get(id)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return nil // poison — ack to avoid a redelivery loop
		}
		return err // transient (disk/lock) — nak to retry
	}

	res, ok := findResource(m, rid)
	if !ok {
		w.logger.Error("memory resource not found", "memory", id, "resource", rid)
		return nil // poison — ack
	}

	// Idempotency on redelivery: already ingested. Try to advance to merge in case
	// the previous merge request didn't land before ack.
	if res.Status == repository.StatusReady {
		return w.maybeRequestMerge(id)
	}

	opName := "ingest"
	if res.Kind == memory.KindGit {
		opName = "clone+extract"
	} else if res.Kind == memory.KindFile {
		opName = "extract"
	}
	if _, err := w.memStore.UpdateResource(id, rid, func(r *memory.Resource) error {
		r.Status = repository.StatusGraphifying
		r.Stage = "ingesting"
		r.LastOperation = &repository.Operation{
			Name:      opName,
			Status:    repository.OperationRunning,
			StartedAt: time.Now().UTC(),
		}
		return nil
	}); err != nil {
		w.logger.Error("memory: begin resource", "memory", id, "resource", rid, "error", err)
		return nil
	}
	w.setMemoryStatus(id, memory.StatusIngesting, "ingesting")

	opts := memory.IngestOptions{
		CloneTimeout: w.cfg.CloneTimeout,
		RunTimeout:   w.runTimeout(),
		CodeOnly:     w.cfg.CodeOnly,
	}
	// Resolve the SSH deploy key for a private git resource. The clone "task"
	// carries the {repo, ref, key} combo. Precedence, highest first:
	//   1. a caller-supplied key stored per-resource (legacy inline sshKey), then
	//   2. a memory-scoped provisioned key referenced by id (Source.KeyID → the
	//      .ssh/keys/<keyId> material, reusable across resources and rotatable), then
	//   3. an ops-provisioned key referenced by name on the read-only SSH volume.
	// On-disk presence is the source of truth — the key travels with the resource.
	if res.Kind == memory.KindGit {
		l := w.memStore.Layout()
		switch {
		case memory.HasResourceSSHKey(l, id, rid):
			opts.SSHKeyPath = l.SSHKeyPath(id, rid)
			if memory.HasResourceKnownHosts(l, id, rid) {
				opts.KnownHosts = l.SSHKnownHostsPath(id, rid)
			} else {
				opts.KnownHosts = w.cfg.KnownHosts
			}
		case res.Source.KeyID != "" && memory.HasMemoryKey(l, id, res.Source.KeyID):
			opts.SSHKeyPath = l.KeyPath(id, res.Source.KeyID)
			if memory.HasMemoryKeyKnownHosts(l, id, res.Source.KeyID) {
				opts.KnownHosts = l.KeyKnownHostsPath(id, res.Source.KeyID)
			} else {
				opts.KnownHosts = w.cfg.KnownHosts
			}
		case res.Source.SSHKeyRef != "":
			opts.SSHKeyPath = filepath.Join(w.cfg.SSHRoot, res.Source.SSHKeyRef)
			opts.KnownHosts = w.cfg.KnownHosts
		}
	}

	var updated memory.Resource
	switch res.Kind {
	case memory.KindGit:
		updated, err = memory.IngestGit(context.Background(), opts, w.memStore.Layout(), id, res)
	case memory.KindFile:
		updated, err = memory.IngestFile(context.Background(), opts, w.memStore.Layout(), id, res)
	default:
		return w.failResource(id, rid, fmt.Sprintf("unknown resource kind %q", res.Kind))
	}
	if err != nil {
		return w.failResource(id, rid, err.Error())
	}

	if _, err := w.memStore.UpdateResource(id, rid, func(r *memory.Resource) error {
		started := time.Now().UTC()
		if r.LastOperation != nil {
			started = r.LastOperation.StartedAt
		}
		opName := "ingest"
		if r.LastOperation != nil {
			opName = r.LastOperation.Name
		}
		*r = updated
		finished := time.Now().UTC()
		r.LastOperation = &repository.Operation{
			Name:       opName,
			Status:     repository.OperationSucceeded,
			StartedAt:  started,
			FinishedAt: &finished,
		}
		return nil
	}); err != nil {
		return w.failResource(id, rid, fmt.Sprintf("persist resource: %v", err))
	}
	w.commitMemory(id, "memory: ingested "+string(updated.Kind)+" resource "+rid)

	if err := w.bus.PublishMemory(events.SubjectMemoryResourceReady,
		"memory-resource-ready:"+id+":"+rid,
		events.MemoryEventData{MemoryID: id, ResourceID: rid, ResolvedSHA: updated.ResolvedSHA}); err != nil {
		w.logger.Error("publish memory.resource.ready", "memory", id, "resource", rid, "error", err)
	}

	w.logger.Info("memory resource ready", "memory", id, "resource", rid, "kind", updated.Kind)
	return w.maybeRequestMerge(id)
}

// maybeRequestMerge publishes merge.requested when every resource in the memory
// has reached ready. The message id fingerprints the ready set (resource ids +
// resolved SHAs) so re-merges after a change get a fresh id while an unchanged
// set is deduplicated within the JetStream window.
func (w *worker) maybeRequestMerge(id string) error {
	m, err := w.memStore.Get(id)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return nil
		}
		return err
	}
	if len(m.Resources) == 0 {
		return nil
	}
	for _, r := range m.Resources {
		if r.Status != repository.StatusReady {
			return nil // not all ready yet
		}
	}
	msgID := "memory-merge-request:" + id + ":" + mergeFingerprint(m.Resources)
	if err := w.bus.PublishMemory(events.SubjectMemoryMergeRequested, msgID,
		events.MemoryEventData{MemoryID: id}); err != nil {
		w.logger.Error("publish memory.merge.requested", "memory", id, "error", err)
		return err // nak: the manifest is durable, redelivery re-requests
	}
	return nil
}

// handleMerge merges every ready resource graph into the memory's unified graph.
func (w *worker) handleMerge(data events.MemoryEventData) error {
	id := data.MemoryID
	m, err := w.memStore.Get(id)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return nil
		}
		return err
	}
	if len(m.Resources) == 0 {
		return nil // nothing to merge — ack
	}
	// Guard against a stale merge request fired before a later-added resource
	// finished: only merge when the whole set is ready.
	for _, r := range m.Resources {
		if r.Status != repository.StatusReady {
			w.logger.Info("memory merge deferred; resource not ready",
				"memory", id, "resource", r.ID, "status", r.Status)
			return nil
		}
	}

	if _, err := w.memStore.Update(id, func(md *memory.Memory) error {
		md.Status = memory.StatusMerging
		md.Stage = "merging"
		now := time.Now().UTC()
		md.Timestamps.MergeStartedAt = &now
		md.LastOperation = &repository.Operation{
			Name:      "merge",
			Status:    repository.OperationRunning,
			StartedAt: now,
		}
		return nil
	}); err != nil {
		w.logger.Error("memory: begin merge", "memory", id, "error", err)
		return nil
	}

	inv, err := memory.Assemble(context.Background(), w.runTimeout(), w.memStore.Layout(), m)
	if err != nil {
		return w.failMerge(id, err.Error())
	}

	if _, err := w.memStore.Update(id, func(md *memory.Memory) error {
		md.Status = memory.StatusReady
		md.Stage = "complete"
		md.Artifacts = inv
		md.Failure = nil
		now := time.Now().UTC()
		md.Timestamps.MergeFinishedAt = &now
		started := now
		if md.LastOperation != nil {
			started = md.LastOperation.StartedAt
		}
		md.LastOperation = &repository.Operation{
			Name:       "merge",
			Status:     repository.OperationSucceeded,
			StartedAt:  started,
			FinishedAt: &now,
		}
		return nil
	}); err != nil {
		return w.failMerge(id, fmt.Sprintf("persist ready: %v", err))
	}

	ref := w.commitMemory(id, "memory: merged unified graph")
	w.logger.Info("memory ready", "memory", id, "artifacts", len(inv), "ref", ref)

	if err := w.bus.PublishMemory(events.SubjectMemoryMergeReady,
		"memory-merge-ready:"+id+":"+ref,
		events.MemoryEventData{MemoryID: id, GraphRef: ref}); err != nil {
		return err // metadata is durably ready; redelivery re-publishes
	}
	return nil
}

// failResource marks a resource (and its memory) failed and publishes
// resource.failed. Terminal — acks the message.
func (w *worker) failResource(id, rid, msg string) error {
	w.logger.Error("memory resource failed", "memory", id, "resource", rid, "msg", msg)
	now := time.Now().UTC()
	fail := &repository.Failure{Stage: "ingest", Code: "ingest_failed", Message: msg, At: now}
	_, _ = w.memStore.UpdateResource(id, rid, func(r *memory.Resource) error {
		r.Status = repository.StatusFailed
		r.Stage = "failed"
		r.Failure = fail
		started := now
		opName := "ingest"
		if r.LastOperation != nil {
			started = r.LastOperation.StartedAt
			opName = r.LastOperation.Name
		}
		r.LastOperation = &repository.Operation{
			Name:       opName,
			Status:     repository.OperationFailed,
			StartedAt:  started,
			FinishedAt: &now,
			Error:      fail,
		}
		return nil
	})
	_, _ = w.memStore.Update(id, func(md *memory.Memory) error {
		md.Status = memory.StatusFailed
		md.Stage = "ingest"
		md.Failure = &repository.Failure{Stage: "ingest", Code: "ingest_failed", Message: msg, At: now}
		return nil
	})
	w.commitMemory(id, "memory: resource "+rid+" failed")
	_ = w.bus.PublishMemory(events.SubjectMemoryResourceFailed, "memory-resource-failed:"+id+":"+rid,
		events.MemoryEventData{MemoryID: id, ResourceID: rid, Message: msg})
	return nil
}

// failMerge marks the memory failed and publishes merge.failed. Terminal — acks.
func (w *worker) failMerge(id, msg string) error {
	w.logger.Error("memory merge failed", "memory", id, "msg", msg)
	now := time.Now().UTC()
	fail := &repository.Failure{Stage: "merge", Code: "merge_failed", Message: msg, At: now}
	_, _ = w.memStore.Update(id, func(md *memory.Memory) error {
		md.Status = memory.StatusFailed
		md.Stage = "merge"
		md.Failure = fail
		started := now
		if md.LastOperation != nil {
			started = md.LastOperation.StartedAt
		}
		md.LastOperation = &repository.Operation{
			Name:       "merge",
			Status:     repository.OperationFailed,
			StartedAt:  started,
			FinishedAt: &now,
			Error:      fail,
		}
		return nil
	})
	w.commitMemory(id, "memory: merge failed")
	_ = w.bus.PublishMemory(events.SubjectMemoryMergeFailed, "memory-merge-failed:"+id,
		events.MemoryEventData{MemoryID: id, Message: msg})
	return nil
}

// setMemoryStatus best-effort advances the memory status (only when the
// transition is allowed), never fatal.
func (w *worker) setMemoryStatus(id string, status memory.Status, stage string) {
	_, err := w.memStore.Update(id, func(md *memory.Memory) error {
		if md.Status == status || memory.CanTransition(md.Status, status) {
			md.Status = status
			md.Stage = stage
		}
		return nil
	})
	if err != nil {
		w.logger.Error("memory: set status", "memory", id, "status", status, "error", err)
	}
}

// commitMemory refreshes the scaffold, commits the memory repo, and persists the
// new HEAD SHA as GraphRef. Returns the new ref (empty on failure — logged, not
// fatal, since the manifest is already durably stored). Mirrors the API helper.
func (w *worker) commitMemory(id, message string) string {
	ctx := context.Background()
	dir := w.memStore.Layout().MemoryDir(id)
	if m, err := w.memStore.Get(id); err == nil {
		_ = memory.WriteScaffold(w.memStore.Layout(), m)
	}
	if err := memory.InitRepo(ctx, dir, w.cfg.MemoryTimeout); err != nil {
		w.logger.Error("memory git init", "memory", id, "error", err)
	}
	ref, err := memory.Commit(ctx, dir, message, w.cfg.MemoryTimeout)
	if err != nil {
		w.logger.Error("commit memory", "memory", id, "error", err)
		return ""
	}
	if _, err := w.memStore.Update(id, func(m *memory.Memory) error {
		m.GraphRef = ref
		return nil
	}); err != nil {
		w.logger.Error("persist memory ref", "memory", id, "error", err)
	}
	return ref
}

// findResource returns a copy of the resource with id rid, and whether it exists.
func findResource(m memory.Memory, rid string) (memory.Resource, bool) {
	for i := range m.Resources {
		if m.Resources[i].ID == rid {
			return m.Resources[i], true
		}
	}
	return memory.Resource{}, false
}

// mergeFingerprint hashes the ready set (sorted resource id@resolvedSha pairs) so
// the merge request id is stable for an unchanged set and fresh after a change.
func mergeFingerprint(rs []memory.Resource) string {
	keys := make([]string, 0, len(rs))
	for _, r := range rs {
		keys = append(keys, r.ID+"@"+r.ResolvedSHA)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "|")))
	return hex.EncodeToString(sum[:])
}

func busReady(bus *events.Bus) error {
	if !bus.Connected() {
		return errors.New("nats not connected")
	}
	return nil
}
