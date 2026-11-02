package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Regression tests for bugs found in review. Each failed before its fix.

// Forgetting a session must drop its msg_ranges too. A stale range outlives
// the session; once FTS reuses those rowids for another session, re-ingesting
// the first one (it came back at the source) deleted the second's rows.
func TestForgetSessionLeavesNoStaleRange(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	ingest := func(sid, text string) {
		t.Helper()
		if err := ix.IngestBatch(ctx, "pi",
			[]Session{{Source: "pi", SourceID: sid, Title: sid, MsgCount: 1}},
			[]Message{{SourceID: sid, Idx: 0, Role: "user", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	ingest("a", "alpha words")
	if err := ix.ForgetSession("pi:a"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM msg_ranges WHERE session_pk = 'pi:a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ForgetSession left %d msg_ranges rows", n)
	}
	ingest("c", "charlie words") // reuses a's rowid
	ingest("a", "alpha again")   // a comes back
	ingest("a", "alpha third")   // and is re-ingested
	if n := ftsRowCount(t, ix, "pi:c"); n != 1 {
		t.Fatalf("session c lost its rows: %d FTS rows, want 1", n)
	}
}

// An index written before the fix already holds orphaned ranges. A returning
// session must not inherit them.
func TestReturningSessionIgnoresOrphanRange(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	ingest := func(sid, text string) {
		t.Helper()
		if err := ix.IngestBatch(ctx, "pi",
			[]Session{{Source: "pi", SourceID: sid, Title: sid, MsgCount: 1}},
			[]Message{{SourceID: sid, Idx: 0, Role: "user", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	ingest("c", "charlie words")
	var rid int64
	if err := ix.db.QueryRow(`SELECT rowid FROM messages_fts WHERE session_pk = 'pi:c'`).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	// Simulate the old ForgetSession: a range for pi:a pointing at c's row.
	if _, err := ix.db.Exec(`INSERT INTO msg_ranges(session_pk, lo, hi) VALUES('pi:a', ?, ?)`, rid, rid); err != nil {
		t.Fatal(err)
	}
	ingest("a", "alpha again")
	ingest("a", "alpha third")
	if n := ftsRowCount(t, ix, "pi:c"); n != 1 {
		t.Fatalf("orphan range deleted session c's rows: %d left", n)
	}
}

// Related joined messages_fts on its UNINDEXED session_pk (a full-table scan,
// 9s on a real index) and scanned NULL text into a string for a session with
// no messages.
func TestRelatedOnSessionWithoutMessages(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "e", Title: "empty session title words"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Related("pi:e", 5); err != nil {
		t.Fatalf("Related on empty session: %v", err)
	}
}

func TestRelatedReadsOpeningMessages(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	seed := func(sid, title, text string) {
		t.Helper()
		if err := ix.IngestBatch(ctx, "pi",
			[]Session{{Source: "pi", SourceID: sid, Title: title, MsgCount: 1}},
			[]Message{{SourceID: sid, Idx: 0, Role: "user", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	seed("a", "kafka consumer lag", "kafka consumer rebalancing storms")
	seed("b", "unrelated", "kafka consumer rebalancing storms again")
	hits, err := ix.Related("pi:a", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SessionID != "pi:b" {
		t.Fatalf("related = %+v, want pi:b", hits)
	}
}

// Search hits carry a message index that `show` resolves by re-reading the
// source. Cursor's Fetch appended header-less bubbles in map order, so the
// index pointed at a different message on every call.
func TestCursorFetchOrderMatchesIndex(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir, tComposer{id: "c1", createdAt: 1, bubbles: []tBubble{
		{id: "b1", typ: 1, text: "first"},
	}})
	for _, id := range []string{"x1", "x2", "x3", "x4", "x5", "x6"} {
		addCursorBubble(t, userDir, "c1", tBubble{id: id, typ: 2, text: "extra " + id})
	}
	addCursorWorkspace(t, userDir, "ws1", "/w", "c1")
	ad := &CursorAdapter{UserDir: userDir}
	_, msgs, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := ad.Fetch(context.Background(), "c1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(msgs) {
			t.Fatalf("fetch has %d msgs, index %d", len(got), len(msgs))
		}
		for j := range msgs {
			if got[j].Text != msgs[j].Text {
				t.Fatalf("try %d: msg %d fetch=%q index=%q", i, j, got[j].Text, msgs[j].Text)
			}
		}
	}
}

// Cursor with no workspaceStorage yet must still index its chats.
func TestCursorWithoutWorkspaceStorage(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir, tComposer{id: "c1", createdAt: 1, bubbles: []tBubble{
		{id: "b1", typ: 1, text: "hello"},
	}})
	ss, _, _, err := (&CursorAdapter{UserDir: userDir}).Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions = %d, want 1", len(ss))
	}
}

// A live session grows by lines that are not messages (metadata, a
// half-written line). That used to fall through to a full re-read and a full
// delete+reinsert of the session on every index run.
func TestNonMessageGrowthIsNotReread(t *testing.T) {
	ctx := context.Background()

	piRoot := t.TempDir()
	pp := piPath(piRoot, "sp")
	writeLines(t, pp,
		piSession("sp", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "hello", "2026-05-03T08:00:02Z"))
	clRoot := t.TempDir()
	cp := filepath.Join(clRoot, "-work", "sc.jsonl")
	writeLines(t, cp,
		claudeUser("start", "2026-05-01T10:00:00Z", "/work"),
		claudeAssistant("ok", "2026-05-01T10:00:05Z"))

	for _, c := range []struct {
		name string
		ad   Adapter
		idx  int // next message index: pi's header line is not a message
		grow func()
		more func()
	}{
		{"pi", &PiAdapter{Root: piRoot}, 1,
			func() { appendRaw(t, pp, `{"type":"model_change","provider":"x"}`+"\n"+`{"type":"mess`) },
			func() {
				appendRaw(t, pp, `age","timestamp":"2026-05-03T08:00:09Z","message":{"role":"assistant","content":"done"}}`+"\n")
			}},
		{"claude", &ClaudeAdapter{Root: clRoot}, 2,
			func() { appendLines(t, cp, `{"type":"file-history-snapshot","snapshot":{}}`) },
			func() { appendLines(t, cp, claudeAssistant("done", "2026-05-01T10:00:09Z")) }},
	} {
		_, _, ck, err := c.ad.Scan(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		c.grow()
		ss, _, ck, err := c.ad.Scan(ctx, ck)
		if err != nil {
			t.Fatal(err)
		}
		if len(ss) != 0 {
			t.Errorf("%s: non-message growth re-emitted the session (append=%v)", c.name, ss[0].Append)
		}
		// The next real message still arrives as an append at the right index.
		c.more()
		ss, ms, _, err := c.ad.Scan(ctx, ck)
		if err != nil {
			t.Fatal(err)
		}
		if len(ss) != 1 || !ss[0].Append || len(ms) != 1 || ms[0].Idx != c.idx || ms[0].Text != "done" {
			t.Errorf("%s: follow-up append = sessions %+v msgs %+v", c.name, ss, ms)
		}
	}
}

// A session first seen with no messages yet (pi writes its header line before
// the first prompt) was recorded with its id, so later growth was appended to
// a session row that never existed: the messages landed in FTS, unreachable.
func TestSessionFirstSeenEmptyIsCreatedOnGrowth(t *testing.T) {
	ctx := context.Background()
	check := func(name string, ad Adapter, src, sid string, grow func()) {
		t.Helper()
		ix := newTestIndex(t)
		ck := ""
		index := func() {
			ss, ms, next, err := ad.Scan(ctx, ck)
			if err != nil {
				t.Fatal(err)
			}
			if err := ix.IngestBatch(ctx, src, ss, ms); err != nil {
				t.Fatal(err)
			}
			ck = next
		}
		index()
		grow()
		index()
		if _, err := ix.LookupSession(src + ":" + sid); err != nil {
			t.Errorf("%s: session missing after growth (orphan FTS rows: %d): %v",
				name, ftsRowCount(t, ix, src+":"+sid), err)
		}
	}

	piRoot := t.TempDir()
	pp := piPath(piRoot, "sp")
	writeLines(t, pp, piSession("sp", "/work/web", "2026-05-03T08:00:00Z"))
	check("pi", &PiAdapter{Root: piRoot}, "pi", "sp",
		func() { appendLines(t, pp, piMsg("user", "hello there", "2026-05-03T08:00:02Z")) })

	plRoot := t.TempDir()
	lp := piPath(plRoot, "sl")
	writeLines(t, lp, piSession("sl", "/work/web", "2026-05-03T08:00:00Z"))
	check("pi.lua", luaAdapterFor(t, "plugins/pi.lua", plRoot), "pi", "sl",
		func() { appendLines(t, lp, piMsg("user", "hello there", "2026-05-03T08:00:02Z")) })

	clRoot := t.TempDir()
	cp := filepath.Join(clRoot, "-work", "sc.jsonl")
	writeLines(t, cp, `{"type":"file-history-snapshot","snapshot":{}}`)
	check("claude", &ClaudeAdapter{Root: clRoot}, "claude", "sc",
		func() { appendLines(t, cp, claudeUser("hello there", "2026-05-01T10:00:00Z", "/work")) })
}

// Titles, outline lines and tool summaries were cut by byte count, which can
// split a multi-byte character.
func TestClipsNeverSplitARune(t *testing.T) {
	title := titleFromPrompt(strings.Repeat("é", 60)) // 120 bytes, cut at 80
	if !utf8.ValidString(title) {
		t.Errorf("title is not valid UTF-8: %q", title)
	}
	if got := clipUTF8("aé", 2); got != "a" {
		t.Errorf("clipUTF8 = %q, want %q", got, "a")
	}
	if got := clipUTF8("abc", 5); got != "abc" {
		t.Errorf("clipUTF8 = %q", got)
	}
}

// Overlapping `recall index` runs (one per pi session, after every turn) each
// appended the same new messages. The writer lock must make the second wait for
// the first, and a cancelled wait must give up.
func TestIndexWriterLockSerialises(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	release, err := lockIndexWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := lockIndexWriter(context.Background(), path)
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second writer did not wait (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second writer never got the lock after release")
	}

	release, err = lockIndexWriter(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := lockIndexWriter(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait: err=%v, want deadline exceeded", err)
	}
}

// A session always matches all of its own top terms, so Search's any-term
// fallback never ran for Related, and a session whose terms co-occur nowhere
// else returned nothing. All-terms hits must still come first.
func TestRelatedFillsFromAnyTerm(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	seed := func(sid, title, text string) {
		t.Helper()
		if err := ix.IngestBatch(ctx, "pi",
			[]Session{{Source: "pi", SourceID: sid, Title: title, MsgCount: 1}},
			[]Message{{SourceID: sid, Idx: 0, Role: "user", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	seed("src", "kafka rebalance", "kafka rebalance storms during deploys")
	seed("all", "x", "kafka rebalance storms during deploys again")
	seed("some", "y", "kafka partitions only")
	hits, err := ix.Related("pi:src", 5)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.SessionID)
	}
	if len(ids) != 2 || ids[0] != "pi:all" || ids[1] != "pi:some" {
		t.Fatalf("related = %v, want [pi:all pi:some]", ids)
	}
}

// --context on a title match printed only the snippet, so the best hit came
// with no context; and every expanded hit restated the session header that
// the hit line above it had just printed.
func TestContextExpandsTitleHitsWithoutRepeatingHeader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".pi", "agent", "sessions")
	writeLines(t, piPath(root, "s1"),
		piSession("s1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "zebra migration plan", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "first we copy the table", "2026-05-03T08:00:06Z"),
		piMsg("user", "then what", "2026-05-03T08:00:09Z"),
	)
	ix := newTestIndex(t)
	ss, ms, _, err := (&PiAdapter{Root: root}).Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi", ss, ms); err != nil {
		t.Fatal(err)
	}
	hits := []Hit{{SessionID: "pi:s1", Source: "pi", Title: "zebra migration plan", MsgIdx: -1, Role: "title"}}
	var b strings.Builder
	if err := printHitsWithContext(context.Background(), &b, ix, hits, 1); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"(title match)", "## 0 user", "zebra migration plan", "## 1 assistant", "## 2 user"} {
		if !strings.Contains(out, want) {
			t.Errorf("title hit context lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "source=pi") {
		t.Errorf("expanded hit repeats the session header:\n%s", out)
	}
}

// An agent turns "last month" into --since 30d, or adds --repo . from the
// wrong directory. A bare "no matches" then reads as "we never did that". The
// output must say what the filter removed, and give the command without it.
func TestEmptyResultNamesHitsOutsideFilter(t *testing.T) {
	ix := newTestIndex(t)
	old := time.Now().AddDate(0, 0, -45).UnixMilli()
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "checkout", Project: "/work/shop", StartedAt: old, MsgCount: 1}},
		[]Message{{SourceID: "s1", Idx: 0, Role: "user", Text: "flaky checkout test"}}); err != nil {
		t.Fatal(err)
	}
	day := time.UnixMilli(old).Format("2006-01-02")
	for _, c := range []struct {
		name string
		cf   commonFlags
		want []string
		gone []string
	}{
		{"since", commonFlags{since: "30d"},
			[]string{"1 hits are outside the time range", day, `recall find "flaky checkout"`}, []string{"--since"}},
		{"repo", commonFlags{repo: "/work/other", tags: stringSlice{"source:pi"}},
			[]string{"outside the project /work/other", "in /work/shop", "--tag source:pi"}, []string{"--repo"}},
		{"both", commonFlags{repo: "/work/other", since: "30d"},
			[]string{"outside the time range and project /work/other"}, []string{"--repo", "--since"}},
	} {
		opts, err := c.cf.toSearchOpts()
		if err != nil {
			t.Fatal(err)
		}
		if hits, _ := ix.Search("flaky checkout", opts); len(hits) != 0 {
			t.Fatalf("%s: setup: the filter should remove the hit, got %d", c.name, len(hits))
		}
		var b strings.Builder
		printOutsideFilter(&b, ix, "flaky checkout", opts, c.cf)
		out := b.String()
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: hint lost %q:\n%s", c.name, w, out)
			}
		}
		for _, g := range c.gone {
			if strings.Contains(out, g) {
				t.Errorf("%s: suggested command keeps %s:\n%s", c.name, g, out)
			}
		}
	}
	// No filter, or nothing outside it: no hint.
	var b strings.Builder
	printOutsideFilter(&b, ix, "flaky checkout", SearchOpts{}, commonFlags{})
	sinceCF := commonFlags{since: "30d"}
	opts, _ := sinceCF.toSearchOpts()
	printOutsideFilter(&b, ix, "zebra", opts, sinceCF)
	if b.Len() != 0 {
		t.Errorf("unexpected hint:\n%s", b.String())
	}
}
