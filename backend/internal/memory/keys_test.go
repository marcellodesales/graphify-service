package memory

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func createTestMemory(t *testing.T, s *Store) Memory {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Create(Memory{ID: id, Name: "t", Status: StatusCreated})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m
}

func TestKeyFingerprintStableAndNormalized(t *testing.T) {
	// The fingerprint must be stable across CRLF/trailing-whitespace noise (both
	// storage and fingerprint apply the same normalization) and must NOT equal the
	// key bytes (it is a one-way digest).
	// Trailing CR/LF/space is normalized away before hashing (the same
	// normalization applied when the key is written to disk), so trailing-only
	// differences yield the same fingerprint.
	base := testPrivKey
	fp1 := KeyFingerprint([]byte(base))
	fp2 := KeyFingerprint([]byte(base + "\r\n\n  "))
	if fp1 != fp2 {
		t.Errorf("fingerprint not stable across trailing whitespace: %q != %q", fp1, fp2)
	}
	if len(fp1) != 64 {
		t.Errorf("fingerprint length = %d, want 64 (sha256 hex)", len(fp1))
	}
	if strings.Contains(fp1, "PRIVATE KEY") || fp1 == base {
		t.Error("fingerprint leaks key material")
	}
	// Different key -> different fingerprint.
	other := "-----BEGIN RSA PRIVATE KEY-----\ndifferent\n-----END RSA PRIVATE KEY-----"
	if KeyFingerprint([]byte(other)) == fp1 {
		t.Error("distinct keys produced the same fingerprint")
	}
}

func TestWriteMemoryKeySeparateFromResourceKeys(t *testing.T) {
	l := NewLayout(t.TempDir())
	const id = "0123456789abcdef0123456789abcdef"
	const keyID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.MkdirAll(l.MemoryDir(id), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := WriteMemoryKey(l, id, keyID, []byte(testPrivKey), []byte("github.com ssh-ed25519 AAAA")); err != nil {
		t.Fatalf("WriteMemoryKey: %v", err)
	}
	if !HasMemoryKey(l, id, keyID) {
		t.Fatal("HasMemoryKey=false after write")
	}
	if !HasMemoryKeyKnownHosts(l, id, keyID) {
		t.Error("HasMemoryKeyKnownHosts=false after write with knownHosts")
	}

	kp := l.KeyPath(id, keyID)
	fi, err := os.Stat(kp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode=%v want 0600", fi.Mode().Perm())
	}
	// Lives under .ssh/keys/, distinct from the legacy per-resource .ssh/<rid>.
	if !strings.HasPrefix(kp, l.KeyDir(id)+string(os.PathSeparator)) {
		t.Errorf("key path %q not under key dir %q", kp, l.KeyDir(id))
	}
	// A resource key with the same 32-hex id would sit at .ssh/<id> — must differ.
	if kp == l.SSHKeyPath(id, keyID) {
		t.Errorf("memory key path collides with resource key path: %q", kp)
	}
	// Never under git/, files/, or graphify-out/ (would be extracted/committed).
	for _, forbidden := range []string{l.GitDir(id), l.FilesDir(id), l.GraphOutDir(id)} {
		if strings.HasPrefix(kp, forbidden+string(os.PathSeparator)) {
			t.Errorf("key path %q must not live under %q", kp, forbidden)
		}
	}

	// Rejects a public key.
	if err := WriteMemoryKey(l, id, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []byte("ssh-ed25519 AAAA u@h"), nil); err == nil {
		t.Error("expected error for public key")
	}

	// RemoveMemoryKey is idempotent and clears material + known_hosts.
	if err := RemoveMemoryKey(l, id, keyID); err != nil {
		t.Fatalf("RemoveMemoryKey: %v", err)
	}
	if HasMemoryKey(l, id, keyID) || HasMemoryKeyKnownHosts(l, id, keyID) {
		t.Error("key material present after RemoveMemoryKey")
	}
	if err := RemoveMemoryKey(l, id, keyID); err != nil {
		t.Errorf("RemoveMemoryKey not idempotent: %v", err)
	}
}

func TestAddUpdateDeleteKey(t *testing.T) {
	s := newTestStore(t)
	m := createTestMemory(t, s)

	_, k, err := s.AddKey(m.ID, Key{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Fingerprint: "fp1"})
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if k.Type != "ssh" {
		t.Errorf("Type defaulted to %q, want ssh", k.Type)
	}
	if k.CreatedAt.IsZero() || k.UpdatedAt.IsZero() {
		t.Error("AddKey did not stamp timestamps")
	}

	// Duplicate id is rejected.
	if _, _, err := s.AddKey(m.ID, Key{ID: k.ID}); err == nil {
		t.Error("expected error adding duplicate key id")
	}

	// FindKey.
	reread, _ := s.Get(m.ID)
	if got, ok := FindKey(reread, k.ID); !ok || got.Fingerprint != "fp1" {
		t.Errorf("FindKey = %+v ok=%v", got, ok)
	}
	if _, ok := FindKey(reread, "ffffffffffffffffffffffffffffffff"); ok {
		t.Error("FindKey found a non-existent key")
	}

	// Rotate: new fingerprint + RotatedAt set.
	if _, err := s.UpdateKey(m.ID, k.ID, func(kk *Key) error {
		kk.Fingerprint = "fp2"
		now := reread.Timestamps.CreatedAt
		kk.RotatedAt = &now
		return nil
	}); err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	reread, _ = s.Get(m.ID)
	got, _ := FindKey(reread, k.ID)
	if got.Fingerprint != "fp2" || got.RotatedAt == nil {
		t.Errorf("rotate did not persist: %+v", got)
	}

	// Delete.
	if _, err := s.DeleteKey(m.ID, k.ID); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	reread, _ = s.Get(m.ID)
	if _, ok := FindKey(reread, k.ID); ok {
		t.Error("key still present after delete")
	}
	// Deleting a missing key errors.
	if _, err := s.DeleteKey(m.ID, k.ID); err == nil {
		t.Error("expected error deleting missing key")
	}
}

func TestDeleteKeyRefusedWhileReferenced(t *testing.T) {
	s := newTestStore(t)
	m := createTestMemory(t, s)
	const keyID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, _, err := s.AddKey(m.ID, Key{ID: keyID}); err != nil {
		t.Fatal(err)
	}
	// A resource references the key.
	rid, _ := NewResourceID()
	if _, _, err := s.AddResource(m.ID, Resource{
		ID:     rid,
		Kind:   KindGit,
		Source: repository.Source{Repository: "r", KeyID: keyID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteKey(m.ID, keyID); !errors.Is(err, ErrKeyInUse) {
		t.Fatalf("DeleteKey = %v, want ErrKeyInUse", err)
	}
	// Once the reference is cleared, delete succeeds.
	if _, err := s.UpdateResource(m.ID, rid, func(r *Resource) error {
		r.Source.KeyID = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteKey(m.ID, keyID); err != nil {
		t.Fatalf("DeleteKey after unref: %v", err)
	}
}

func TestKeyMaterialNeverInManifest(t *testing.T) {
	s := newTestStore(t)
	m := createTestMemory(t, s)
	const keyID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := WriteMemoryKey(s.Layout(), m.ID, keyID, []byte(testPrivKey), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddKey(m.ID, Key{ID: keyID, Fingerprint: KeyFingerprint([]byte(testPrivKey))}); err != nil {
		t.Fatal(err)
	}
	// The persisted manifest must contain the fingerprint but NEVER the material.
	raw, err := os.ReadFile(s.Layout().MetadataPath(m.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("memory.json contains private key material")
	}
	var back Memory
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got, ok := FindKey(back, keyID); !ok || got.Fingerprint == "" {
		t.Errorf("fingerprint not round-tripped: %+v ok=%v", got, ok)
	}
}
