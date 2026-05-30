package main

import (
	"path/filepath"
	"testing"
)

func ftsRowCount(t *testing.T, ix *Index, pk string) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE session_pk = ?`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func dupIdxRows(t *testing.T, ix *Index) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT session_pk, idx, COUNT(*) c FROM messages_fts GROUP BY session_pk, idx HAVING c > 1)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sessionMsgCount(t *testing.T, ix *Index, pk string) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT msg_count FROM sessions WHERE id = ?`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIngestAppendInvariants(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-e2e-1")
	writeLines(t, path,
		piSession("sess-e2e-1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "alpha bravo charlie", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "delta echo", "2026-05-03T08:00:06Z"),
	)
	ad := &PiAdapter{Root: root}
	ix := newTestIndex(t)
	pk := "pi:sess-e2e-1"

	// Full ingest.
	s, m, next, err := ad.Scan("")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch("pi", s, m); err != nil {
		t.Fatal(err)
	}
	if got := sessionMsgCount(t, ix, pk); got != 2 {
		t.Fatalf("msg_count after full = %d, want 2", got)
	}
	if got := ftsRowCount(t, ix, pk); got != 2 {
		t.Fatalf("fts rows after full = %d, want 2", got)
	}

	// Append one message and ingest via the append path.
	appendLines(t, path, piMsg("user", "foxtrot golf", "2026-05-03T08:10:00Z"))
	s2, m2, _, err := ad.Scan(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2) != 1 || !s2[0].Append {
		t.Fatalf("expected an Append session, got %+v", s2)
	}
	if err := ix.IngestBatch("pi", s2, m2); err != nil {
		t.Fatal(err)
	}

	// Invariant: msg_count tracks the appended delta, FTS has no duplicates.
	if got := sessionMsgCount(t, ix, pk); got != 3 {
		t.Errorf("msg_count after append = %d, want 3", got)
	}
	if got := ftsRowCount(t, ix, pk); got != 3 {
		t.Errorf("fts rows after append = %d, want 3", got)
	}
	if got := dupIdxRows(t, ix); got != 0 {
		t.Errorf("found %d duplicated (session_pk, idx) rows after append", got)
	}

	// The appended content is searchable.
	hits, err := ix.Search("foxtrot", SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].SessionID != pk {
		t.Fatalf("appended message not found via search: %+v", hits)
	}
}

func TestFullReingestReplacesNotDuplicates(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-e2e-2")
	writeLines(t, path,
		piSession("sess-e2e-2", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "hotel india", "2026-05-03T08:00:02Z"),
	)
	ad := &PiAdapter{Root: root}
	ix := newTestIndex(t)
	pk := "pi:sess-e2e-2"

	// Ingest the same full session twice (e.g. a forced --full re-read).
	for i := 0; i < 2; i++ {
		s, m, _, err := ad.Scan("") // prev="" => always full
		if err != nil {
			t.Fatal(err)
		}
		if err := ix.IngestBatch("pi", s, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := ftsRowCount(t, ix, pk); got != 1 {
		t.Errorf("fts rows after double full ingest = %d, want 1 (delete+reinsert)", got)
	}
	if got := dupIdxRows(t, ix); got != 0 {
		t.Errorf("found %d duplicate rows after re-ingest", got)
	}
}

func TestSearchTitleAndProjectFilter(t *testing.T) {
	root := t.TempDir()
	writeLines(t, piPath(root, "sess-f1"),
		piSession("sess-f1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "kilo lima mike", "2026-05-03T08:00:02Z"),
	)
	// a second session in a different project
	writeLines(t, filepath.Join(root, "-work-api", "20260504T080000_sess-f2.jsonl"),
		piSession("sess-f2", "/work/api", "2026-05-04T08:00:00Z"),
		piMsg("user", "kilo november", "2026-05-04T08:00:02Z"),
	)
	ad := &PiAdapter{Root: root}
	ix := newTestIndex(t)
	s, m, _, err := ad.Scan("")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch("pi", s, m); err != nil {
		t.Fatal(err)
	}

	all, err := ix.Search("kilo", SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 hits for 'kilo', got %d", len(all))
	}
	scoped, err := ix.Search("kilo", SearchOpts{Project: "/work/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].SessionID != "pi:sess-f2" {
		t.Fatalf("project filter failed: %+v", scoped)
	}
}
