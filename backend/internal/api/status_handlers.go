package api

import (
	"net/http"

	"github.com/marcellodesales/graphify-service/backend/internal/memory"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// resourceStatusView is a compact per-resource work-status summary: the current
// lifecycle snapshot plus the generalized last-operation record.
type resourceStatusView struct {
	ID            string                `json:"id"`
	Kind          string                `json:"kind"`
	Status        string                `json:"status"`
	Stage         string                `json:"stage,omitempty"`
	LastOperation *repository.Operation `json:"lastOperation,omitempty"`
	Failure       *repository.Failure   `json:"failure,omitempty"`
}

// memoryStatusResponse is the GET /api/v1/memories/{id}/status body: the memory's
// overall status plus its last memory-level operation (e.g. merge) and a compact
// per-resource summary, so a caller can poll "how is the work going" in one call.
type memoryStatusResponse struct {
	MemoryID      string                `json:"memoryId"`
	Status        string                `json:"status"`
	Stage         string                `json:"stage,omitempty"`
	LastOperation *repository.Operation `json:"lastOperation,omitempty"`
	Failure       *repository.Failure   `json:"failure,omitempty"`
	Resources     []resourceStatusView  `json:"resources"`
}

// handleMemoryStatus implements GET /api/v1/memories/{id}/status.
func (s *Server) handleMemoryStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	resources := make([]resourceStatusView, 0, len(m.Resources))
	for _, res := range m.Resources {
		resources = append(resources, resourceStatusViewFor(res))
	}
	writeJSON(w, r, http.StatusOK, memoryStatusResponse{
		MemoryID:      m.ID,
		Status:        string(m.Status),
		Stage:         m.Stage,
		LastOperation: m.LastOperation,
		Failure:       m.Failure,
		Resources:     resources,
	})
}

// handleResourceStatus implements GET
// /api/v1/memories/{id}/resources/{rid}/status.
func (s *Server) handleResourceStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	rid := r.PathValue("rid")
	if !memory.ValidResourceID(rid) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid resource id")
		return
	}
	for _, res := range m.Resources {
		if res.ID == rid {
			writeJSON(w, r, http.StatusOK, resourceStatusViewFor(res))
			return
		}
	}
	writeError(w, r, http.StatusNotFound, "not_found", "resource not found")
}

func resourceStatusViewFor(res memory.Resource) resourceStatusView {
	return resourceStatusView{
		ID:            res.ID,
		Kind:          string(res.Kind),
		Status:        string(res.Status),
		Stage:         res.Stage,
		LastOperation: res.LastOperation,
		Failure:       res.Failure,
	}
}
