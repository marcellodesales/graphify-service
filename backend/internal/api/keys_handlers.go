package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/memory"
)

// handleAddKey implements POST /api/v1/memories/{id}/keys: provision a
// first-class, reusable SSH key on the memory. The key material is written under
// the memory's gitignored .ssh/keys/ tree (mode 0600) BEFORE the metadata is
// persisted, so a later git-add referencing it always finds it on disk. Only a
// fingerprint (a one-way digest, not the secret) plus timestamps are stored in
// memory.json; the material itself is never persisted, returned, or logged.
func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
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

	var req addKeyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	if t := strings.TrimSpace(req.Type); t != "" && t != "ssh" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "type must be \"ssh\"")
		return
	}
	sshKey := strings.TrimSpace(req.SSHKey)
	if !memory.LooksLikePrivateKey([]byte(sshKey)) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "sshKey must be a PEM private key (not a public key)")
		return
	}

	keyID, err := memory.NewKeyID()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to allocate key id")
		return
	}
	if err := memory.WriteMemoryKey(s.memStore.Layout(), id, keyID, []byte(sshKey), []byte(req.KnownHosts)); err != nil {
		s.logger.Error("store memory key", "request_id", RequestIDFrom(r.Context()), "id", id, "key", keyID, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to store ssh key")
		return
	}

	knownHostsStored := strings.TrimSpace(req.KnownHosts) != ""
	_, stored, err := s.memStore.AddKey(id, memory.Key{
		ID:               keyID,
		Name:             strings.TrimSpace(req.Name),
		Type:             "ssh",
		Fingerprint:      memory.KeyFingerprint([]byte(sshKey)),
		KnownHostsStored: knownHostsStored,
	})
	if err != nil {
		// The material was written but metadata failed; best-effort cleanup so we
		// don't leave an orphaned, unreferenced key on disk.
		_ = memory.RemoveMemoryKey(s.memStore.Layout(), id, keyID)
		s.logger.Error("add memory key", "request_id", RequestIDFrom(r.Context()), "id", id, "key", keyID, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to persist key")
		return
	}
	writeJSON(w, r, http.StatusCreated, keyViewFor(id, stored))
}

// handleListKeys implements GET /api/v1/memories/{id}/keys (metadata only).
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	views := make([]keyView, 0, len(m.Keys))
	for _, k := range m.Keys {
		views = append(views, keyViewFor(m.ID, k))
	}
	writeJSON(w, r, http.StatusOK, keyListResponse{MemoryID: m.ID, Keys: views})
}

// handleGetKey implements GET /api/v1/memories/{id}/keys/{keyId} (metadata only).
func (s *Server) handleGetKey(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMemory(w, r)
	if !ok {
		return
	}
	keyID := r.PathValue("keyId")
	if !memory.ValidKeyID(keyID) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid key id")
		return
	}
	k, found := memory.FindKey(m, keyID)
	if !found {
		writeError(w, r, http.StatusNotFound, "not_found", "key not found")
		return
	}
	writeJSON(w, r, http.StatusOK, keyViewFor(m.ID, k))
}

// handleRotateKey implements PUT /api/v1/memories/{id}/keys/{keyId}: replace a
// provisioned key's material in place. Every git resource that references this
// key id automatically picks up the new material on its next ingest — no
// re-add needed. The new fingerprint and RotatedAt are persisted; no secret is.
func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	keyID := r.PathValue("keyId")
	if !memory.ValidID(id) || !memory.ValidKeyID(keyID) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid memory or key id")
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
	if _, found := memory.FindKey(m, keyID); !found {
		writeError(w, r, http.StatusNotFound, "not_found", "key not found")
		return
	}

	var req rotateKeyRequest
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
	if err := memory.WriteMemoryKey(s.memStore.Layout(), id, keyID, []byte(sshKey), []byte(req.KnownHosts)); err != nil {
		s.logger.Error("rotate memory key", "request_id", RequestIDFrom(r.Context()), "id", id, "key", keyID, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to store ssh key")
		return
	}

	knownHostsStored := strings.TrimSpace(req.KnownHosts) != ""
	name := strings.TrimSpace(req.Name)
	updated, err := s.memStore.UpdateKey(id, keyID, func(k *memory.Key) error {
		k.Fingerprint = memory.KeyFingerprint([]byte(sshKey))
		k.KnownHostsStored = knownHostsStored
		if name != "" {
			k.Name = name
		}
		now := time.Now().UTC()
		k.RotatedAt = &now
		return nil
	})
	if err != nil {
		s.logger.Error("persist rotated key", "request_id", RequestIDFrom(r.Context()), "id", id, "key", keyID, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to update key")
		return
	}
	k, _ := memory.FindKey(updated, keyID)
	writeJSON(w, r, http.StatusOK, keyViewFor(id, k))
}

// handleDeleteKey implements DELETE /api/v1/memories/{id}/keys/{keyId}. It
// refuses (409) while any git resource still references the key, then removes
// both the metadata and the on-disk material.
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	keyID := r.PathValue("keyId")
	if !memory.ValidID(id) || !memory.ValidKeyID(keyID) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid memory or key id")
		return
	}
	if _, err := s.memStore.DeleteKey(id, keyID); err != nil {
		switch {
		case errors.Is(err, memory.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "not_found", "memory not found")
		case errors.Is(err, memory.ErrKeyInUse):
			writeError(w, r, http.StatusConflict, "key_in_use", "key is referenced by a resource; remove those resources first")
		case strings.Contains(err.Error(), "not found"):
			writeError(w, r, http.StatusNotFound, "not_found", "key not found")
		default:
			s.logger.Error("delete memory key", "request_id", RequestIDFrom(r.Context()), "id", id, "key", keyID, "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to delete key")
		}
		return
	}
	// Metadata is gone; remove the material (best-effort — a leftover unreferenced
	// key file is harmless and gitignored).
	if err := memory.RemoveMemoryKey(s.memStore.Layout(), id, keyID); err != nil {
		s.logger.Error("remove key material", "request_id", RequestIDFrom(r.Context()), "id", id, "key", keyID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// keyViewFor renders a key as its API view (metadata + links, never material).
func keyViewFor(memID string, k memory.Key) keyView {
	return keyView{
		Key: k,
		Links: keyLinks{
			Self:   memPath(memID) + "/keys/" + k.ID,
			Memory: memPath(memID),
		},
	}
}

// boolCount returns how many of the given conditions are true.
func boolCount(conds ...bool) int {
	n := 0
	for _, c := range conds {
		if c {
			n++
		}
	}
	return n
}
