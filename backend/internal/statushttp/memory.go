package statushttp

import (
	"net/http"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/memory"
)

// MemoryEnvelopeFor builds a status envelope for a memory id from the memory
// store. Services that have never seen the id report phase "unknown". The memory
// repo's git HEAD SHA (GraphRef) is surfaced in ResolvedSHA so a single Envelope
// shape serves both the repository and memory pipelines.
func MemoryEnvelopeFor(service string, store *memory.Store, id string) Envelope {
	env := Envelope{
		ID:      id,
		Service: service,
		Phase:   "unknown",
		KnownAt: time.Now().UTC().Format(time.RFC3339),
	}
	if !memory.ValidID(id) {
		return env
	}
	m, err := store.Get(id)
	if err != nil {
		return env
	}
	env.Phase = string(m.Status)
	env.ResolvedSHA = m.GraphRef
	if m.Failure != nil {
		env.Detail = m.Failure.Stage + ": " + m.Failure.Message
	}
	return env
}

// MemoryMux returns an http.Handler exposing /healthz, /readyz, and /status/{id}
// for a memory worker service. ready reports readiness (nil = ready). It mirrors
// Mux but resolves the id against the memory store.
func MemoryMux(service string, store *memory.Store, ready func() error) http.Handler {
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
		writeJSON(w, http.StatusOK, MemoryEnvelopeFor(service, store, r.PathValue("id")))
	})
	return mux
}
