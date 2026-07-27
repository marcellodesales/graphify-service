package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/repository"
)

// ErrNotFound is returned when a memory does not exist.
var ErrNotFound = errors.New("memory: not found")

// Store is a filesystem-backed memory metadata store. memory.json is written
// atomically (temp + fsync + rename) and never modified in place; a per-ID
// mutex serializes updates within this process. Mirrors repository.Store.
type Store struct {
	layout Layout

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewStore creates (if needed) the memories root and returns a Store.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("memory: empty memories root")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("memory: create memories root: %w", err)
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

// Create persists a new memory in the created state and makes its directory.
// The caller is responsible for git-initializing the directory and committing.
func (s *Store) Create(meta Memory) (Memory, error) {
	if !ValidID(meta.ID) {
		return Memory{}, fmt.Errorf("memory: invalid id %q", meta.ID)
	}
	lock := s.lockFor(meta.ID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := s.readUnlocked(meta.ID); err == nil {
		return Memory{}, fmt.Errorf("memory: %q already exists", meta.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return Memory{}, err
	}

	now := time.Now().UTC()
	meta.SchemaVersion = SchemaVersion
	if meta.Status == "" {
		meta.Status = StatusCreated
	}
	if meta.Resources == nil {
		meta.Resources = []Resource{}
	}
	if meta.Artifacts == nil {
		meta.Artifacts = []repository.Artifact{}
	}
	meta.Timestamps.CreatedAt = now
	meta.Timestamps.UpdatedAt = now

	if err := os.MkdirAll(s.layout.MemoryDir(meta.ID), 0o750); err != nil {
		return Memory{}, fmt.Errorf("memory: create dir: %w", err)
	}
	if err := s.writeAtomic(meta); err != nil {
		return Memory{}, err
	}
	return meta, nil
}

// Get returns the memory for id, or ErrNotFound.
func (s *Store) Get(id string) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrNotFound
	}
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	return s.readUnlocked(id)
}

// Update applies mutate under the per-id lock and persists atomically, bumping
// UpdatedAt. Returns the new record.
func (s *Store) Update(id string, mutate func(*Memory) error) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrNotFound
	}
	lock := s.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m, err := s.readUnlocked(id)
	if err != nil {
		return Memory{}, err
	}
	if err := mutate(&m); err != nil {
		return Memory{}, err
	}
	m.Timestamps.UpdatedAt = time.Now().UTC()
	if err := s.writeAtomic(m); err != nil {
		return Memory{}, err
	}
	return m, nil
}

// AddResource appends r to the memory (assigning AddedAt/UpdatedAt) under the
// per-id lock. Returns the updated memory and the stored resource.
func (s *Store) AddResource(id string, r Resource) (Memory, Resource, error) {
	now := time.Now().UTC()
	r.AddedAt = now
	r.UpdatedAt = now
	if r.Status == "" {
		r.Status = repository.StatusQueued
	}
	m, err := s.Update(id, func(md *Memory) error {
		md.Resources = append(md.Resources, r)
		return nil
	})
	if err != nil {
		return Memory{}, Resource{}, err
	}
	return m, r, nil
}

// UpdateResource applies mutate to the resource with rid under the per-id lock.
func (s *Store) UpdateResource(id, rid string, mutate func(*Resource) error) (Memory, error) {
	return s.Update(id, func(md *Memory) error {
		for i := range md.Resources {
			if md.Resources[i].ID == rid {
				if err := mutate(&md.Resources[i]); err != nil {
					return err
				}
				md.Resources[i].UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return fmt.Errorf("memory: resource %q not found", rid)
	})
}

// ErrKeyInUse is returned when a key delete is refused because a resource still
// references it.
var ErrKeyInUse = errors.New("memory: key is referenced by a resource")

// AddKey appends a provisioned key's metadata to the memory (assigning
// CreatedAt/UpdatedAt) under the per-id lock. The key MATERIAL must already have
// been written to disk (WriteMemoryKey) — only non-secret metadata is stored
// here. Returns the updated memory and the stored key.
func (s *Store) AddKey(id string, k Key) (Memory, Key, error) {
	now := time.Now().UTC()
	k.CreatedAt = now
	k.UpdatedAt = now
	if k.Type == "" {
		k.Type = "ssh"
	}
	m, err := s.Update(id, func(md *Memory) error {
		for _, existing := range md.Keys {
			if existing.ID == k.ID {
				return fmt.Errorf("memory: key %q already exists", k.ID)
			}
		}
		md.Keys = append(md.Keys, k)
		return nil
	})
	if err != nil {
		return Memory{}, Key{}, err
	}
	return m, k, nil
}

// UpdateKey applies mutate to the key with keyID under the per-id lock, bumping
// its UpdatedAt. Used for rotation (new fingerprint + RotatedAt).
func (s *Store) UpdateKey(id, keyID string, mutate func(*Key) error) (Memory, error) {
	return s.Update(id, func(md *Memory) error {
		for i := range md.Keys {
			if md.Keys[i].ID == keyID {
				if err := mutate(&md.Keys[i]); err != nil {
					return err
				}
				md.Keys[i].UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return fmt.Errorf("memory: key %q not found", keyID)
	})
}

// DeleteKey removes the key with keyID from the memory. It refuses (ErrKeyInUse)
// while any git resource still references the key, so a referenced key can never
// be pulled out from under a pending or future ingest.
func (s *Store) DeleteKey(id, keyID string) (Memory, error) {
	return s.Update(id, func(md *Memory) error {
		found := false
		for _, k := range md.Keys {
			if k.ID == keyID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("memory: key %q not found", keyID)
		}
		for _, r := range md.Resources {
			if r.Source.KeyID == keyID {
				return ErrKeyInUse
			}
		}
		kept := md.Keys[:0]
		for _, k := range md.Keys {
			if k.ID != keyID {
				kept = append(kept, k)
			}
		}
		md.Keys = kept
		return nil
	})
}

// FindKey returns the key with keyID from a memory snapshot, or false.
func FindKey(m Memory, keyID string) (Key, bool) {
	for _, k := range m.Keys {
		if k.ID == keyID {
			return k, true
		}
	}
	return Key{}, false
}

// ListFilter narrows and pages a List call.
type ListFilter struct {
	Status Status
	Limit  int
	Cursor string
}

// ListResult is a page of memories plus the next cursor (empty when done).
type ListResult struct {
	Memories   []Memory
	NextCursor string
}

// List scans the memories root and returns memories sorted by ID.
func (s *Store) List(f ListFilter) (ListResult, error) {
	entries, err := os.ReadDir(s.layout.Root())
	if err != nil {
		return ListResult{}, fmt.Errorf("memory: list: %w", err)
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

	out := make([]Memory, 0, limit)
	next := ""
	for _, id := range ids {
		if f.Cursor != "" && id <= f.Cursor {
			continue
		}
		m, err := s.Get(id)
		if err != nil {
			continue
		}
		if f.Status != "" && m.Status != f.Status {
			continue
		}
		if len(out) == limit {
			next = out[len(out)-1].ID
			break
		}
		out = append(out, m)
	}
	return ListResult{Memories: out, NextCursor: next}, nil
}

func (s *Store) readUnlocked(id string) (Memory, error) {
	path := s.layout.MetadataPath(id)
	// Atomic writes mean POSIX readers never see a partial file; retry a decode
	// error a few times as defense-in-depth on non-atomic-rename shares (Docker
	// Desktop bind mounts on macOS). A real read error returns at once.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Memory{}, ErrNotFound
			}
			return Memory{}, fmt.Errorf("memory: read metadata: %w", err)
		}
		var m Memory
		if err := json.Unmarshal(b, &m); err != nil {
			lastErr = fmt.Errorf("memory: decode metadata: %w", err)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return m, nil
	}
	return Memory{}, lastErr
}

func (s *Store) writeAtomic(meta Memory) error {
	dir := s.layout.MemoryDir(meta.ID)
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: encode metadata: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "memory-*.json.tmp")
	if err != nil {
		return fmt.Errorf("memory: temp metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: write metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: sync metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close metadata: %w", err)
	}
	if err := os.Rename(tmpName, s.layout.MetadataPath(meta.ID)); err != nil {
		return fmt.Errorf("memory: rename metadata: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
