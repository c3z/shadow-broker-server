package main

import (
	"io"
	"log"
	"path/filepath"
	"testing"
)

// newTestStore opens a local (embedded SQLite) store in a temp dir.
// No external services involved.
func newTestStore(t *testing.T, dbURL string) messageStore {
	t.Helper()
	logger = log.New(io.Discard, "", 0)
	st, err := newMessageStore(dbURL, "")
	if err != nil {
		t.Fatalf("newMessageStore(%q): %v", dbURL, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestLocalStore covers auto-schema + insert + poll + pull + maxID
// on a file: backend in a temporary directory.
func TestLocalStore(t *testing.T) {
	// The db file lives in a subdirectory that does not exist yet;
	// the store must create it (auto-schema included).
	dbURL := "file:" + filepath.Join(t.TempDir(), "data", "broker.db")
	st := newTestStore(t, dbURL)

	// maxID on an empty, freshly created table
	maxID, err := st.MaxID()
	if err != nil {
		t.Fatalf("MaxID: %v", err)
	}
	if maxID != 0 {
		t.Fatalf("MaxID on empty table = %d, want 0", maxID)
	}

	// insert
	id1, err := st.Insert("q1", "bm9uY2Ux", "Y2lwaGVyMQ==")
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id2, err := st.Insert("q2", "bm9uY2Uy", "Y2lwaGVyMg==")
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id3, err := st.Insert("q1", "bm9uY2Uz", "Y2lwaGVyMw==")
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !(id1 < id2 && id2 < id3) {
		t.Fatalf("ids not increasing: %d, %d, %d", id1, id2, id3)
	}

	// maxID
	maxID, err = st.MaxID()
	if err != nil {
		t.Fatalf("MaxID: %v", err)
	}
	if maxID != id3 {
		t.Fatalf("MaxID = %d, want %d", maxID, id3)
	}

	// poll (fetch new)
	all, err := st.FetchNew(0)
	if err != nil {
		t.Fatalf("FetchNew: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("FetchNew(0) returned %d messages, want 3", len(all))
	}
	for i, want := range []int64{id1, id2, id3} {
		if all[i].ID != want {
			t.Fatalf("FetchNew order: all[%d].ID = %d, want %d", i, all[i].ID, want)
		}
	}
	if all[0].Queue != "q1" || all[0].NonceB64 != "bm9uY2Ux" || all[0].CipherB64 != "Y2lwaGVyMQ==" {
		t.Fatalf("FetchNew row mismatch: %+v", all[0])
	}
	if all[0].CreatedAt == "" {
		t.Fatalf("CreatedAt empty: %+v", all[0])
	}
	fresh, err := st.FetchNew(id1)
	if err != nil {
		t.Fatalf("FetchNew: %v", err)
	}
	if len(fresh) != 2 || fresh[0].ID != id2 || fresh[1].ID != id3 {
		t.Fatalf("FetchNew(%d) = %+v, want ids [%d %d]", id1, fresh, id2, id3)
	}

	// pull
	q1, err := st.Pull("q1", 0, 100)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(q1) != 2 || q1[0].ID != id1 || q1[1].ID != id3 {
		t.Fatalf("Pull(q1) = %+v, want ids [%d %d]", q1, id1, id3)
	}
	since, err := st.Pull("q1", id1, 100)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(since) != 1 || since[0].ID != id3 {
		t.Fatalf("Pull(q1, since=%d) = %+v, want [%d]", id1, since, id3)
	}
	limited, err := st.Pull("q1", 0, 1)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != id1 {
		t.Fatalf("Pull(q1, limit=1) = %+v, want [%d]", limited, id1)
	}
}

// TestLocalStorePersistence: data survives closing and reopening the file.
func TestLocalStorePersistence(t *testing.T) {
	dbURL := "file:" + filepath.Join(t.TempDir(), "broker.db")

	st := newTestStore(t, dbURL)
	id, err := st.Insert("q", "bm9uY2U=", "Y2lwaGVy")
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	st.Close()

	st2 := newTestStore(t, dbURL)
	maxID, err := st2.MaxID()
	if err != nil {
		t.Fatalf("MaxID after reopen: %v", err)
	}
	if maxID != id {
		t.Fatalf("MaxID after reopen = %d, want %d", maxID, id)
	}
	msgs, err := st2.Pull("q", 0, 100)
	if err != nil {
		t.Fatalf("Pull after reopen: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("Pull after reopen = %+v, want [%d]", msgs, id)
	}
}

// TestNewMessageStoreUnsupportedScheme: unknown db_url schemes are rejected.
func TestNewMessageStoreUnsupportedScheme(t *testing.T) {
	logger = log.New(io.Discard, "", 0)
	if _, err := newMessageStore("postgres://example", ""); err == nil {
		t.Fatal("expected error for unsupported scheme, got nil")
	}
}
