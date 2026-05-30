package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func codexMeta(id, cwd, ts string) string {
	return fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"cwd":%q,"timestamp":%q}}`, ts, id, cwd, ts)
}

func codexMsg(role, text, ts string) string {
	return fmt.Sprintf(`{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":%q,"content":[{"type":"text","text":%q}]}}`, ts, role, text)
}

func codexCall(name, args, ts string) string {
	return fmt.Sprintf(`{"type":"response_item","timestamp":%q,"payload":{"type":"function_call","name":%q,"arguments":%q}}`, ts, name, args)
}

// codex sessions live under <root>/YYYY/MM/DD/rollout-*.jsonl
func codexPath(root, sessID string) string {
	return filepath.Join(root, "2026", "05", "02", "rollout-2026-05-02-"+sessID+".jsonl")
}

func TestCodexScanFull(t *testing.T) {
	root := t.TempDir()
	path := codexPath(root, "sess-codex-1")
	writeLines(t, path,
		codexMeta("sess-codex-1", "/work/api", "2026-05-02T09:00:00Z"),
		codexMsg("user", "add retry logic", "2026-05-02T09:00:03Z"),
		codexMsg("assistant", "Added.", "2026-05-02T09:00:08Z"),
		codexCall("shell", `{"cmd":"go test"}`, "2026-05-02T09:00:09Z"),
	)
	ad := &CodexAdapter{Root: root}
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
	if s.SourceID != "sess-codex-1" {
		t.Errorf("sourceID = %q", s.SourceID)
	}
	if s.Project != "/work/api" {
		t.Errorf("project = %q", s.Project)
	}
	if s.Title != "add retry logic" {
		t.Errorf("title = %q", s.Title)
	}
	mm := msgsFor(msgs, "sess-codex-1")
	if len(mm) != 3 {
		t.Fatalf("want 3 messages (user, assistant, tool), got %d", len(mm))
	}
	if mm[2].Role != "tool" {
		t.Errorf("function_call should map to tool role, got %q", mm[2].Role)
	}
	if next == "" {
		t.Error("expected checkpoint")
	}
}

func TestCodexAppendIncremental(t *testing.T) {
	root := t.TempDir()
	path := codexPath(root, "sess-codex-2")
	writeLines(t, path,
		codexMeta("sess-codex-2", "/work/api", "2026-05-02T09:00:00Z"),
		codexMsg("user", "kick things off", "2026-05-02T09:00:03Z"),
	)
	ad := &CodexAdapter{Root: root}
	_, _, next, err := ad.Scan("")
	if err != nil {
		t.Fatal(err)
	}

	appendLines(t, path, codexMsg("assistant", "done deal", "2026-05-02T09:05:00Z"))
	sessions, msgs, _, err := ad.Scan(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].Append {
		t.Fatalf("expected 1 Append session, got %+v", sessions)
	}
	// SID comes from the checkpoint, not a re-read of the (skipped) meta line.
	if sessions[0].SourceID != "sess-codex-2" {
		t.Errorf("append sourceID = %q", sessions[0].SourceID)
	}
	mm := msgsFor(msgs, "sess-codex-2")
	if len(mm) != 1 || mm[0].Idx != 1 || mm[0].Text != "done deal" {
		t.Fatalf("append messages = %+v", mm)
	}
}
