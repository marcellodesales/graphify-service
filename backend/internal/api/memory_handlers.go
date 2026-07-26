package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marcellodesales/graphify-service/backend/internal/artifacts"
	"github.com/marcellodesales/graphify-service/backend/internal/events"
	"github.com/marcellodesales/graphify-service/backend/internal/giturl"
	"github.com/marcellodesales/graphify-service/backend/internal/mcpproxy"
	"github.com/marcellodesales/graphify-service/backend/internal/memory"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// handleCreateMemory implements POST /api/v1/memories: it allocates an ID,
// persists an empty memory, git-initializes its directory, and commits the
// scaffold so the memory has a stable GraphRef from the very first response.
func (s *Server) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	var req createMemoryRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}

	id, err := memory.NewID()
	if err != nil {
		s.logger.Error("memory id", "request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to allocate id")
		return
	}

	saved, err := s.memStore.Create(memory.Memory{
		ID:          id,
		Name:        strings.TrimSpace(req.Name),
		Description: strings.TrimSpace(req.Description),
		Status:      memory.StatusCreated,
	})
	if err != nil {
		s.logger.Error("create memory", "request_id", RequestIDFrom(r.Context()), "id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to persist memory")
		return
	}

	// Scaffold + git init + first commit. Detached from the request context so a
	// client disconnect can't leave a half-initialized repo.
	ctx := context.Background()
	dir := s.memStore.Layout().MemoryDir(id)
	if err := memory.WriteScaffold(s.memStore.Layout(), saved); err != nil {
		s.logger.Error("memory scaffold", "id", id, "error", err)
	}
	if err := memory.InitRepo(ctx, dir, s.cfg.MemoryTimeout); err != nil {
		s.logger.Error("memory git init", "id", id, "error", err)
	}
	if ref, err := memory.Commit(ctx, dir, "memory: create "+id, s.cfg.MemoryTimeout); err != nil {
		s.logger.Error("memory initial commit", "id", id, "error", err)
	} else {
		if updated, uerr := s.memStore.Update(id, func(m *memory.Memory) error {
			m.GraphRef = ref
			return nil
		}); uerr == nil {
			saved = updated
		}
	}

	writeJSON(w, r, http.StatusCreated, memViewFor(saved))
}

// handleListMemories implements GET /api/v1/memories.
func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := memory.ListFilter{
		Status: memory.Status(strings.TrimSpace(q.Get("status"))),
		Cursor: strings.TrimSpace(q.Get("cursor")),
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "limit must be a non-negative integer")
			return
		}
		f.Limit = n
	}
	res, err := s.memStore.List(f)
	if err != nil {
		s.logger.Error("list memories", "request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to list memories")
		return
	}
	views := make([]memoryView, 0, len(res.Memories))
	for _, m := range res.Memories {
		views = append(views, memViewFor(m))
	}
	resp := memoryListResponse{Memories: views}
	if res.NextCursor != "" {
		resp.NextCursor = &res.NextCursor
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// handleGetMemory implements GET /api/v1/memories/{id}.
func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	writeJSON(w, r, http.StatusOK, memViewFor(m))
}

// handleAddResource implements POST /api/v1/memories/{id}/resources. It accepts
// either a JSON git-source body or a multipart file upload, appends the source,
// commits the manifest, and publishes a resource.requested event for the worker.
func (s *Server) handleAddResource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !memory.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid memory id")
		return
	}
	if _, err := s.memStore.Get(id); err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "memory not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read memory")
		return
	}

	// This route is exempt from the global body limit (see withBodyLimit) so it
	// can accept large uploads; enforce the appropriate cap here instead.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
		s.addFileResource(w, r, id)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	s.addGitResource(w, r, id)
}

// addGitResource appends a git-repository source (JSON body).
func (s *Server) addGitResource(w http.ResponseWriter, r *http.Request, id string) {
	var req addGitResourceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}

	repo, err := giturl.Parse(req.GitRepoURL)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "gitRepoUrl: "+err.Error())
		return
	}
	if len(s.cfg.AllowedGitHosts) > 0 && !hostAllowed(repo.Host, s.cfg.AllowedGitHosts) {
		writeError(w, r, http.StatusForbidden, "host_not_allowed", "git host is not allowed: "+repo.Host)
		return
	}
	sel, err := buildSelector(req.Ref, req.Sha)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	sshKeyRef := strings.TrimSpace(req.SSHKeyRef)
	sshKey := strings.TrimSpace(req.SSHKey)
	if sshKeyRef != "" && sshKey != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "provide either sshKeyRef (a provisioned key name) or sshKey (the key material), not both")
		return
	}
	if sshKeyRef != "" && !validKeyRef(sshKeyRef) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "sshKeyRef must be a simple name (no path separators)")
		return
	}
	if sshKey != "" && !memory.LooksLikePrivateKey([]byte(sshKey)) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "sshKey must be a PEM private key (not a public key)")
		return
	}

	rid, err := memory.NewResourceID()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to allocate resource id")
		return
	}

	// Store caller-supplied key material on our volume BEFORE the resource is
	// published for ingestion, so the worker finds it. The key is written under
	// the memory's gitignored .ssh/ dir (mode 0600) and never persisted in the
	// manifest — only the non-secret SSHKeyStored flag records its presence.
	sshKeyStored := false
	if sshKey != "" {
		if err := memory.WriteResourceSSHKey(s.memStore.Layout(), id, rid, []byte(sshKey), []byte(req.KnownHosts)); err != nil {
			s.logger.Error("store resource ssh key", "request_id", RequestIDFrom(r.Context()), "id", id, "resource", rid, "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to store ssh key")
			return
		}
		sshKeyStored = true
	}

	res := memory.Resource{
		ID:       rid,
		Kind:     memory.KindGit,
		Status:   repository.StatusQueued,
		Selector: sel,
		Source: repository.Source{
			NormalizedURL: repo.Canonical,
			Host:          repo.Host,
			OwnerPath:     repo.Owner,
			Repository:    repo.Name,
			Transport:     string(repo.Transport),
			Private:       sshKeyRef != "" || sshKeyStored,
			SSHKeyRef:     sshKeyRef,
			SSHKeyStored:  sshKeyStored,
		},
	}
	s.appendResource(w, r, id, res)
}

// addFileResource appends an uploaded-file source (multipart body). The bytes are
// written synchronously under files/<resourceId>/ so the worker only extracts.
func (s *Server) addFileResource(w http.ResponseWriter, r *http.Request, id string) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "upload_too_large", "upload exceeds the maximum allowed size or is malformed")
		return
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "missing form file field \"file\"")
		return
	}
	defer file.Close()

	rid, err := memory.NewResourceID()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to allocate resource id")
		return
	}

	// filepath.Base strips any directory components; guard the dot names. The
	// original extension is preserved so graphify's detect.py can classify it.
	fname := filepath.Base(fh.Filename)
	if fname == "." || fname == ".." || fname == "" {
		fname = "upload"
	}

	dir := s.memStore.Layout().FileResourceDir(id, rid)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to stage upload")
		return
	}
	dst := filepath.Join(dir, fname)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to write upload")
		return
	}
	size, cerr := io.Copy(out, file)
	if closeErr := out.Close(); closeErr != nil && cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		_ = os.RemoveAll(dir)
		writeError(w, r, http.StatusRequestEntityTooLarge, "upload_too_large", "upload exceeds the maximum allowed size")
		return
	}

	res := memory.Resource{
		ID:       rid,
		Kind:     memory.KindFile,
		Status:   repository.StatusQueued,
		FileName: fname,
		Size:     size,
	}
	s.appendResource(w, r, id, res)
}

// appendResource stores the resource, commits the manifest, publishes the
// resource.requested event, and writes the 202 response.
func (s *Server) appendResource(w http.ResponseWriter, r *http.Request, id string, res memory.Resource) {
	_, stored, err := s.memStore.AddResource(id, res)
	if err != nil {
		s.logger.Error("add resource", "request_id", RequestIDFrom(r.Context()), "id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to persist resource")
		return
	}

	ref := s.commitMemory(id, "memory: add "+string(stored.Kind)+" resource "+stored.ID)

	if s.bus != nil {
		if err := s.bus.PublishMemory(events.SubjectMemoryResourceRequested,
			"memory-resource-request:"+id+":"+stored.ID,
			events.MemoryEventData{MemoryID: id, ResourceID: stored.ID},
		); err != nil {
			s.logger.Error("publish memory.resource.requested", "id", id, "resource", stored.ID, "error", err)
		}
	}

	writeJSON(w, r, http.StatusAccepted, addResourceResponse{
		MemoryID:   id,
		ResourceID: stored.ID,
		Kind:       string(stored.Kind),
		Status:     string(stored.Status),
		Ref:        ref,
	})
}

// handleListResources implements GET /api/v1/memories/{id}/resources.
func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	res := m.Resources
	if res == nil {
		res = []memory.Resource{}
	}
	writeJSON(w, r, http.StatusOK, resourceListResponse{MemoryID: m.ID, Resources: res})
}

// handleGetResource implements GET /api/v1/memories/{id}/resources/{rid}.
func (s *Server) handleGetResource(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	rid := r.PathValue("rid")
	for i := range m.Resources {
		if m.Resources[i].ID == rid {
			writeJSON(w, r, http.StatusOK, m.Resources[i])
			return
		}
	}
	writeError(w, r, http.StatusNotFound, "not_found", "resource not found")
}

// handleSetResourceSSHKey implements PUT
// /api/v1/memories/{id}/resources/{rid}/ssh-key. It stores (or replaces) the
// caller-supplied deploy key for an existing git resource on our volume and
// re-queues that resource so the worker retries the clone with the key. The key
// bytes are written under the memory's gitignored .ssh/ dir (mode 0600) and are
// never persisted in the manifest — only the non-secret SSHKeyStored flag is.
func (s *Server) handleSetResourceSSHKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rid := r.PathValue("rid")
	if !memory.ValidID(id) || !memory.ValidResourceID(rid) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid memory or resource id")
		return
	}
	m, err := s.memStore.Get(id)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "memory not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read memory")
		return
	}
	var found *memory.Resource
	for i := range m.Resources {
		if m.Resources[i].ID == rid {
			found = &m.Resources[i]
			break
		}
	}
	if found == nil {
		writeError(w, r, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if found.Kind != memory.KindGit {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "ssh keys apply only to git resources")
		return
	}

	var req setResourceSSHKeyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	sshKey := strings.TrimSpace(req.SSHKey)
	if !memory.LooksLikePrivateKey([]byte(sshKey)) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "sshKey must be a PEM private key (not a public key)")
		return
	}
	if err := memory.WriteResourceSSHKey(s.memStore.Layout(), id, rid, []byte(sshKey), []byte(req.KnownHosts)); err != nil {
		s.logger.Error("store resource ssh key", "request_id", RequestIDFrom(r.Context()), "id", id, "resource", rid, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to store ssh key")
		return
	}

	// Re-queue the resource so the worker re-attempts the clone with the key.
	// Setting status away from ready is what makes handleResource re-extract
	// (rather than short-circuit to merge) on the re-published request.
	if _, err := s.memStore.UpdateResource(id, rid, func(res *memory.Resource) error {
		res.Source.Private = true
		res.Source.SSHKeyStored = true
		res.Status = repository.StatusQueued
		res.Stage = "queued"
		res.Failure = nil
		return nil
	}); err != nil {
		s.logger.Error("requeue resource", "request_id", RequestIDFrom(r.Context()), "id", id, "resource", rid, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to update resource")
		return
	}

	ref := s.commitMemory(id, "memory: set ssh key for resource "+rid)

	if s.bus != nil {
		// The fresh commit ref makes the message id unique per re-key so
		// JetStream delivers it instead of deduplicating against the original.
		msgID := "memory-resource-request:" + id + ":" + rid
		if ref != "" {
			msgID += ":" + ref
		}
		if err := s.bus.PublishMemory(events.SubjectMemoryResourceRequested, msgID,
			events.MemoryEventData{MemoryID: id, ResourceID: rid}); err != nil {
			s.logger.Error("publish memory.resource.requested (rekey)", "id", id, "resource", rid, "error", err)
		}
	}

	writeJSON(w, r, http.StatusAccepted, setResourceSSHKeyResponse{
		MemoryID:   id,
		ResourceID: rid,
		Status:     string(repository.StatusQueued),
		Ref:        ref,
	})
}

// handleMemoryGraph serves the merged unified graph (graphify-out/graph.json).
func (s *Server) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	full := filepath.Join(s.memStore.Layout().GraphOutDir(m.ID), "graph.json")
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, r, http.StatusConflict, "not_ready", "merged graph is not available yet (status: "+string(m.Status)+")")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to open graph")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	_, _ = io.Copy(w, f)
}

// handleMemoryQuery implements POST /api/v1/memories/{id}/query. It composes
// with the shared graphify-mcp server exactly like the repository /query
// endpoint, but injects project_path = the memory directory so the MCP resolves
// <memory>/graphify-out/graph.json — the merged, cross-source unified graph.
// This is the "memory-aware MCP" integration: no new MCP service, the existing
// stateless server answers Q&A against any memory on the shared repos volume.
func (s *Server) handleMemoryQuery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !memory.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid memory id")
		return
	}
	m, err := s.memStore.Get(id)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "memory not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read memory")
		return
	}
	if m.Status != memory.StatusReady {
		writeError(w, r, http.StatusConflict, "not_ready", "memory is not ready (status: "+string(m.Status)+")")
		return
	}

	var req queryRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		tool = "query_graph"
	}
	question := strings.TrimSpace(req.Question)
	if tool == "query_graph" && question == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "question is required for query_graph")
		return
	}

	// Inject project_path so the shared graphify-mcp resolves this memory's
	// unified graph (<memory>/graphify-out/graph.json) for the call.
	args := map[string]any{"project_path": s.memStore.Layout().MemoryDir(id)}
	if question != "" {
		args["question"] = question
	}

	client := mcpproxy.New(s.cfg.MCPURL)
	answer, isErr, err := client.CallTool(r.Context(), tool, args)
	if err != nil {
		s.logger.Error("memory query", "request_id", RequestIDFrom(r.Context()), "id", id, "tool", tool, "error", err)
		writeError(w, r, http.StatusBadGateway, "query_backend_error", "graph query backend error")
		return
	}
	writeJSON(w, r, http.StatusOK, queryResponse{
		ID:       id,
		Tool:     tool,
		Question: question,
		Answer:   answer,
		IsError:  isErr,
	})
}

// handleMemoryArtifacts implements GET /api/v1/memories/{id}/artifacts.
func (s *Server) handleMemoryArtifacts(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	arts := m.Artifacts
	if arts == nil {
		arts = []repository.Artifact{}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"id":        m.ID,
		"status":    string(m.Status),
		"artifacts": arts,
	})
}

// handleMemoryArtifactFile serves one allowlisted merged artifact.
func (s *Server) handleMemoryArtifactFile(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var art *repository.Artifact
	for i := range m.Artifacts {
		if m.Artifacts[i].Name == name {
			art = &m.Artifacts[i]
			break
		}
	}
	if art == nil {
		writeError(w, r, http.StatusNotFound, "not_found", "artifact not found")
		return
	}
	full := filepath.Join(s.memStore.Layout().MemoryDir(m.ID), art.Path)
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, r, http.StatusNotFound, "not_found", "artifact unavailable")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to open artifact")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", art.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(art.Size, 10))
	_, _ = io.Copy(w, f)
}

// handleMemoryDownload implements GET /api/v1/memories/{id}/download?format=zip.
func (s *Server) handleMemoryDownload(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	if f := strings.TrimSpace(r.URL.Query().Get("format")); f != "" && f != "zip" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "only format=zip is supported")
		return
	}
	if m.Status != memory.StatusReady {
		writeError(w, r, http.StatusConflict, "not_ready", "memory is not ready (status: "+string(m.Status)+")")
		return
	}
	items := artifacts.Select(m.Artifacts, csv(r.URL.Query().Get("include")), csv(r.URL.Query().Get("exclude")))
	if len(items) == 0 {
		writeError(w, r, http.StatusNotFound, "no_artifacts", "no matching artifacts to download")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"memory-"+m.ID[:12]+".zip\"")
	if err := artifacts.Zip(w, s.memStore.Layout().MemoryDir(m.ID), items); err != nil {
		s.logger.Error("memory zip", "request_id", RequestIDFrom(r.Context()), "id", m.ID, "error", err)
	}
}

// --- helpers ---

// loadMemory validates the {id} path value and loads the memory, writing the
// appropriate error response and returning ok=false on any problem.
func (s *Server) loadMemory(w http.ResponseWriter, r *http.Request) (memory.Memory, bool) {
	id := r.PathValue("id")
	if !memory.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid memory id")
		return memory.Memory{}, false
	}
	m, err := s.memStore.Get(id)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "memory not found")
			return memory.Memory{}, false
		}
		s.logger.Error("get memory", "request_id", RequestIDFrom(r.Context()), "id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read memory")
		return memory.Memory{}, false
	}
	return m, true
}

// commitMemory refreshes the scaffold, commits the memory repo, and persists the
// new HEAD SHA as GraphRef. Returns the new ref (empty on failure — logged, not
// fatal, since the manifest is already durably stored).
func (s *Server) commitMemory(id, message string) string {
	ctx := context.Background()
	if m, err := s.memStore.Get(id); err == nil {
		_ = memory.WriteScaffold(s.memStore.Layout(), m)
	}
	ref, err := memory.Commit(ctx, s.memStore.Layout().MemoryDir(id), message, s.cfg.MemoryTimeout)
	if err != nil {
		s.logger.Error("commit memory", "id", id, "error", err)
		return ""
	}
	if _, err := s.memStore.Update(id, func(m *memory.Memory) error {
		m.GraphRef = ref
		return nil
	}); err != nil {
		s.logger.Error("persist memory ref", "id", id, "error", err)
	}
	return ref
}

func memPath(id string) string { return "/api/v1/memories/" + id }

func memViewFor(m memory.Memory) memoryView {
	return memoryView{
		Memory: m,
		Links: memoryLinks{
			Self:        memPath(m.ID),
			Resources:   memPath(m.ID) + "/resources",
			Graph:       memPath(m.ID) + "/graph",
			Query:       memPath(m.ID) + "/query",
			Artifacts:   memPath(m.ID) + "/artifacts",
			DownloadZip: memPath(m.ID) + "/download?format=zip",
		},
	}
}
