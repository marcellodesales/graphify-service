package repository

import (
	"crypto/sha256"
	"encoding/hex"
)

// SelectorType identifies how a specific revision of a repository was requested.
type SelectorType string

const (
	// SelectorDefault clones the remote's default branch.
	SelectorDefault SelectorType = "default"
	// SelectorRef clones a specific branch or tag.
	SelectorRef SelectorType = "ref"
	// SelectorSHA clones a specific commit SHA.
	SelectorSHA SelectorType = "sha"
)

// Selector is the revision selector portion of a repository request.
type Selector struct {
	Type  SelectorType `json:"type"`
	Value string       `json:"value"`
}

// ComputeID returns the deterministic repository/job ID:
//
//	SHA-256(canonicalURL + "\n" + selectorType + "\n" + selectorValue)
//
// The same canonical URL and selector always produce the same ID (spec §5.1).
func ComputeID(canonicalURL string, sel Selector) string {
	h := sha256.New()
	h.Write([]byte(canonicalURL))
	h.Write([]byte("\n"))
	h.Write([]byte(sel.Type))
	h.Write([]byte("\n"))
	h.Write([]byte(sel.Value))
	return hex.EncodeToString(h.Sum(nil))
}
