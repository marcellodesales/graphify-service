package repository

import "path/filepath"

// Layout resolves on-disk paths for a repository job under a repos root.
//
// The job ID is a SHA-256 hex string and is validated with ValidID before use,
// so URL-derived components never control filesystem paths (spec §4.2, §4.3).
type Layout struct {
	root string
}

// NewLayout returns a Layout rooted at root.
func NewLayout(root string) Layout { return Layout{root: root} }

// Root returns the repos root directory.
func (l Layout) Root() string { return l.root }

// TmpRoot returns the directory holding in-progress temporary clones.
func (l Layout) TmpRoot() string { return filepath.Join(l.root, ".tmp") }

// JobDir returns <root>/<id>.
func (l Layout) JobDir(id string) string { return filepath.Join(l.root, id) }

// MetadataPath returns <root>/<id>/metadata.json.
func (l Layout) MetadataPath(id string) string {
	return filepath.Join(l.root, id, "metadata.json")
}

// RepositoryDir returns <root>/<id>/repository.
func (l Layout) RepositoryDir(id string) string {
	return filepath.Join(l.root, id, "repository")
}

// LogsDir returns <root>/<id>/logs.
func (l Layout) LogsDir(id string) string { return filepath.Join(l.root, id, "logs") }

// LocksDir returns <root>/<id>/locks.
func (l Layout) LocksDir(id string) string { return filepath.Join(l.root, id, "locks") }

// ValidID reports whether id is a 64-character lowercase hex SHA-256 string.
// This guards every filesystem lookup against path traversal.
func ValidID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
