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
type addGitResourceRequest struct {
	GitRepoURL string `json:"gitRepoUrl"`
	Ref        string `json:"ref"`
	Sha        string `json:"sha"`
	SSHKeyRef  string `json:"sshKeyRef"`
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
