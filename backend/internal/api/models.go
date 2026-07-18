package api

import "github.com/marcellodesales/graphify-service/backend/internal/repository"

// submitRequest is the POST /api/v1/repositories body (spec §6.2).
type submitRequest struct {
	GithubRepoURL string `json:"githubRepoUrl"`
	GithubRef     string `json:"githubRef"`
	GithubSha     string `json:"githubSha"`
	SSHKeyRef     string `json:"sshKeyRef"`
	Force         bool   `json:"force"`
}

// submitResponse is returned from a successful submit (spec §6.2).
type submitResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	StatusURL    string `json:"statusUrl"`
	ArtifactsURL string `json:"artifactsUrl"`
}

// links is the hypermedia block attached to a repository view (spec §6.4).
type links struct {
	Self        string `json:"self"`
	Artifacts   string `json:"artifacts"`
	DownloadZip string `json:"downloadZip"`
}

// repositoryView is the GET /api/v1/repositories/{id} response.
type repositoryView struct {
	repository.Metadata
	Links links `json:"links"`
}

// listResponse is the GET /api/v1/repositories response (spec §6.3).
type listResponse struct {
	Repositories []repositoryView `json:"repositories"`
	NextCursor   *string          `json:"nextCursor"`
}

// errorResponse is the standard error envelope (spec §6.1).
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}
