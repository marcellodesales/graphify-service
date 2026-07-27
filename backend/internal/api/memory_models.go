package api

import "github.com/marcellodesales/graphify-service/backend/internal/memory"

// createMemoryRequest is the POST /api/v1/memories body. A memory is created
// empty (no resources); sources are added afterwards via the resources endpoint.
type createMemoryRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// addGitResourceRequest is the JSON body for adding a git-repository source to a
// memory (Content-Type: application/json). File sources use multipart instead.
//
// A private repository is unlocked with an SSH deploy key supplied one of two
// ways (mutually exclusive):
//   - SSHKeyRef: the name of an ops-provisioned key already present on the
//     read-only SSH secrets volume (validated as a simple name);
//   - SSHKey: the caller-supplied PEM private key material itself, which the
//     service stores on its own volume alongside the resource and uses for the
//     clone. Like a password on a protected zip, it unlocks the clone task and
//     is NEVER persisted in the manifest.
//
// KnownHosts optionally carries the repo host's public host keys (not a secret)
// for strict host-key verification of a caller-supplied key.
// A private repo may alternatively reference a memory-scoped provisioned key by
// id (KeyID) — added once via POST .../keys and reusable across git-adds. KeyID,
// SSHKeyRef, and SSHKey are mutually exclusive.
type addGitResourceRequest struct {
	GitRepoURL string `json:"gitRepoUrl"`
	Ref        string `json:"ref"`
	Sha        string `json:"sha"`
	SSHKeyRef  string `json:"sshKeyRef"`
	SSHKey     string `json:"sshKey"`
	KeyID      string `json:"keyId"`
	KnownHosts string `json:"knownHosts"`
}

// setResourceSSHKeyRequest is the JSON body for PUT
// /api/v1/memories/{id}/resources/{rid}/ssh-key: supply or replace the deploy
// key for an existing git resource. The key material is stored on our volume
// and never persisted in the manifest.
type setResourceSSHKeyRequest struct {
	SSHKey     string `json:"sshKey"`
	KnownHosts string `json:"knownHosts"`
}

// setResourceSSHKeyResponse is returned when a key is accepted and the resource
// is re-queued for ingestion with the new key.
type setResourceSSHKeyResponse struct {
	MemoryID   string `json:"memoryId"`
	ResourceID string `json:"resourceId"`
	Status     string `json:"status"`
	Ref        string `json:"ref,omitempty"` // memory git HEAD SHA after the mutation
}

// addResourceResponse is returned when a source is accepted for ingestion.
type addResourceResponse struct {
	MemoryID   string `json:"memoryId"`
	ResourceID string `json:"resourceId"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Ref        string `json:"ref,omitempty"` // memory git HEAD SHA after the mutation
}

// memoryLinks is the hypermedia block attached to a memory view.
type memoryLinks struct {
	Self        string `json:"self"`
	Resources   string `json:"resources"`
	Keys        string `json:"keys"`
	Status      string `json:"status"`
	Graph       string `json:"graph"`
	Query       string `json:"query"`
	Artifacts   string `json:"artifacts"`
	DownloadZip string `json:"downloadZip"`
}

// memoryView is the GET /api/v1/memories/{id} response: the manifest plus links.
type memoryView struct {
	memory.Memory
	Links memoryLinks `json:"links"`
}

// memoryListResponse is the GET /api/v1/memories response.
type memoryListResponse struct {
	Memories   []memoryView `json:"memories"`
	NextCursor *string      `json:"nextCursor"`
}

// resourceListResponse is the GET /api/v1/memories/{id}/resources response.
type resourceListResponse struct {
	MemoryID  string            `json:"memoryId"`
	Resources []memory.Resource `json:"resources"`
}

// addKeyRequest is the POST /api/v1/memories/{id}/keys body: provision a
// first-class, reusable SSH key on the memory. Only the material is a secret;
// the service stores it under the gitignored .ssh/keys/ tree and persists just a
// fingerprint + timestamps. KnownHosts (public host keys) is optional.
type addKeyRequest struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // optional; defaults to "ssh"
	SSHKey     string `json:"sshKey"`
	KnownHosts string `json:"knownHosts"`
}

// rotateKeyRequest is the PUT /api/v1/memories/{id}/keys/{keyId} body: replace a
// provisioned key's material (and optionally its known_hosts and label).
type rotateKeyRequest struct {
	Name       string `json:"name"`
	SSHKey     string `json:"sshKey"`
	KnownHosts string `json:"knownHosts"`
}

// keyView is the API representation of a provisioned key: metadata only — the
// key material is NEVER returned.
type keyView struct {
	memory.Key
	Links keyLinks `json:"links"`
}

// keyLinks is the hypermedia block attached to a key view.
type keyLinks struct {
	Self   string `json:"self"`
	Memory string `json:"memory"`
}

// keyListResponse is the GET /api/v1/memories/{id}/keys response.
type keyListResponse struct {
	MemoryID string    `json:"memoryId"`
	Keys     []keyView `json:"keys"`
}
