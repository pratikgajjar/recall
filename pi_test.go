package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func piSession(id, cwd, ts string) string {
	return fmt.Sprintf(`{"type":"session","id":%q,"cwd":%q,"timestamp":%q}`, id, cwd, ts)
}

func piMsg(role, text, ts string) string {
	return fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":%q,"content":%q}}`, ts, role, text)
}

// pi sessions live under <root>/<sanitized-cwd>/<timestamp>_<uuid>.jsonl
func piPath(root, sessID string) string {
	return filepath.Join(root, "-work-web", "20260503T080000_"+sessID+".jsonl")
}

func TestPiScanFull(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-pi-1")
	writeLines(t, path,
		piSession("sess-pi-1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "refactor the parser", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "Sure.", "2026-05-03T08:00:06Z"),
	)
	ad := &PiAdapter{Root: root}
	if !ad.Available() {
		t.Fatal("adapter should be available")
	}
	sessions, msgs, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.SourceID != "sess-pi-1" {
		t.Errorf("sourceID = %q", s.SourceID)
	}
	if s.Project != "/work/web" {
		t.Errorf("project = %q", s.Project)
	}
	if s.Title != "refactor the parser" {
		t.Errorf("title = %q", s.Title)
	}
	if len(msgsFor(msgs, "sess-pi-1")) != 2 {
		t.Fatalf("want 2 messages")
	}
}

func TestPiAppendIncremental(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-pi-2")
	writeLines(t, path,
		piSession("sess-pi-2", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "first question", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "first answer", "2026-05-03T08:00:06Z"),
	)
	ad := &PiAdapter{Root: root}
	_, _, next, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	appendLines(t, path, piMsg("user", "second question", "2026-05-03T08:10:00Z"))
	sessions, msgs, next2, err := ad.Scan(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].Append {
		t.Fatalf("expected 1 Append session, got %+v", sessions)
	}
	mm := msgsFor(msgs, "sess-pi-2")
	if len(mm) != 1 || mm[0].Idx != 2 || mm[0].Text != "second question" {
		t.Fatalf("append messages = %+v", mm)
	}

	// A half-written trailing line (no newline yet) must never be indexed.
	// (The scan falls back to a full re-read here, but the partial content is
	// left for next time — it must not leak into any indexed message.)
	appendRaw(t, path, `{"type":"message","timestamp":"2026-05-03T08:20:00Z","message":{"role":"assistant","content":"partial`)
	_, m3, next3, err := ad.Scan(context.Background(), next2)
	if err != nil {
		t.Fatal(err)
	}
	for _, mm := range m3 {
		if strings.Contains(mm.Text, "partial") {
			t.Fatalf("half-written line must not be indexed, but got: %q", mm.Text)
		}
	}

	// Complete the line — now it is consumed exactly once, continuing the idx.
	appendRaw(t, path, ` answer"}}`+"\n")
	_, m4, _, err := ad.Scan(context.Background(), next3)
	if err != nil {
		t.Fatal(err)
	}
	mm4 := msgsFor(m4, "sess-pi-2")
	if len(mm4) != 1 || mm4[0].Idx != 3 || mm4[0].Text != "partial answer" {
		t.Fatalf("completed line should yield exactly 1 msg at idx 3, got %+v", mm4)
	}
}

func TestPiFetch(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-pi-3")
	writeLines(t, path,
		piSession("sess-pi-3", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "hello there", "2026-05-03T08:00:02Z"),
	)
	ad := &PiAdapter{Root: root}
	msgs, err := ad.Fetch(context.Background(), "sess-pi-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello there" {
		t.Fatalf("fetch = %+v", msgs)
	}
}
