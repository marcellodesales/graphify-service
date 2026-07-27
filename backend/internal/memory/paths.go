package memory

import "path/filepath"

// Layout resolves the on-disk paths for the memory store. A memory lives at
// <root>/<id> where <root> is typically <repos-root>/memories. That directory
// is itself a git repository:
//
//	<root>/<id>/
//	  .git/                      # working repo; HEAD SHA is the returned GraphRef
//	  .gitignore                 # ignores git/, files/, .tmp/, .ssh/ (working data)
//	  README.md                  # committed at create
//	  memory.json                # the manifest (this package's Metadata)
//	  graphify-out/              # the merged unified graph (committed)
//	    graph.json graph.html GRAPH_REPORT.md manifest.json
//	  git/<host>/<owner>/<repo>/ # raw clone + graphify-out/graph.json (gitignored)
//	  files/<name>/              # raw upload + graphify-out/graph.json (gitignored)
//	  .ssh/<resourceId>          # caller-supplied deploy key for a private git
//	                             # resource, mode 0600 (gitignored — never
//	                             # committed, never in GraphRef, never extracted)
//
// The merged output lives at the conventional graphify-out/ directory name so the
// existing artifacts package (Inventory/Select/Zip) and graphify.Enrich, which all
// operate on <dir>/graphify-out, apply to the memory dir with no copying.
//
// Raw clones and uploads are deliberately gitignored: they are large, may carry
// their own nested .git, and are reproducible working data. Only the manifest
// and the merged output are versioned, so the returned ref is a stable,
// meaningful pointer to the memory's knowledge-graph state.
type Layout struct {
	root string
}

// NewLayout returns a Layout rooted at root (the memories subdirectory).
func NewLayout(root string) Layout { return Layout{root: root} }

// Root is the memories root directory.
func (l Layout) Root() string { return l.root }

// TmpRoot is a scratch directory for atomic writes and staging clones.
func (l Layout) TmpRoot() string { return filepath.Join(l.root, ".tmp") }

// MemoryDir is the per-memory directory (and git repo root).
func (l Layout) MemoryDir(id string) string { return filepath.Join(l.root, id) }

// MetadataPath is the memory manifest.
func (l Layout) MetadataPath(id string) string { return filepath.Join(l.MemoryDir(id), "memory.json") }

// ReadmePath is the committed human-readable README.
func (l Layout) ReadmePath(id string) string { return filepath.Join(l.MemoryDir(id), "README.md") }

// GitignorePath is the committed .gitignore.
func (l Layout) GitignorePath(id string) string { return filepath.Join(l.MemoryDir(id), ".gitignore") }

// GitDir holds all git-repo resources for a memory.
func (l Layout) GitDir(id string) string { return filepath.Join(l.MemoryDir(id), "git") }

// GitResourceDir is where a single git resource is cloned. Host/owner/repo are
// used verbatim to build a readable, collision-free path; callers must supply
// values already validated by giturl.Parse (no traversal segments).
func (l Layout) GitResourceDir(id, host, owner, repo string) string {
	return filepath.Join(l.GitDir(id), host, filepath.FromSlash(owner), repo)
}

// FilesDir holds all uploaded-file resources for a memory.
func (l Layout) FilesDir(id string) string { return filepath.Join(l.MemoryDir(id), "files") }

// FileResourceDir is the directory for a single uploaded-file resource. name
// must be a validated resource ID (hex), not the original filename.
func (l Layout) FileResourceDir(id, name string) string {
	return filepath.Join(l.FilesDir(id), name)
}

// SSHDir holds caller-supplied deploy keys for a memory's private git
// resources. It is gitignored (see WriteScaffold) so keys are never committed
// to the memory repo and never surface via GraphRef, and it lives OUTSIDE git/
// and files/ so graphify never extracts a key into any graph.
func (l Layout) SSHDir(id string) string { return filepath.Join(l.MemoryDir(id), ".ssh") }

// SSHKeyPath is the stored deploy key for a single git resource, keyed by
// resource ID. rid MUST be a validated resource ID (hex) — never caller free
// text — since it is used to build this path.
func (l Layout) SSHKeyPath(id, rid string) string {
	return filepath.Join(l.SSHDir(id), rid)
}

// SSHKnownHostsPath is the optional stored known_hosts for a git resource. Host
// keys are public (not a secret) and are stored alongside the key so a
// caller-supplied key can verify its host without relying on global ops config.
func (l Layout) SSHKnownHostsPath(id, rid string) string {
	return filepath.Join(l.SSHDir(id), rid+".known_hosts")
}

// KeyDir holds a memory's provisioned, first-class SSH keys — keyed by KEY id
// (not resource id) so one key can be referenced by many git resources and
// rotated independently. It lives under the gitignored .ssh/ tree (never
// committed, never in GraphRef, never extracted) in its own keys/ subdirectory
// so a key id can never collide with a legacy per-resource key at .ssh/<rid>.
func (l Layout) KeyDir(id string) string { return filepath.Join(l.SSHDir(id), "keys") }

// KeyPath is the stored private key material for a provisioned key. keyID MUST
// be a validated key ID (hex) — never caller free text — since it builds this
// path. The material here is NEVER committed and NEVER persisted in memory.json.
func (l Layout) KeyPath(id, keyID string) string {
	return filepath.Join(l.KeyDir(id), keyID)
}

// KeyKnownHostsPath is the optional stored known_hosts for a provisioned key.
// Host keys are public (not a secret).
func (l Layout) KeyKnownHostsPath(id, keyID string) string {
	return filepath.Join(l.KeyDir(id), keyID+".known_hosts")
}

// GraphOutDir is where the merged unified graph is written (committed). It uses
// the conventional graphify-out name so artifacts.Inventory/Zip and
// graphify.Enrich, which read <dir>/graphify-out, operate on the memory dir
// directly with no file copying.
func (l Layout) GraphOutDir(id string) string {
	return filepath.Join(l.MemoryDir(id), "graphify-out")
}
