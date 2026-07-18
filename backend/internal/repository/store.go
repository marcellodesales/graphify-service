package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a repository job does not exist.
var ErrNotFound = errors.New("repository: not found")

// Store is a filesystem-backed metadata store (spec §7.4). Metadata is written
// atomically (temp file + fsync + rename) and never modified in place. A
// per-ID mutex serializes concurrent updates within this process.
type Store struct {
	layout Layout

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewStore creates (if needed) the repos root and returns a Store.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("repository: empty repos root")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("repository: create repos root: %w", err)
	}
	return &Store{layout: NewLayout(root), locks: make(map[string]*sync.Mutex)}, nil
}

// Layout exposes the store's path layout.
func (s *Store) Layout() Layout { return s.layout }

func (s *Store) lockFor(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

// Create idempotently persists a new queued job. If a job with the same ID
// already exists, the existing metadata is returned with created=false and no
// write occurs (spec §5.3).
func (s *Store) Create(meta Metadata) (Metadata, bool, error) {
	if !ValidID(meta.ID) {
		return Metadata{}, false, fmt.Errorf("repository: invalid id %q", meta.ID)
	}
	lock := s.lockFor(meta.ID)
	lock.Lock()
	defer lock.Unlock()

	if existing, err := s.readUnlocked(meta.ID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, false, err
	}

	now := time.Now().UTC()
	meta.SchemaVersion = SchemaVersion
	meta.Status = StatusQueued
	meta.Timestamps.CreatedAt = now
	meta.Timestamps.UpdatedAt = now
	if meta.Artifacts == nil {
		meta.Artifacts = []Artifact{}
	}

	if err := os.MkdirAll(s.layout.JobDir(meta.ID), 0o750); err != nil {
		return Metadata{}, false, fmt.Errorf("repository: create job dir: %w", err)
	}
	if err := s.writeAtomic(meta); err != nil {
		return Metadata{}, false, err
	}
	return meta, true, nil
}

// Get returns the metadata for id, or ErrNotFound.
func (s *Store) Get(id string) (Metadata, error) {
	if !ValidID(id) {
		return Metadata{}, ErrNotFound
	}
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	return s.readUnlocked(id)
}

// Update applies mutate to the current metadata for id and persists it
// atomically under the per-id lock. It bumps UpdatedAt. Returns the new record.
func (s *Store) Update(id string, mutate func(*Metadata) error) (Metadata, error) {
	if !ValidID(id) {
		return Metadata{}, ErrNotFound
	}
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m, err := s.readUnlocked(id)
	if err != nil {
		return Metadata{}, err
	}
	if err := mutate(&m); err != nil {
		return Metadata{}, err
	}
	m.Timestamps.UpdatedAt = time.Now().UTC()
	if err := s.writeAtomic(m); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// ListFilter narrows and pages a List call.
type ListFilter struct {
	Status Status // optional
	Host   string // optional
	Owner  string // optional
	Limit  int    // default 50, max 200
	Cursor string // last id from a previous page (exclusive)
}

// ListResult is a page of repositories plus the next cursor (empty when done).
type ListResult struct {
	Repositories []Metadata
	NextCursor   string
}

// List scans the repos root and returns jobs sorted by ID, applying filters
// and cursor-based pagination.
func (s *Store) List(f ListFilter) (ListResult, error) {
	entries, err := os.ReadDir(s.layout.Root())
	if err != nil {
		return ListResult{}, fmt.Errorf("repository: list: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	out := make([]Metadata, 0, limit)
	next := ""
	for _, id := range ids {
		if f.Cursor != "" && id <= f.Cursor {
			continue
		}
		meta, err := s.Get(id)
		if err != nil {
			continue // skip unreadable/partial jobs
		}
		if !matches(meta, f) {
			continue
		}
		if len(out) == limit {
			next = out[len(out)-1].ID
			break
		}
		out = append(out, meta)
	}
	return ListResult{Repositories: out, NextCursor: next}, nil
}

func matches(m Metadata, f ListFilter) bool {
	if f.Status != "" && m.Status != f.Status {
		return false
	}
	if f.Host != "" && m.Source.Host != f.Host {
		return false
	}
	if f.Owner != "" && m.Source.OwnerPath != f.Owner {
		return false
	}
	return true
}

func (s *Store) readUnlocked(id string) (Metadata, error) {
	b, err := os.ReadFile(s.layout.MetadataPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, ErrNotFound
		}
		return Metadata{}, fmt.Errorf("repository: read metadata: %w", err)
	}
	var m Metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return Metadata{}, fmt.Errorf("repository: decode metadata: %w", err)
	}
	return m, nil
}

// writeAtomic writes meta to metadata.json via a temp file + fsync + rename so
// readers never observe a partially written document.
func (s *Store) writeAtomic(meta Metadata) error {
	dir := s.layout.JobDir(meta.ID)
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("repository: encode metadata: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "metadata-*.json.tmp")
	if err != nil {
		return fmt.Errorf("repository: temp metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("repository: write metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("repository: sync metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("repository: close metadata: %w", err)
	}
	if err := os.Rename(tmpName, s.layout.MetadataPath(meta.ID)); err != nil {
		return fmt.Errorf("repository: rename metadata: %w", err)
	}
	// Best-effort fsync of the directory so the rename is durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
