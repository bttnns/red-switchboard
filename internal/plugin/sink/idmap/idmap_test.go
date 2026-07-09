package idmap

import (
	"path/filepath"
	"testing"
)

func TestIDDeterministicAndPositive(t *testing.T) {
	m, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id1, err := m.ID("guid-abc")
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("expected positive id, got %d", id1)
	}

	id2, _ := m.ID("guid-abc")
	if id1 != id2 {
		t.Fatalf("expected stable id, got %d then %d", id1, id2)
	}

	other, _ := m.ID("guid-xyz")
	if other == id1 {
		t.Fatalf("expected distinct ids for distinct guids")
	}
}

func TestHashIDStableAcrossInstances(t *testing.T) {
	// Without any cache, the same GUID must hash to the same id.
	a, _ := New("")
	b, _ := New("")
	ida, _ := a.ID("vehicle-guid-1")
	idb, _ := b.ID("vehicle-guid-1")
	if ida != idb {
		t.Fatalf("hash not stable: %d != %d", ida, idb)
	}
}

func TestReverseLookup(t *testing.T) {
	m, _ := New("")
	id, _ := m.ID("guid-rev")
	guid, ok := m.GUID(id)
	if !ok || guid != "guid-rev" {
		t.Fatalf("reverse lookup failed: %q ok=%v", guid, ok)
	}
	if _, ok := m.GUID(999999999); ok {
		t.Fatalf("expected miss for unknown id")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idmap.json")

	m1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want, _ := m1.ID("persist-guid")

	m2, err := New(path)
	if err != nil {
		t.Fatalf("reload New: %v", err)
	}
	got, _ := m2.ID("persist-guid")
	if got != want {
		t.Fatalf("persisted id changed: %d != %d", got, want)
	}
	if guid, ok := m2.GUID(want); !ok || guid != "persist-guid" {
		t.Fatalf("reverse lookup after reload failed: %q ok=%v", guid, ok)
	}
}
