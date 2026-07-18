package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marcellodesales/graphify-service/backend/internal/artifacts"
	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/events"
	"github.com/marcellodesales/graphify-service/backend/internal/giturl"
	"github.com/marcellodesales/graphify-service/backend/internal/mcpproxy"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
	"github.com/marcellodesales/graphify-service/backend/internal/statushttp"
)

// Publisher publishes pipeline events (satisfied by *events.Bus). Nil-safe: when
// nil the API still accepts submissions but does not drive the async pipeline.
type Publisher interface {
	Publish(subject, msgID string, data events.RepoEventData) error
	Connected() bool
}

// Server holds the HTTP handler dependencies.
type Server struct {
	cfg    config.Config
	store  *repository.Store
	logger *slog.Logger
	auth   authenticator
	bus    Publisher
}

// NewServer builds a Server. bus may be nil (pipeline disabled).
func NewServer(cfg config.Config, store *repository.Store, logger *slog.Logger, bus Publisher) *Server {
	return &Server{
		cfg:    cfg,
		store:  store,
		logger: logger,
		auth:   authenticator{mode: cfg.AuthMode, token: cfg.APIToken},
		bus:    bus,
	}
}

// handleSubmit implements POST /api/v1/repositories (spec §6.2).
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}

	repo, err := giturl.Parse(req.GithubRepoURL)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "githubRepoUrl: "+err.Error())
		return
	}

	if len(s.cfg.AllowedGitHosts) > 0 && !hostAllowed(repo.Host, s.cfg.AllowedGitHosts) {
		writeError(w, r, http.StatusForbidden, "host_not_allowed", "git host is not allowed: "+repo.Host)
		return
	}

	sel, err := buildSelector(req.GithubRef, req.GithubSha)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	sshKeyRef := strings.TrimSpace(req.SSHKeyRef)
	if sshKeyRef != "" && !validKeyRef(sshKeyRef) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "sshKeyRef must be a simple name (no path separators)")
		return
	}

	id := repository.ComputeID(repo.Canonical, sel)
	meta := repository.Metadata{
		ID:       id,
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

	saved, created, err := s.store.Create(meta)
	if err != nil {
		s.logger.Error("create repository", "request_id", RequestIDFrom(r.Context()), "id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to persist repository")
		return
	}

	// Kick off (or re-kick) the async pipeline. Idempotent via Nats-Msg-Id;
	// only publish while still queued so terminal/in-flight jobs aren't disturbed.
	if s.bus != nil && saved.Status == repository.StatusQueued {
		if err := s.bus.Publish(events.SubjectCloneRequested, "clone-request:"+saved.ID, events.RepoEventData{
			RepositoryID:  saved.ID,
			SelectorType:  string(saved.Selector.Type),
			SelectorValue: saved.Selector.Value,
		}); err != nil {
			// Metadata is durably 'queued'; a later resubmit re-publishes.
			s.logger.Error("publish clone.requested", "request_id", RequestIDFrom(r.Context()), "id", saved.ID, "error", err)
		}
	}

	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, r, status, submitResponse{
		ID:           saved.ID,
		Status:       string(saved.Status),
		StatusURL:    repoPath(saved.ID),
		ArtifactsURL: repoPath(saved.ID) + "/artifacts",
	})
}

// handleArtifacts implements GET /api/v1/repositories/{id}/artifacts (spec §6.5).
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !repository.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid repository id")
		return
	}
	m, err := s.store.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "repository not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read repository")
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

// handleDownload implements GET /api/v1/repositories/{id}/download?format=zip
// with optional include/exclude filtering (spec §6.6, PRD-003).
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !repository.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid repository id")
		return
	}
	if f := strings.TrimSpace(r.URL.Query().Get("format")); f != "" && f != "zip" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "only format=zip is supported")
		return
	}
	m, err := s.store.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "repository not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read repository")
		return
	}
	if m.Status != repository.StatusReady {
		writeError(w, r, http.StatusConflict, "not_ready", "repository is not ready (status: "+string(m.Status)+")")
		return
	}
	items := artifacts.Select(m.Artifacts, csv(r.URL.Query().Get("include")), csv(r.URL.Query().Get("exclude")))
	if len(items) == 0 {
		writeError(w, r, http.StatusNotFound, "no_artifacts", "no matching artifacts to download")
		return
	}
	repoDir := s.store.Layout().RepositoryDir(id)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"graphify-"+id[:12]+".zip\"")
	if err := artifacts.Zip(w, repoDir, items); err != nil {
		s.logger.Error("zip download", "request_id", RequestIDFrom(r.Context()), "id", id, "error", err)
		// Headers/body may be partially written; nothing safe to add.
	}
}

// handleServiceStatus implements GET /status/{id} — the uniform status protocol.
func (s *Server) handleServiceStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, statushttp.EnvelopeFor("api", s.store, r.PathValue("id")))
}

// handleQuery implements POST /api/v1/repositories/{id}/query (PRD-004): it
// composes with the graphify-mcp server, injecting project_path for this repo,
// so the caller can ask questions about a ready repo's graph.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !repository.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid repository id")
		return
	}
	m, err := s.store.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "repository not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read repository")
		return
	}
	if m.Status != repository.StatusReady {
		writeError(w, r, http.StatusConflict, "not_ready", "repository is not ready (status: "+string(m.Status)+")")
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

	// Inject project_path so the shared graphify-mcp answers for THIS repo.
	args := map[string]any{"project_path": s.store.Layout().RepositoryDir(id)}
	if question != "" {
		args["question"] = question
	}

	client := mcpproxy.New(s.cfg.MCPURL)
	answer, isErr, err := client.CallTool(r.Context(), tool, args)
	if err != nil {
		s.logger.Error("query", "request_id", RequestIDFrom(r.Context()), "id", id, "tool", tool, "error", err)
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

// handleList implements GET /api/v1/repositories (spec §6.3).
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repository.ListFilter{
		Status: repository.Status(strings.TrimSpace(q.Get("status"))),
		Host:   strings.ToLower(strings.TrimSpace(q.Get("host"))),
		Owner:  strings.TrimSpace(q.Get("owner")),
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

	res, err := s.store.List(f)
	if err != nil {
		s.logger.Error("list repositories", "request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to list repositories")
		return
	}

	views := make([]repositoryView, 0, len(res.Repositories))
	for _, m := range res.Repositories {
		views = append(views, viewFor(m))
	}
	resp := listResponse{Repositories: views}
	if res.NextCursor != "" {
		resp.NextCursor = &res.NextCursor
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// handleGet implements GET /api/v1/repositories/{id} (spec §6.4).
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !repository.ValidID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid repository id")
		return
	}
	meta, err := s.store.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "repository not found")
			return
		}
		s.logger.Error("get repository", "request_id", RequestIDFrom(r.Context()), "id", id, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to read repository")
		return
	}
	writeJSON(w, r, http.StatusOK, viewFor(meta))
}

// handleHealthz reports process liveness (public).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports readiness: repos root writable + NATS connected (public).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := checkWritable(s.store.Layout().Root()); err != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "repos root not writable",
		})
		return
	}
	if s.bus != nil && !s.bus.Connected() {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "nats not connected",
		})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
}

// --- helpers ---

// csv splits a comma-separated query value into trimmed, non-empty names.
func csv(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func buildSelector(ref, sha string) (repository.Selector, error) {
	ref = strings.TrimSpace(ref)
	sha = strings.TrimSpace(sha)
	if ref != "" && sha != "" {
		return repository.Selector{}, errors.New("githubRef and githubSha are mutually exclusive")
	}
	switch {
	case sha != "":
		sha = strings.ToLower(sha)
		if !isHexSHA(sha) {
			return repository.Selector{}, errors.New("githubSha must be a hexadecimal commit identifier")
		}
		return repository.Selector{Type: repository.SelectorSHA, Value: sha}, nil
	case ref != "":
		if err := giturl.ValidateRef(ref); err != nil {
			return repository.Selector{}, err
		}
		return repository.Selector{Type: repository.SelectorRef, Value: ref}, nil
	default:
		return repository.Selector{Type: repository.SelectorDefault, Value: ""}, nil
	}
}

func isHexSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func validKeyRef(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func hostAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		if host == a {
			return true
		}
	}
	return false
}

func repoPath(id string) string { return "/api/v1/repositories/" + id }

func viewFor(m repository.Metadata) repositoryView {
	return repositoryView{
		Metadata: m,
		Links: links{
			Self:        repoPath(m.ID),
			Artifacts:   repoPath(m.ID) + "/artifacts",
			DownloadZip: repoPath(m.ID) + "/download?format=zip",
		},
	}
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".readyz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(filepath.Clean(name))
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
	_ = r // request retained for symmetry / future use
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, r, status, errorResponse{Error: errorBody{
		Code:      code,
		Message:   message,
		RequestID: RequestIDFrom(r.Context()),
	}})
}
