package memory

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// idPattern matches a memory ID: 32 lowercase hex chars (16 random bytes).
//
// Unlike repository IDs (which are content-addressed SHA-256, 64 hex), a memory
// is a mutable, user-defined collection whose contents change over time. It is
// therefore NOT content-addressed — it gets a random opaque identifier at
// creation and keeps it for its lifetime. The git HEAD SHA of the memory's
// working repo (GraphRef) is what changes on every mutation.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewID returns a fresh random 32-hex memory ID.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidID reports whether id is a well-formed memory ID. Every filesystem
// lookup keyed by a memory ID MUST be guarded by this check to prevent path
// traversal (a caller-supplied id is otherwise used to build a path).
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// resourcePattern matches a resource ID: 32 lowercase hex chars.
var resourcePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewResourceID returns a fresh random 32-hex resource ID (a source within a
// memory: a git repo or an uploaded file).
func NewResourceID() (string, error) { return NewID() }

// ValidResourceID reports whether rid is a well-formed resource ID.
func ValidResourceID(rid string) bool { return resourcePattern.MatchString(rid) }
