package memory

import (
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// SchemaVersion is the current memory.json schema version.
const SchemaVersion = 1

// Memory is the authoritative record for a user-defined memory: a mutable
// collection of any number of resources graphify accepts (git repositories —
// one or more — plus uploaded files like PDF/doc/markdown), merged into one
// unified knowledge graph at a centralized location.
//
// It is persisted at <memories-root>/<id>/memory.json and is the manifest for a
// working git repository rooted at the same directory; every mutation commits
// and the new git HEAD SHA is surfaced as GraphRef.
//
// Secrets (private keys, passphrases, tokens, authenticated URLs) are never
// persisted here.
type Memory struct {
	SchemaVersion int                 `json:"schemaVersion"`
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Description   string              `json:"description,omitempty"`
	Status        Status              `json:"status"`
	Stage         string              `json:"stage,omitempty"`
	Resources     []Resource          `json:"resources"`
	GraphRef      string              `json:"graphRef,omitempty"` // git HEAD SHA of the memory repo
	Artifacts     []repository.Artifact `json:"artifacts"`        // the merged graph outputs (graphify-out/)
	Timestamps    Timestamps          `json:"timestamps"`
	Failure       *repository.Failure `json:"failure"`
}

// Kind is the type of a resource within a memory.
type Kind string

const (
	// KindGit is a git repository resource (cloned, then graphify-extracted).
	KindGit Kind = "git"
	// KindFile is an uploaded file resource (PDF, docx, md, …) that graphify's
	// detect.py already classifies and extracts. NOT vector RAG — see docs.
	KindFile Kind = "file"
)

// Resource is one source belonging to a memory. Each resource is graphified
// independently into its own graphify-out/graph.json, and all resource graphs
// are later merged into the memory's unified graph.
//
// Secrets (private keys, passphrases, tokens, authenticated URLs) are never
// persisted here.
type Resource struct {
	ID       string            `json:"id"`
	Kind     Kind              `json:"kind"`
	Status   repository.Status `json:"status"` // reuses the repository lifecycle per resource
	Stage    string            `json:"stage,omitempty"`

	// Git resources.
	Source   repository.Source   `json:"source,omitempty"`
	Selector repository.Selector `json:"selector,omitempty"`
	ResolvedSHA string           `json:"resolvedSha,omitempty"`

	// File resources.
	FileName string `json:"fileName,omitempty"` // original upload name (sanitized)
	Size     int64  `json:"size,omitempty"`

	// RelPath is the resource's location relative to the memory dir, e.g.
	// git/<host>/<owner>/<repo> or files/<name>. GraphOutPath is the relative
	// path to its graphify-out directory once extracted.
	RelPath      string `json:"relPath,omitempty"`
	GraphOutPath string `json:"graphOutPath,omitempty"`

	Failure *repository.Failure `json:"failure,omitempty"`

	AddedAt   time.Time `json:"addedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Timestamps records the memory's lifecycle instants.
type Timestamps struct {
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	MergeStartedAt  *time.Time `json:"mergeStartedAt"`
	MergeFinishedAt *time.Time `json:"mergeFinishedAt"`
}
