package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// IngestBatch must honor context cancellation (Ctrl+C during `recall index`):
// a cancelled context aborts the transaction and persists nothing.
func TestIngestBatchHonorsContext(t *testing.T) {
	ix := newTestIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := []Session{{Source: "pi", SourceID: "s1", Project: "/w", Title: "t", MsgCount: 1}}
	m := []Message{{SourceID: "s1", Idx: 0, Role: "user", TS: 1, Text: "alpha bravo"}}
	if err := ix.IngestBatch(ctx, "pi", s, m); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if hits, _ := ix.Search("alpha", SearchOpts{}); len(hits) != 0 {
		t.Fatalf("cancelled ingest must persist nothing, got %d rows", len(hits))
	}
}

func seedSession(t *testing.T, ix *Index, pk, project, title, text string) {
	t.Helper()
	s := []Session{{
		Source:    "pi",
		SourceID:  pk,
		Project:   project,
		Title:     title,
		StartedAt: time.Now().UnixMilli(),
		EndedAt:   time.Now().UnixMilli(),
		MsgCount:  1,
	}}
	m := []Message{{SourceID: pk, Idx: 0, Role: "user", TS: time.Now().UnixMilli(), Text: text}}
	if err := ix.IngestBatch(context.Background(), "pi", s, m); err != nil {
		t.Fatalf("seed %s: %v", pk, err)
	}
}

// The writer DSN keeps WAL and grabs its lock up front; the reader DSN is
// query_only and never sets journal_mode. This is the contract that keeps
// searches from taking a write lock.
func TestIndexDSN(t *testing.T) {
	w := indexDSN("/tmp/x.sqlite", false)
	if !strings.Contains(w, "_txlock=immediate") || !strings.Contains(w, "journal_mode(WAL)") {
		t.Fatalf("writer DSN missing txlock/WAL: %s", w)
	}
	if strings.Contains(w, "query_only") {
		t.Fatalf("writer DSN must not be query_only: %s", w)
	}
	r := indexDSN("/tmp/x.sqlite", true)
	if !strings.Contains(r, "query_only(1)") {
		t.Fatalf("reader DSN missing query_only: %s", r)
	}
	if strings.Contains(r, "journal_mode") || strings.Contains(r, "_txlock") {
		t.Fatalf("reader DSN must not set journal_mode/txlock: %s", r)
	}
}

// Regression guard for the lock bug: BulkMode used to flip journal_mode to
// MEMORY, which takes an exclusive lock and blocks readers. It must stay WAL.
func TestBulkModeStaysWAL(t *testing.T) {
	ix := newTestIndex(t)

	assertWAL := func(when string) {
		var mode string
		if err := ix.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(mode, "wal") {
			t.Fatalf("journal_mode %s = %q, want wal", when, mode)
		}
	}

	assertWAL("at open")
	ix.BulkMode(true, true)
	assertWAL("during BulkMode")
	seedSession(t, ix, "pi:s1", "/work/web", "alpha", "alpha bravo charlie")
	assertWAL("after bulk write")
	ix.BulkMode(false, true)
	assertWAL("after BulkMode off")
}

// A read-only handle must reject writes — it can never bootstrap or take a
// write lock, so a search can't contend with the indexer.
func TestOpenIndexReadRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	w, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, w, "pi:s1", "/work/web", "alpha", "alpha bravo charlie")
	w.Close()

	r, err := openIndexRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if hits, err := r.Search("alpha", SearchOpts{}); err != nil || len(hits) != 1 {
		t.Fatalf("read handle search: hits=%d err=%v", len(hits), err)
	}
	if _, err := r.db.Exec(`INSERT INTO meta(k,v) VALUES('x','y')`); err == nil {
		t.Fatal("write through read-only handle succeeded; query_only not in effect")
	}
}

// The real bug: a separate reader process hit SQLITE_BUSY while the background
// indexer held the DB. Simulate it with independent connections (separate *sql.DB
// = separate locks, like separate processes): a writer ingesting in a loop while
// readers search continuously. With WAL + query_only readers, none should error.
func TestConcurrentReadsDuringIndexing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	w, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	seedSession(t, w, "pi:seed", "/work/web", "alpha", "alpha bravo charlie delta")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 64)

	// Writer: repeated bulk ingests, like the extension's background refresh.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			select {
			case <-stop:
				return
			default:
			}
			w.BulkMode(true, false)
			for j := 0; j < 25; j++ {
				pk := "pi:w" + string(rune('a'+i%26)) + string(rune('0'+j%10))
				seedSession(t, w, pk, "/work/web", "alpha", "alpha bravo charlie")
			}
			w.BulkMode(false, false)
		}
	}()

	// Readers: independent read-only connections searching in a tight loop.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rd, err := openIndexRead(path)
			if err != nil {
				errs <- err
				return
			}
			defer rd.Close()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := rd.Search("alpha", SearchOpts{Limit: 5}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	time.Sleep(750 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent read/write error (lock bug regressed?): %v", err)
	}
}
