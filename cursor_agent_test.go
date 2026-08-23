package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorAgentUsesSidebarTitleFromChatsMeta(t *testing.T) {
	root := t.TempDir()
	chats := t.TempDir()
	sid := "b6f31b34-0c63-44ef-a496-81218369f3e3"
	writeLines(t, filepath.Join(root, "proj", "agent-transcripts", sid, sid+".jsonl"),
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query> long skill dump that must not become the title </user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
	)
	metaDir := filepath.Join(chats, "workspacehash", sid)
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "meta.json"), []byte(`{"title":"WeChat File Sorter"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ad := &CursorAgentAdapter{Root: root, ChatsRoot: chats}
	sessions, _, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].Title != "WeChat File Sorter" {
		t.Fatalf("title = %q, want sidebar title from meta.json", sessions[0].Title)
	}
}

func TestCursorAgentFallsBackToPromptWhenChatMetaMissing(t *testing.T) {
	root := t.TempDir()
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeLines(t, filepath.Join(root, "proj", "agent-transcripts", sid, sid+".jsonl"),
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query> refactor the parser </user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
	)
	ad := &CursorAgentAdapter{Root: root, ChatsRoot: t.TempDir()}
	sessions, _, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].Title != "refactor the parser" {
		t.Fatalf("title = %q, want stripped first user prompt", sessions[0].Title)
	}
}
