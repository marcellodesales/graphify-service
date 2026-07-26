package api

import (
	"net/http"
	"testing"

	"github.com/marcellodesales/graphify-service/backend/internal/config"
)

// TestMemoryQueryValidation covers the request-validation paths of the memory
// /query endpoint that do NOT need a live graphify-mcp backend: a malformed id
// (400), a well-formed but unknown id (404), and a freshly-created memory that
// is not yet ready (409). The happy path is exercised by the compose-driven
// integration test (test/integration/memory_mcp_test.sh).
func TestMemoryQueryValidation(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()

	// Malformed id -> 400.
	if w := doJSON(t, h, "POST", "/api/v1/memories/not-a-hex/query",
		`{"question":"hi"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id status = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	// Well-formed but unknown id -> 404.
	missing := "00000000000000000000000000000000"
	if w := doJSON(t, h, "POST", "/api/v1/memories/"+missing+"/query",
		`{"question":"hi"}`, nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	// Create a memory (status: created) then query it -> 409 not_ready. The
	// merged graph does not exist until the worker finishes, so we must refuse
	// to call the MCP backend.
	cw := doJSON(t, h, "POST", "/api/v1/memories", `{"name":"q-test"}`, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create memory status = %d, want 201; body=%s", cw.Code, cw.Body.String())
	}
	var mv memoryView
	mustJSON(t, cw.Body.Bytes(), &mv)
	if mv.Links.Query != "/api/v1/memories/"+mv.ID+"/query" {
		t.Fatalf("query link = %q, want .../query", mv.Links.Query)
	}
	if w := doJSON(t, h, "POST", "/api/v1/memories/"+mv.ID+"/query",
		`{"question":"hi"}`, nil); w.Code != http.StatusConflict {
		t.Fatalf("not-ready status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}
