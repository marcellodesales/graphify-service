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
	if sshKeyRef != "" && !validKeyRef(sshKeyRef) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "sshKeyRef must be a simple name (no path separators)")
		return
	}

	rid, err := memory.NewResourceID()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to allocate resource id")
		return
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
			Private:       sshKeyRef != "",
			SSHKeyRef:     sshKeyRef,
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
			Artifacts:   memPath(m.ID) + "/artifacts",
			DownloadZip: memPath(m.ID) + "/download?format=zip",
		},
	}
}
