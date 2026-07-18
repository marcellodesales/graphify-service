package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

func testServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 1 << 20
	}
	store, err := repository.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewServer(cfg, store, logger)
}

func doJSON(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSubmitReturnsDeterministicIDAndIsIdempotent(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()

	body := `{"githubRepoUrl":"https://github.com/marcellodesales/graphify-service"}`

	w1 := doJSON(t, h, "POST", "/api/v1/repositories", body, nil)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first submit status = %d, want 202; body=%s", w1.Code, w1.Body.String())
	}
	var r1 submitResponse
	mustJSON(t, w1.Body.Bytes(), &r1)
	if len(r1.ID) != 64 {
		t.Fatalf("id not sha256 hex: %q", r1.ID)
	}
	if r1.Status != "queued" {
		t.Fatalf("status = %q, want queued", r1.Status)
	}
	if r1.StatusURL != "/api/v1/repositories/"+r1.ID {
		t.Fatalf("statusUrl = %q", r1.StatusURL)
	}

	// Duplicate submit -> 200 with the same id.
	w2 := doJSON(t, h, "POST", "/api/v1/repositories", body, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("duplicate submit status = %d, want 200", w2.Code)
	}
	var r2 submitResponse
	mustJSON(t, w2.Body.Bytes(), &r2)
	if r2.ID != r1.ID {
		t.Fatalf("duplicate id mismatch: %q != %q", r2.ID, r1.ID)
	}
}

func TestSubmitValidation(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()

	cases := []struct {
		name string
		body string
	}{
		{"bad url", `{"githubRepoUrl":"file:///etc/passwd"}`},
		{"missing url", `{"githubRepoUrl":""}`},
		{"ref and sha", `{"githubRepoUrl":"https://github.com/o/r","githubRef":"main","githubSha":"51d5269"}`},
		{"bad sha", `{"githubRepoUrl":"https://github.com/o/r","githubSha":"zzzz"}`},
		{"bad ref", `{"githubRepoUrl":"https://github.com/o/r","githubRef":"a..b"}`},
		{"bad sshKeyRef", `{"githubRepoUrl":"https://github.com/o/r","sshKeyRef":"../escape"}`},
		{"unknown field", `{"githubRepoUrl":"https://github.com/o/r","nope":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doJSON(t, h, "POST", "/api/v1/repositories", c.body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			var e errorResponse
			mustJSON(t, w.Body.Bytes(), &e)
			if e.Error.Code == "" || e.Error.RequestID == "" {
				t.Fatalf("error envelope incomplete: %+v", e.Error)
			}
		})
	}
}

func TestGetAndNotFound(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()

	w := doJSON(t, h, "POST", "/api/v1/repositories", `{"githubRepoUrl":"https://github.com/o/r"}`, nil)
	var r submitResponse
	mustJSON(t, w.Body.Bytes(), &r)

	got := doJSON(t, h, "GET", "/api/v1/repositories/"+r.ID, "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", got.Code)
	}
	var view repositoryView
	mustJSON(t, got.Body.Bytes(), &view)
	if view.ID != r.ID || view.Links.Self != "/api/v1/repositories/"+r.ID {
		t.Fatalf("unexpected view: %+v", view)
	}

	// Unknown but well-formed id -> 404.
	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	nf := doJSON(t, h, "GET", "/api/v1/repositories/"+missing, "", nil)
	if nf.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", nf.Code)
	}

	// Malformed id -> 400.
	bad := doJSON(t, h, "GET", "/api/v1/repositories/not-a-sha", "", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad id status = %d, want 400", bad.Code)
	}
}

func TestAuthStatic(t *testing.T) {
	cfg := config.Config{AuthMode: config.AuthStatic, APIToken: "s3cr3t"}
	srv := testServer(t, cfg)
	h := srv.Handler()

	body := `{"githubRepoUrl":"https://github.com/o/r"}`

	// No token -> 401.
	if w := doJSON(t, h, "POST", "/api/v1/repositories", body, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", w.Code)
	}
	// Wrong token -> 401.
	if w := doJSON(t, h, "POST", "/api/v1/repositories", body, map[string]string{"Authorization": "Bearer nope"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token status = %d, want 401", w.Code)
	}
	// Correct bearer -> 202.
	if w := doJSON(t, h, "POST", "/api/v1/repositories", body, map[string]string{"Authorization": "Bearer s3cr3t"}); w.Code != http.StatusAccepted {
		t.Fatalf("bearer status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	// X-API-Key also accepted -> 200 (idempotent duplicate of the previous submit).
	if w := doJSON(t, h, "POST", "/api/v1/repositories", body, map[string]string{"X-API-Key": "s3cr3t"}); w.Code != http.StatusOK {
		t.Fatalf("x-api-key status = %d, want 200", w.Code)
	}
}

func TestHealthAndReadyPublic(t *testing.T) {
	// Even with auth on, health/readiness must be reachable without a token.
	srv := testServer(t, config.Config{AuthMode: config.AuthStatic, APIToken: "x"})
	h := srv.Handler()

	if w := doJSON(t, h, "GET", "/healthz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", w.Code)
	}
	if w := doJSON(t, h, "GET", "/readyz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", w.Code)
	}
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("json decode: %v (body=%s)", err, string(b))
	}
}
