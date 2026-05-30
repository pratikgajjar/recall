package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func claudeUser(text, ts, cwd string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":%q,"message":{"role":"user","content":%q}}`, ts, cwd, text)
}

func claudeAssistant(text, ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ts, text)
}

func TestClaudeScanFull(t *testing.T) {
	root := t.TempDir()
	// ~/.claude/projects/<sanitized>/<sessid>.jsonl
	path := filepath.Join(root, "-work-acme-api", "sess-claude-1.jsonl")
	writeLines(t, path,
		claudeUser("fix the auth bug", "2026-05-01T10:00:00Z", "/work/acme-api"),
		claudeAssistant("On it.", "2026-05-01T10:00:05Z"),
	)

	ad := &ClaudeAdapter{Root: root}
	if !ad.Available() {
		t.Fatal("adapter should be available")
	}
	sessions, msgs, next, err := ad.Scan("")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.SourceID != "sess-claude-1" {
		t.Errorf("sourceID = %q", s.SourceID)
	}
	if s.Project != "/work/acme-api" {
		t.Errorf("project = %q (want cwd from event)", s.Project)
	}
	if s.Title != "fix the auth bug" {
		t.Errorf("title = %q", s.Title)
	}
	if s.MsgCount != 2 {
		t.Errorf("msg_count = %d", s.MsgCount)
	}
	mm := msgsFor(msgs, "sess-claude-1")
	if len(mm) != 2 || mm[0].Role != "user" || mm[1].Role != "assistant" {
		t.Fatalf("messages = %+v", mm)
	}
	if mm[0].Idx != 0 || mm[1].Idx != 1 {
		t.Errorf("idx not sequential: %d, %d", mm[0].Idx, mm[1].Idx)
	}
	if next == "" {
		t.Error("expected a non-empty checkpoint")
	}
}

func TestClaudeAppendIncremental(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-work-acme-api", "sess-claude-2.jsonl")
	writeLines(t, path,
		claudeUser("start the refactor", "2026-05-01T10:00:00Z", "/work/acme-api"),
		claudeAssistant("Sure.", "2026-05-01T10:00:05Z"),
	)
	ad := &ClaudeAdapter{Root: root}

	_, _, next, err := ad.Scan("")
	if err != nil {
		t.Fatal(err)
	}

	// no-op: nothing changed
	s0, _, next0, err := ad.Scan(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(s0) != 0 {
		t.Fatalf("no-op scan should return 0 sessions, got %d", len(s0))
	}

	// append one new message
	appendLines(t, path, claudeUser("now run the tests", "2026-05-01T10:01:00Z", "/work/acme-api"))
	sessions, msgs, _, err := ad.Scan(next0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("append scan should return 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if !s.Append {
		t.Error("session should be marked Append")
	}
	if s.MsgCount != 1 {
		t.Errorf("append delta msg_count = %d, want 1", s.MsgCount)
	}
	mm := msgsFor(msgs, "sess-claude-2")
	if len(mm) != 1 {
		t.Fatalf("append should yield 1 new message, got %d", len(mm))
	}
	if mm[0].Idx != 2 {
		t.Errorf("append idx = %d, want 2 (continues from full read)", mm[0].Idx)
	}
	if mm[0].Text != "now run the tests" {
		t.Errorf("append text = %q", mm[0].Text)
	}
}

func TestClaudeFetchUntruncated(t *testing.T) {
	root := t.TempDir()
	long := ""
	for i := 0; i < excerptMax+500; i++ {
		long += "x"
	}
	path := filepath.Join(root, "-work-acme-api", "sess-claude-3.jsonl")
	writeLines(t, path,
		claudeUser(long, "2026-05-01T10:00:00Z", "/work/acme-api"),
	)
	ad := &ClaudeAdapter{Root: root}

	// Scan stores a truncated excerpt.
	_, msgs, _, err := ad.Scan("")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs[0].Text); got != excerptMax {
		t.Errorf("indexed excerpt len = %d, want %d", got, excerptMax)
	}
	// Fetch returns the full untruncated message.
	full, err := ad.Fetch("sess-claude-3")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(full[0].Text); got != excerptMax+500 {
		t.Errorf("fetched len = %d, want %d", got, excerptMax+500)
	}
}
