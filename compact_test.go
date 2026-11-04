package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func pragmaInt(t *testing.T, ix *Index, q string) int64 {
	t.Helper()
	var n int64
	if err := ix.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func staleRows(t *testing.T, ix *Index) int64 {
	t.Helper()
	n, err := strconv.ParseInt(ix.GetMeta(metaStaleRows), 10, 64)
	if err != nil {
		t.Fatalf("stale counter %q: %v", ix.GetMeta(metaStaleRows), err)
	}
	return n
}

// seedMany ingests n sessions whose single message is long enough to need
// several FTS rows, so deletes leave a measurable amount of stale data.
func seedMany(t *testing.T, ix *Index, n int, tag string) {
	t.Helper()
	var ss []Session
	var ms []Message
	for i := 0; i < n; i++ {
		sid := fmt.Sprintf("s%d", i)
		var b strings.Builder
		for w := 0; w < 400; w++ {
			fmt.Fprintf(&b, "%s%dw%d ", tag, i, w)
		}
		ss = append(ss, Session{Source: "pi", SourceID: sid, Title: sid, MsgCount: 1})
		ms = append(ms, Message{SourceID: sid, Idx: 0, Role: "user", Text: b.String()})
	}
	if err := ix.IngestBatch(context.Background(), "pi", ss, ms); err != nil {
		t.Fatal(err)
	}
}

func TestNewIndexStartsClean(t *testing.T) {
	ix := newTestIndex(t)
	if got := staleRows(t, ix); got != 0 {
		t.Fatalf("stale = %d on a new index", got)
	}
	if need, why := ix.NeedsCompaction(); need {
		t.Fatalf("new index wants compaction: %s", why)
	}
	if mode := pragmaInt(t, ix, `PRAGMA auto_vacuum`); mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental)", mode)
	}
}

// Every deleted FTS row is stale data until a merge removes it. The counter
// must see deletes from re-ingest and from ForgetSession.
func TestStaleRowsCountDeletes(t *testing.T) {
	ix := newTestIndex(t)
	seedMany(t, ix, 20, "a")
	if got := staleRows(t, ix); got != 0 {
		t.Fatalf("stale after first ingest = %d, want 0", got)
	}
	live := pragmaInt(t, ix, `SELECT COUNT(*) FROM messages_fts`)
	perSession := live / 20
	seedMany(t, ix, 20, "b") // replaces every session
	if got := staleRows(t, ix); got != live {
		t.Fatalf("stale after re-ingest = %d, want %d", got, live)
	}
	if need, _ := ix.NeedsCompaction(); !need {
		t.Fatal("index with 100% stale rows does not want compaction")
	}
	if err := ix.ForgetSession("pi:s0"); err != nil {
		t.Fatal(err)
	}
	if got := staleRows(t, ix); got != live+perSession {
		t.Fatalf("stale after forget = %d, want %d", got, live+perSession)
	}
}

// An index from before the counter existed has an unknown history.
func TestLegacyIndexCompactsOnce(t *testing.T) {
	ix := newTestIndex(t)
	seedMany(t, ix, 5, "a")
	if _, err := ix.db.Exec(`DELETE FROM meta WHERE k = ?`, metaStaleRows); err != nil {
		t.Fatal(err)
	}
	if need, _ := ix.NeedsCompaction(); !need {
		t.Fatal("legacy index must compact once")
	}
	if err := ix.Compact(); err != nil {
		t.Fatal(err)
	}
	if need, why := ix.NeedsCompaction(); need {
		t.Fatalf("still wants compaction after Compact: %s", why)
	}
}

// Compact must remove the stale data, give the space back to the disk, and
// leave search working.
func TestCompactRemovesStaleData(t *testing.T) {
	ix := newTestIndex(t)
	seedMany(t, ix, 40, "a")
	// Full rebuilds that never reached their final merge: automerge off, every
	// row deleted and inserted again.
	ix.BulkMode(true, true)
	for i := 0; i < 4; i++ {
		seedMany(t, ix, 40, fmt.Sprintf("r%d", i))
	}
	ix.BulkMode(false, false)
	before := pragmaInt(t, ix, `SELECT SUM(LENGTH(block)) FROM messages_fts_data`)
	if err := ix.Compact(); err != nil {
		t.Fatal(err)
	}
	after := pragmaInt(t, ix, `SELECT SUM(LENGTH(block)) FROM messages_fts_data`)
	clean := newTestIndex(t)
	seedMany(t, clean, 40, "r3")
	if err := clean.Compact(); err != nil {
		t.Fatal(err)
	}
	want := pragmaInt(t, clean, `SELECT SUM(LENGTH(block)) FROM messages_fts_data`)
	if after > want+want/5 || after >= before { // larger rowids cost a few bytes
		t.Errorf("FTS data %d -> %d bytes; a clean index of the same rows is %d", before, after, want)
	}
	pages := pragmaInt(t, ix, `PRAGMA page_count`)
	free := pragmaInt(t, ix, `PRAGMA freelist_count`)
	if free*4 >= pages {
		t.Errorf("free pages %d of %d were not returned to the disk", free, pages)
	}
	if got := staleRows(t, ix); got != 0 {
		t.Errorf("stale = %d after Compact", got)
	}
	hits, err := ix.Search("r37w10", SearchOpts{})
	if err != nil || len(hits) != 1 || hits[0].SessionID != "pi:s7" {
		t.Fatalf("search after compact = %+v, %v", hits, err)
	}
	if hits, _ := ix.Search("a7w10", SearchOpts{}); len(hits) != 0 {
		t.Fatalf("deleted text still found: %+v", hits)
	}
}

// A full run turns automerge off for speed. It must come back on, or every
// later run accumulates segments and delete markers with nothing merging them.
func TestBulkModeRestoresAutomerge(t *testing.T) {
	ix := newTestIndex(t)
	seedMany(t, ix, 2, "a")
	ix.BulkMode(true, true)
	ix.BulkMode(false, false) // the interrupted-run exit path
	for _, tbl := range []string{"messages_fts", "sessions_fts"} {
		var v string
		err := ix.db.QueryRow(`SELECT v FROM ` + tbl + `_config WHERE k = 'automerge'`).Scan(&v)
		if err != nil || v != strconv.Itoa(ftsAutomerge) {
			t.Errorf("%s automerge = %q (%v), want %d", tbl, v, err, ftsAutomerge)
		}
	}
}

// An index created before auto_vacuum was set converts once, via VACUUM.
func TestCompactConvertsLegacyAutoVacuum(t *testing.T) {
	ix := newTestIndex(t)
	for _, q := range []string{`PRAGMA auto_vacuum=NONE`, `VACUUM`} {
		if _, err := ix.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if mode := pragmaInt(t, ix, `PRAGMA auto_vacuum`); mode != 0 {
		t.Fatalf("setup: auto_vacuum = %d", mode)
	}
	seedMany(t, ix, 40, "a")
	seedMany(t, ix, 40, "b")
	if err := ix.Compact(); err != nil {
		t.Fatal(err)
	}
	if mode := pragmaInt(t, ix, `PRAGMA auto_vacuum`); mode != 2 {
		t.Errorf("auto_vacuum = %d after Compact, want 2", mode)
	}
	if free := pragmaInt(t, ix, `PRAGMA freelist_count`); free != 0 {
		t.Errorf("freelist = %d after VACUUM", free)
	}
}

// ingestUncounted re-ingests like a writer that does not count its deletes
// (an older recall): the deletes happen, the counter does not move.
func ingestUncounted(t *testing.T, ix *Index, n int, tag string) {
	t.Helper()
	before := ix.GetMeta(metaStaleRows)
	seedMany(t, ix, n, tag)
	if err := ix.SetMeta(metaStaleRows, before); err != nil {
		t.Fatal(err)
	}
}

// Deletes by another writer never reach the counter. A real index grew from
// 1.5 GB to 3.2 GB under an older binary and nothing compacted it.
func TestForeignDeletesAreFound(t *testing.T) {
	ix := newTestIndex(t)
	seedMany(t, ix, 20, "a")
	if err := ix.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := ix.RecordSeen(); err != nil {
		t.Fatal(err)
	}
	live := pragmaInt(t, ix, `SELECT COUNT(*) FROM messages_fts`)

	// No other writer: no change, and no count of every row.
	if foreign, err := ix.ReconcileForeignWrites(); err != nil || foreign {
		t.Fatalf("no other writer, got foreign=%v err=%v", foreign, err)
	}

	ingestUncounted(t, ix, 20, "b") // replaces every row
	if got := staleRows(t, ix); got != 0 {
		t.Fatalf("setup: counter moved to %d", got)
	}
	foreign, err := ix.ReconcileForeignWrites()
	if err != nil || !foreign {
		t.Fatalf("other writer not seen: foreign=%v err=%v", foreign, err)
	}
	if got := staleRows(t, ix); got != live {
		t.Fatalf("stale = %d after reconcile, want %d", got, live)
	}
	if need, _ := ix.NeedsCompaction(); !need {
		t.Fatal("foreign deletes did not trigger compaction")
	}

	// After Compact, the old gaps are the new base: no debt remains.
	if err := ix.Compact(); err != nil {
		t.Fatal(err)
	}
	ix.db.Exec(`DELETE FROM meta WHERE k = ?`, metaSeenMaxRowid) // force the full check
	if _, err := ix.ReconcileForeignWrites(); err != nil {
		t.Fatal(err)
	}
	if got := staleRows(t, ix); got != 0 {
		t.Fatalf("stale = %d after compact + reconcile, want 0", got)
	}
}

// The counter already holds this build's own deletes. Reconcile must not add
// them a second time.
func TestReconcileDoesNotDoubleCount(t *testing.T) {
	ix := newTestIndex(t)
	seedMany(t, ix, 20, "a")
	live := pragmaInt(t, ix, `SELECT COUNT(*) FROM messages_fts`)
	seedMany(t, ix, 20, "b") // counted
	ix.db.Exec(`DELETE FROM meta WHERE k = ?`, metaSeenMaxRowid)
	if _, err := ix.ReconcileForeignWrites(); err != nil {
		t.Fatal(err)
	}
	if got := staleRows(t, ix); got != live {
		t.Fatalf("stale = %d, want %d", got, live)
	}
}
