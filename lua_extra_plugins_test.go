package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Each test below pins one shipped plugin against a hand-written sample of the
// tool's on-disk format: session identity (id/project/title), which frames are
// dropped, and how tool traffic is flattened into a single searchable line.

func scanPlugin(t *testing.T, plugin, root string) ([]Session, []Message) {
	t.Helper()
	ad := luaAdapterFor(t, filepath.Join("plugins", plugin), root)
	sessions, msgs, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("%s: scan: %v", plugin, err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%s: sessions = %d, want 1 (%+v)", plugin, len(sessions), sessions)
	}
	return sessions, msgs
}

func roles(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

func TestLuaGrok(t *testing.T) {
	root := t.TempDir()
	// The session directory is the id; its parent is the percent-encoded cwd.
	writeLines(t, filepath.Join(root, "%2Fwork%2Facme", "sess-grok-1", "chat_history.jsonl"),
		`{"type":"user","content":[{"type":"text","text":"find the proxy docs"}]}`,
		`{"type":"assistant","content":"on it"}`,
		`{"type":"backend_tool_call","kind":{"tool_type":"web_search","action":{"query":"isp proxy pricing"}}}`,
		`{"type":"tool_result","content":"result body"}`,
		`{"type":"reasoning","content":"model thinking is not indexed"}`,
		`{"type":"system","content":"system prompt is not indexed"}`,
	)
	sessions, msgs := scanPlugin(t, "grok.lua", root)
	s := sessions[0]
	if s.SourceID != "sess-grok-1" || s.Project != "/work/acme" {
		t.Fatalf("session = %+v", s)
	}
	if got := strings.Join(roles(msgs), ","); got != "user,assistant,tool,tool" {
		t.Fatalf("roles = %s", got)
	}
	if !strings.Contains(msgs[2].Text, "web_search isp proxy pricing") {
		t.Errorf("tool call text = %q", msgs[2].Text)
	}
	if msgs[3].Text != "result body" {
		t.Errorf("tool result text = %q", msgs[3].Text)
	}
}

func TestLuaKimi(t *testing.T) {
	root := t.TempDir()
	writeLines(t,
		filepath.Join(root, "wd_acme", "session_0f6b1c1e-1111-2222-3333-444455556666", "agents", "main", "wire.jsonl"),
		`{"type":"turn.prompt","time":1780000000000,"input":[{"type":"text","text":"why is the build red"}]}`,
		`{"type":"context.append_message","time":1780000000100,"message":{"role":"assistant","content":[{"type":"text","text":"checking the log"}]}}`,
		`{"type":"context.append_loop_event","time":1780000000200,"event":{"type":"content.part","part":{"type":"text","text":"found it"}}}`,
		`{"type":"context.append_loop_event","time":1780000000300,"event":{"type":"tool.call","name":"bash","args":"go test ./..."}}`,
		`{"type":"usage.record","time":1780000000400}`,
	)
	sessions, msgs := scanPlugin(t, "kimi.lua", root)
	if id := sessions[0].SourceID; id != "0f6b1c1e-1111-2222-3333-444455556666" {
		t.Fatalf("session id = %q", id)
	}
	if got := strings.Join(roles(msgs), ","); got != "user,assistant,assistant,tool:bash" {
		t.Fatalf("roles = %s", got)
	}
	if msgs[0].TS != 1780000000000 {
		t.Errorf("ts = %d", msgs[0].TS)
	}
}

func TestLuaDroid(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "-work-acme", "sess-droid-1.jsonl"),
		`{"type":"session_start","id":"sess-droid-1","title":"Fix the flaky test","cwd":"/work/acme"}`,
		`{"type":"message","timestamp":"2026-05-01T10:00:00.500Z","message":{"role":"user","content":[{"type":"text","text":"the test is flaky"}]}}`,
		`{"type":"message","timestamp":"2026-05-01T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"running it"},{"type":"tool_use","name":"bash","input":{"command":"go test -count=5"}}]}}`,
		`{"type":"message","timestamp":"2026-05-01T10:00:06.000Z","message":{"role":"assistant","content":[{"type":"thinking","text":"hidden reasoning"}]}}`,
	)
	sessions, msgs := scanPlugin(t, "droid.lua", root)
	s := sessions[0]
	if s.SourceID != "sess-droid-1" || s.Project != "/work/acme" || s.Title != "Fix the flaky test" {
		t.Fatalf("session = %+v", s)
	}
	if s.StartedAt != 1777632e3 && s.StartedAt == 0 {
		t.Errorf("started_at not derived from message ts: %+v", s)
	}
	if got := strings.Join(roles(msgs), ","); got != "user,assistant" {
		t.Fatalf("roles = %s (reasoning-only message must be dropped)", got)
	}
	if !strings.Contains(msgs[1].Text, "running it") || !strings.Contains(msgs[1].Text, "go test -count=5") {
		t.Errorf("assistant text = %q", msgs[1].Text)
	}
}

func TestLuaOmp(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "-work-acme", "2026-05-01T10-00-00-000Z_sess-omp-1.jsonl"),
		`{"type":"session","id":"sess-omp-1","cwd":"/work/acme","title":"Move the skills"}`,
		`{"type":"message","timestamp":"2026-05-01T10:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"move these skills"}]}}`,
		`{"type":"message","timestamp":"2026-05-01T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"git mv a b"}}]}}`,
		`{"type":"message","timestamp":"2026-05-01T10:00:03.000Z","message":{"role":"toolResult","content":[{"type":"text","text":"moved"}]}}`,
	)
	sessions, msgs := scanPlugin(t, "omp.lua", root)
	s := sessions[0]
	if s.SourceID != "sess-omp-1" || s.Project != "/work/acme" || s.Title != "Move the skills" {
		t.Fatalf("session = %+v", s)
	}
	if s.StartedAt == 0 {
		t.Errorf("started_at not derived from message ts: %+v", s)
	}
	if got := strings.Join(roles(msgs), ","); got != "user,assistant,tool" {
		t.Fatalf("roles = %s", got)
	}
}
