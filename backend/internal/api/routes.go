package api

import "net/http"

// Handler builds the fully-wrapped HTTP handler for the API/MCP process.
//
// Global middleware (recover, request-id, logging, body limit) wraps every
// route. Repository routes are additionally gated by bearer auth; health and
// readiness stay public (spec §13.4).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Protected repository API.
	mux.HandleFunc("POST /api/v1/repositories", s.protect(s.handleSubmit))
	mux.HandleFunc("GET /api/v1/repositories", s.protect(s.handleList))
	mux.HandleFunc("GET /api/v1/repositories/{id}", s.protect(s.handleGet))

	var h http.Handler = mux
	h = withBodyLimit(s.cfg.MaxRequestBytes, h)
	h = withLogging(s.logger, h)
	h = withRequestID(h)
	h = withRecover(s.logger, h)
	return h
}
