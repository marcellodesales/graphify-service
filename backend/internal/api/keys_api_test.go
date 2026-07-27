package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/marcellodesales/graphify-service/backend/internal/config"
)

const apiTestPrivKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDtESTKEY0000000000000000000000000000000000000000
-----END OPENSSH PRIVATE KEY-----`

// createMemoryForTest creates a memory via the API and returns its id.
func createMemoryForTest(t *testing.T, h http.Handler) string {
	t.Helper()
	cw := doJSON(t, h, "POST", "/api/v1/memories", `{"name":"keytest"}`, nil)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create memory = %d; body=%s", cw.Code, cw.Body.String())
	}
	var mv memoryView
	mustJSON(t, cw.Body.Bytes(), &mv)
	return mv.ID
}

func TestKeyLifecycle(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()
	id := createMemoryForTest(t, h)

	// Add a key.
	body := `{"name":"deploy","sshKey":` + jsonString(apiTestPrivKey) + `}`
	aw := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/keys", body, nil)
	if aw.Code != http.StatusCreated {
		t.Fatalf("add key = %d; body=%s", aw.Code, aw.Body.String())
	}
	if strings.Contains(aw.Body.String(), "PRIVATE KEY") {
		t.Fatal("add-key response leaked key material")
	}
	var kv keyView
	mustJSON(t, aw.Body.Bytes(), &kv)
	if kv.ID == "" || kv.Fingerprint == "" || kv.Type != "ssh" {
		t.Fatalf("unexpected key view: %+v", kv)
	}
	if kv.Links.Self != "/api/v1/memories/"+id+"/keys/"+kv.ID {
		t.Fatalf("self link = %q", kv.Links.Self)
	}

	// A public key is rejected.
	if w := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/keys",
		`{"sshKey":"ssh-ed25519 AAAA u@h"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("public key add = %d, want 400", w.Code)
	}

	// List.
	lw := doJSON(t, h, "GET", "/api/v1/memories/"+id+"/keys", "", nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("list keys = %d", lw.Code)
	}
	var list keyListResponse
	mustJSON(t, lw.Body.Bytes(), &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != kv.ID {
		t.Fatalf("list = %+v", list)
	}

	// Get.
	gw := doJSON(t, h, "GET", "/api/v1/memories/"+id+"/keys/"+kv.ID, "", nil)
	if gw.Code != http.StatusOK {
		t.Fatalf("get key = %d", gw.Code)
	}

	// Rotate changes the fingerprint and sets rotatedAt.
	rotKey := "-----BEGIN RSA PRIVATE KEY-----\nrotated-material-here\n-----END RSA PRIVATE KEY-----"
	rw := doJSON(t, h, "PUT", "/api/v1/memories/"+id+"/keys/"+kv.ID,
		`{"sshKey":`+jsonString(rotKey)+`}`, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("rotate = %d; body=%s", rw.Code, rw.Body.String())
	}
	var rotated keyView
	mustJSON(t, rw.Body.Bytes(), &rotated)
	if rotated.Fingerprint == kv.Fingerprint {
		t.Error("rotate did not change fingerprint")
	}
	if rotated.RotatedAt == nil {
		t.Error("rotate did not set rotatedAt")
	}

	// Delete.
	dw := doJSON(t, h, "DELETE", "/api/v1/memories/"+id+"/keys/"+kv.ID, "", nil)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", dw.Code)
	}
	if w := doJSON(t, h, "GET", "/api/v1/memories/"+id+"/keys/"+kv.ID, "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", w.Code)
	}
}

func TestGitAddWithKeyID(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()
	id := createMemoryForTest(t, h)

	// Referencing an unknown key id -> 400.
	badKey := "00000000000000000000000000000000"
	if w := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/resources",
		`{"gitRepoUrl":"git@github.com:o/r.git","keyId":"`+badKey+`"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown keyId add = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	// Provision a key, then reference it.
	aw := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/keys",
		`{"sshKey":`+jsonString(apiTestPrivKey)+`}`, nil)
	var kv keyView
	mustJSON(t, aw.Body.Bytes(), &kv)

	rw := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/resources",
		`{"gitRepoUrl":"git@github.com:o/r.git","keyId":"`+kv.ID+`"}`, nil)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("git add with keyId = %d, want 202; body=%s", rw.Code, rw.Body.String())
	}

	// Deleting a referenced key -> 409.
	if w := doJSON(t, h, "DELETE", "/api/v1/memories/"+id+"/keys/"+kv.ID, "", nil); w.Code != http.StatusConflict {
		t.Fatalf("delete referenced key = %d, want 409; body=%s", w.Code, w.Body.String())
	}

	// Mutually exclusive with sshKey.
	if w := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/resources",
		`{"gitRepoUrl":"git@github.com:o/r.git","keyId":"`+kv.ID+`","sshKey":`+jsonString(apiTestPrivKey)+`}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("keyId+sshKey add = %d, want 400", w.Code)
	}
}

func TestMemoryStatusEndpoints(t *testing.T) {
	srv := testServer(t, config.Config{AuthMode: config.AuthNone})
	h := srv.Handler()
	id := createMemoryForTest(t, h)

	// Add a git resource so the memory has one.
	rw := doJSON(t, h, "POST", "/api/v1/memories/"+id+"/resources",
		`{"gitRepoUrl":"https://github.com/o/r"}`, nil)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("add resource = %d; body=%s", rw.Code, rw.Body.String())
	}
	var ar addResourceResponse
	mustJSON(t, rw.Body.Bytes(), &ar)

	// Memory status.
	sw := doJSON(t, h, "GET", "/api/v1/memories/"+id+"/status", "", nil)
	if sw.Code != http.StatusOK {
		t.Fatalf("memory status = %d; body=%s", sw.Code, sw.Body.String())
	}
	var st memoryStatusResponse
	mustJSON(t, sw.Body.Bytes(), &st)
	if st.MemoryID != id || len(st.Resources) != 1 {
		t.Fatalf("unexpected status: %+v", st)
	}

	// Resource status.
	rsw := doJSON(t, h, "GET", "/api/v1/memories/"+id+"/resources/"+ar.ResourceID+"/status", "", nil)
	if rsw.Code != http.StatusOK {
		t.Fatalf("resource status = %d", rsw.Code)
	}
	var rst resourceStatusView
	mustJSON(t, rsw.Body.Bytes(), &rst)
	if rst.ID != ar.ResourceID {
		t.Fatalf("resource status id = %q, want %q", rst.ID, ar.ResourceID)
	}

	// Unknown resource -> 404.
	if w := doJSON(t, h, "GET", "/api/v1/memories/"+id+"/resources/00000000000000000000000000000000/status", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown resource status = %d, want 404", w.Code)
	}
}

// jsonString JSON-encodes s as a quoted string literal for embedding in a body.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
