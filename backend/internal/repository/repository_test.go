package repository

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComputeIDDeterministicAndDistinct(t *testing.T) {
	url := "https://github.com/marcellodesales/graphify-service.git"

	def1 := ComputeID(url, Selector{Type: SelectorDefault})
	def2 := ComputeID(url, Selector{Type: SelectorDefault})
	if def1 != def2 {
		t.Fatalf("default id not deterministic: %q != %q", def1, def2)
	}
	if len(def1) != 64 || !ValidID(def1) {
		t.Fatalf("id is not a valid sha256 hex: %q", def1)
	}

	ref := ComputeID(url, Selector{Type: SelectorRef, Value: "refs/heads/main"})
	sha := ComputeID(url, Selector{Type: SelectorSHA, Value: "51d5269"})
	if def1 == ref || def1 == sha || ref == sha {
		t.Fatalf("selectors must produce distinct ids: default=%s ref=%s sha=%s", def1, ref, sha)
	}

	other := ComputeID("https://github.com/other/repo.git", Selector{Type: SelectorDefault})
	if other == def1 {
		t.Fatalf("different urls must produce different ids")
	}
}

func TestStateTransitions(t *testing.T) {
	ok := [][2]Status{
		{StatusQueued, StatusCloning},
		{StatusCloning, StatusCloned},
		{StatusCloning, StatusFailed},
		{StatusCloned, StatusGraphifying},
		{StatusGraphifying, StatusReady},
		{StatusGraphifying, StatusFailed},
	}
	for _, tr := range ok {
		if !CanTransition(tr[0], tr[1]) {
			t.Errorf("expected %s -> %s allowed", tr[0], tr[1])
		}
	}
	bad := [][2]Status{
		{StatusReady, StatusGraphifying},
		{StatusQueued, StatusReady},
		{StatusReady, StatusFailed},
		{StatusFailed, StatusQueued},
		{StatusCloned, StatusReady},
	}
	for _, tr := range bad {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("expected %s -> %s forbidden", tr[0], tr[1])
		}
	}
	if !StatusReady.Terminal() || !StatusFailed.Terminal() {
		t.Errorf("ready/failed should be terminal")
	}
	if StatusQueued.Terminal() {
		t.Errorf("queued should not be terminal")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func sampleMeta(id string) Metadata {
	return Metadata{
		ID:       id,
		Selector: Selector{Type: SelectorDefault},
		Source: Source{
			NormalizedURL: "https://github.com/o/r.git",
			Host:          "github.com",
			OwnerPath:     "o",
			Repository:    "r",
			Transport:     "https",
		},
	}
}

func TestStoreCreateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	id := ComputeID("https://github.com/o/r.git", Selector{Type: SelectorDefault})

	first, created, err := s.Create(sampleMeta(id))
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	if first.Status != StatusQueued {
		t.Fatalf("expected queued, got %s", first.Status)
	}
	if first.Timestamps.CreatedAt.IsZero() {
		t.Fatalf("createdAt not set")
	}

	second, created2, err := s.Create(sampleMeta(id))
	if err != nil {
		t.Fatalf("second create err: %v", err)
	}
	if created2 {
		t.Fatalf("second create should report created=false")
	}
	if second.ID != first.ID || !second.Timestamps.CreatedAt.Equal(first.Timestamps.CreatedAt) {
		t.Fatalf("idempotent create must return the original record")
	}
}

func TestStoreCreateRejectsBadID(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Create(sampleMeta("not-a-sha")); err == nil {
		t.Fatalf("expected error for invalid id")
	}
}

func TestStoreGetAndAtomicFile(t *testing.T) {
	s := newTestStore(t)
	id := ComputeID("https://github.com/o/r.git", Selector{Type: SelectorRef, Value: "main"})
	if _, err := s.Get(id); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, _, err := s.Create(sampleMeta(id)); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id {
		t.Fatalf("get returned wrong id")
	}
	// metadata.json exists and no leftover temp files remain.
	entries, _ := os.ReadDir(s.Layout().JobDir(id))
	var tmp int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || (len(e.Name()) > 9 && e.Name()[:9] == "metadata-") {
			tmp++
		}
	}
	if tmp != 0 {
		t.Fatalf("leftover temp files: %d", tmp)
	}
}

func TestStoreListFilterAndPaging(t *testing.T) {
	s := newTestStore(t)
	// Create 3 default-branch repos on distinct URLs.
	urls := []string{
		"https://github.com/o/a.git",
		"https://github.com/o/b.git",
		"https://github.com/o/c.git",
	}
	for _, u := range urls {
		id := ComputeID(u, Selector{Type: SelectorDefault})
		m := sampleMeta(id)
		m.Source.NormalizedURL = u
		if _, _, err := s.Create(m); err != nil {
			t.Fatalf("create %s: %v", u, err)
		}
	}

	all, err := s.List(ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all.Repositories) != 3 {
		t.Fatalf("expected 3, got %d", len(all.Repositories))
	}

	// Paging: limit 2 then follow cursor.
	page1, _ := s.List(ListFilter{Limit: 2})
	if len(page1.Repositories) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1 unexpected: n=%d cursor=%q", len(page1.Repositories), page1.NextCursor)
	}
	page2, _ := s.List(ListFilter{Limit: 2, Cursor: page1.NextCursor})
	if len(page2.Repositories) != 1 || page2.NextCursor != "" {
		t.Fatalf("page2 unexpected: n=%d cursor=%q", len(page2.Repositories), page2.NextCursor)
	}

	// Filter by status: none are ready.
	ready, _ := s.List(ListFilter{Status: StatusReady})
	if len(ready.Repositories) != 0 {
		t.Fatalf("expected 0 ready, got %d", len(ready.Repositories))
	}
}
