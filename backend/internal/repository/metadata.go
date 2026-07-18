package repository

import "time"

// SchemaVersion is the current metadata.json schema version.
const SchemaVersion = 1

// Metadata is the authoritative per-job record persisted at
// <repos-root>/<id>/metadata.json (spec §7.3).
//
// Secrets (private keys, passphrases, tokens, authenticated URLs) are never
// persisted here.
type Metadata struct {
	SchemaVersion int        `json:"schemaVersion"`
	ID            string     `json:"id"`
	Status        Status     `json:"status"`
	Stage         string     `json:"stage,omitempty"`
	Source        Source     `json:"source"`
	Selector      Selector   `json:"selector"`
	ResolvedSHA   string     `json:"resolvedSha,omitempty"`
	Attempts      Attempts   `json:"attempts"`
	Timestamps    Timestamps `json:"timestamps"`
	Artifacts     []Artifact `json:"artifacts"`
	Failure       *Failure   `json:"failure"`
}

// Source captures the non-secret human-readable origin of a repository.
type Source struct {
	NormalizedURL     string `json:"normalizedUrl"`
	Host              string `json:"host"`
	OwnerPath         string `json:"ownerPath"`
	Repository        string `json:"repository"`
	Transport         string `json:"transport"`
	Private           bool   `json:"private"`
	SSHKeyRef         string `json:"sshKeyRef,omitempty"`
	DefaultBranch     string `json:"defaultBranch,omitempty"`     // resolved at clone time
	HasCommittedGraph bool   `json:"hasCommittedGraph,omitempty"` // repo already contains graphify-out/
	GraphOutPath      string `json:"graphOutPath,omitempty"`      // relative, e.g. graphify-out
}

// Attempts counts how many times each stage has run.
type Attempts struct {
	Clone    int `json:"clone"`
	Graphify int `json:"graphify"`
}

// Timestamps records lifecycle instants. Optional stage instants are pointers
// so they serialize as null until set.
type Timestamps struct {
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	CloneStartedAt     *time.Time `json:"cloneStartedAt"`
	CloneFinishedAt    *time.Time `json:"cloneFinishedAt"`
	GraphifyStartedAt  *time.Time `json:"graphifyStartedAt"`
	GraphifyFinishedAt *time.Time `json:"graphifyFinishedAt"`
}

// Artifact is one downloadable output produced by the graphify worker.
type Artifact struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Failure records a safe, non-secret failure summary.
type Failure struct {
	Stage   string    `json:"stage"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}
