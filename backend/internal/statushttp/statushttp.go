// Package statushttp provides the shared status/verification protocol (PRD-001
// R5): a consistent status envelope any microservice can return for a reference
// ID, plus public /healthz and /readyz endpoints.
package statushttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// Envelope is the uniform status shape every service returns for an id.
type Envelope struct {
	ID          string `json:"id"`
	Service     string `json:"service"`
	Phase       string `json:"phase"`
	KnownAt     string `json:"knownAt"`
	Detail      string `json:"detail,omitempty"`
	ResolvedSHA string `json:"resolvedSha,omitempty"`
}

// EnvelopeFor builds a status envelope for id from the shared metadata store.
// Services that have never seen the id report phase "unknown".
func EnvelopeFor(service string, store *repository.Store, id string) Envelope {
	env := Envelope{
		ID:      id,
		Service: service,
		Phase:   "unknown",
		KnownAt: time.Now().UTC().Format(time.RFC3339),
	}
	if !repository.ValidID(id) {
		return env
	}
	m, err := store.Get(id)
	if err != nil {
		return env
	}
	env.Phase = string(m.Status)
	env.ResolvedSHA = m.ResolvedSHA
	if m.Failure != nil {
		env.Detail = m.Failure.Stage + ": " + m.Failure.Message
	}
	return env
}

// Mux returns an http.Handler exposing /healthz, /readyz, and /status/{id} for a
// worker service. ready reports readiness (nil = ready).
func Mux(service string, store *repository.Store, ready func() error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": service})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "unavailable", "service": service, "reason": err.Error(),
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "service": service})
	})
	mux.HandleFunc("GET /status/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, EnvelopeFor(service, store, r.PathValue("id")))
	})
	return mux
}

// Ready is a helper that combines readiness checks; returns the first error.
func Ready(checks ...func() error) func() error {
	return func() error {
		for _, c := range checks {
			if c == nil {
				continue
			}
			if err := c(); err != nil {
				return err
			}
		}
		return nil
	}
}

// ErrNotReady is a convenience sentinel.
var ErrNotReady = errors.New("not ready")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
