package main

import (
	"context"
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
