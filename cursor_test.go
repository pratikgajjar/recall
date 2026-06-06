package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCursorScanFullAndFetch(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir, tComposer{
		id: "c1", name: "", createdAt: 1700000000000,
		bubbles: []tBubble{
			{id: "b1", typ: 1, text: "investigate the race condition"},
			{id: "b2", typ: 2, text: "Looking now."},
		},
	})
	addCursorWorkspace(t, userDir, "ws1", "/work/acme-api", "c1", "c2")

	ad := &CursorAdapter{UserDir: userDir}
	if !ad.Available() {
		t.Fatal("adapter should be available")
	}
	sessions, msgs, next, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := findSession(sessions, "c1")
	if !ok {
		t.Fatalf("c1 not found in %+v", sessions)
	}
	if s.Project != "/work/acme-api" {
		t.Errorf("project = %q (want folder from workspace map)", s.Project)
	}
	if s.Title != "investigate the race condition" {
		t.Errorf("title = %q", s.Title)
	}
	if len(msgsFor(msgs, "c1")) != 2 {
		t.Fatalf("want 2 messages for c1")
	}
	if next == "" {
		t.Error("expected checkpoint")
	}

	full, err := ad.Fetch(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 2 || full[0].Role != "user" || full[1].Role != "assistant" {
		t.Fatalf("fetch = %+v", full)
	}
}

func TestCursorResumeMidProvider(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir,
		tComposer{id: "c1", createdAt: 1, bubbles: []tBubble{{id: "b1", typ: 1, text: "alpha"}}},
		tComposer{id: "c2", createdAt: 2, bubbles: []tBubble{{id: "b2", typ: 1, text: "bravo"}}},
		tComposer{id: "c3", createdAt: 3, bubbles: []tBubble{{id: "b3", typ: 1, text: "charlie"}}},
	)
	addCursorWorkspace(t, userDir, "ws1", "/work/acme-api", "c1", "c2", "c3")
	ad := &CursorAdapter{UserDir: userDir, chunk: 1} // one composer per emit
	ctx := context.Background()

	// First run: index two composers, then "crash" before the third.
	var committed string
	done := 0
	err := ad.ScanStream(ctx, "", func(s []Session, m []Message, ckpt string) error {
		if len(s) == 0 {
			return nil
		}
		if done == 2 {
			return fmt.Errorf("boom")
		}
		committed = ckpt
		done++
		return nil
	})
	if err == nil || done != 2 || committed == "" {
		t.Fatalf("expected interruption after 2 composers: done=%d err=%v", done, err)
	}

	// Resume: only the un-reached composer should be processed.
	var resumed []string
	if err := ad.ScanStream(ctx, committed, func(s []Session, m []Message, ckpt string) error {
		for _, ss := range s {
			resumed = append(resumed, ss.SourceID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0] != "c3" {
		t.Fatalf("resume should process only the un-reached composer, got %v", resumed)
	}
}

func TestCursorIncrementalByRowID(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir, tComposer{
		id: "c1", createdAt: 1700000000000,
		bubbles: []tBubble{{id: "b1", typ: 1, text: "first chat"}},
	})
	addCursorWorkspace(t, userDir, "ws1", "/work/acme-api", "c1", "c2")
	ad := &CursorAdapter{UserDir: userDir}

	_, _, next, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	// no-op
	s0, _, next0, err := ad.Scan(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(s0) != 0 {
		t.Fatalf("no-op scan should return 0 sessions, got %d", len(s0))
	}

	// A new composer appears with a higher rowid.
	addCursorComposers(t, userDir, tComposer{
		id: "c2", createdAt: 1700000100000,
		bubbles: []tBubble{{id: "b9", typ: 1, text: "second chat"}},
	})
	sessions, _, _, err := ad.Scan(context.Background(), next0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("incremental scan should return only the new composer, got %d", len(sessions))
	}
	if sessions[0].SourceID != "c2" {
		t.Errorf("expected c2, got %q", sessions[0].SourceID)
	}
}

// TestSourceDBNotMutated guards the read-only contract: opening a WAL-mode
// source database (Cursor's state.vscdb is one) must not checkpoint or
// otherwise write it. A bare "path?mode=ro" DSN is silently opened read-write
// by modernc and checkpoints the WAL on close; sourceSQLiteDSN's file: URI
// form prevents that. Skips if the sqlite3 CLI isn't available to build the
// pending-WAL fixture.
func TestSourceDBNotMutated(t *testing.T) {
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	run := func(q string) {
		if out, e := exec.Command(sqlite3, path, q).CombinedOutput(); e != nil {
			t.Fatalf("sqlite3 setup: %v: %s", e, out)
		}
	}
	run("PRAGMA journal_mode=WAL; CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY, value BLOB); INSERT INTO cursorDiskKV VALUES('a','1');")
	// Second connection with autocheckpoint off leaves an uncheckpointed -wal.
	run("PRAGMA wal_autocheckpoint=0; INSERT INTO cursorDiskKV VALUES('b','2');")
	if _, e := os.Stat(path + "-wal"); e != nil {
		t.Skip("environment did not leave a pending -wal; can't exercise checkpoint path")
	}

	sum := func() string {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	before := sum()

	db, err := sql.Open("sqlite", sourceSQLiteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM cursorDiskKV`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	db.Close()

	if after := sum(); after != before {
		t.Fatalf("source db was MODIFIED by a read-only open\n  before=%s\n  after =%s", before, after)
	}
	if _, e := os.Stat(path + "-wal"); e != nil {
		t.Fatal("source -wal was checkpointed away by a read-only open")
	}
}
