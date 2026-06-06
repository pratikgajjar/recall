package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- generic JSONL helpers -------------------------------------------------

// writeLines writes lines as a newline-terminated JSONL file (creating dirs).
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendLines appends more newline-terminated lines to an existing file.
func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// appendRaw appends bytes verbatim (used to simulate a half-written line).
func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// --- assertions ------------------------------------------------------------

func findSession(sessions []Session, sid string) (Session, bool) {
	for _, s := range sessions {
		if s.SourceID == sid {
			return s, true
		}
	}
	return Session{}, false
}

func msgsFor(msgs []Message, sid string) []Message {
	var out []Message
	for _, m := range msgs {
		if m.SourceID == sid {
			out = append(out, m)
		}
	}
	return out
}

// --- cursor SQLite fixture -------------------------------------------------

type tBubble struct {
	id   string
	typ  int // 1=user, 2=assistant, other=tool
	text string
}

type tComposer struct {
	id        string
	name      string
	createdAt int64
	bubbles   []tBubble
}

// cursorGlobalDB returns the path to the (created-on-demand) globalStorage DB.
func cursorGlobalDB(t *testing.T, userDir string) string {
	t.Helper()
	dir := filepath.Join(userDir, "globalStorage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "state.vscdb")
}

// addCursorComposers inserts composers (and their bubbles) into the global DB
// in order, so each gets a higher rowid — mirroring how Cursor appends chats.
func addCursorComposers(t *testing.T, userDir string, composers ...tComposer) {
	t.Helper()
	db, err := sql.Open("sqlite", cursorGlobalDB(t, userDir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range composers {
		headers := make([]map[string]any, 0, len(c.bubbles))
		for _, b := range c.bubbles {
			headers = append(headers, map[string]any{"bubbleId": b.id})
		}
		cd, _ := json.Marshal(map[string]any{
			"composerId":                  c.id,
			"name":                        c.name,
			"createdAt":                   c.createdAt,
			"fullConversationHeadersOnly": headers,
		})
		if _, err := db.Exec(`INSERT INTO cursorDiskKV(key,value) VALUES(?,?)`, "composerData:"+c.id, cd); err != nil {
			t.Fatal(err)
		}
		for _, b := range c.bubbles {
			bv, _ := json.Marshal(map[string]any{"type": b.typ, "text": b.text})
			if _, err := db.Exec(`INSERT INTO cursorDiskKV(key,value) VALUES(?,?)`,
				"bubbleId:"+c.id+":"+b.id, bv); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// addCursorBubble appends a single bubble row to an existing composer without
// rewriting its composerData header. The new row gets a higher rowid, so it's
// visible to the watermark-incremental kv scan and reaches the transform via
// the unseen-bubble tail loop.
func addCursorBubble(t *testing.T, userDir, cid string, b tBubble) {
	t.Helper()
	db, err := sql.Open("sqlite", cursorGlobalDB(t, userDir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bv, _ := json.Marshal(map[string]any{"type": b.typ, "text": b.text})
	if _, err := db.Exec(`INSERT INTO cursorDiskKV(key,value) VALUES(?,?)`,
		"bubbleId:"+cid+":"+b.id, bv); err != nil {
		t.Fatal(err)
	}
}

// addCursorWorkspace maps the given composer ids to a project folder via a
// workspaceStorage entry (workspace.json + per-workspace state.vscdb).
func addCursorWorkspace(t *testing.T, userDir, hash, folder string, composerIDs ...string) {
	t.Helper()
	dir := filepath.Join(userDir, "workspaceStorage", hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	wj, _ := json.Marshal(map[string]any{"folder": "file://" + folder})
	if err := os.WriteFile(filepath.Join(dir, "workspace.json"), wj, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ItemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	all := make([]map[string]any, 0, len(composerIDs))
	for _, id := range composerIDs {
		all = append(all, map[string]any{"composerId": id})
	}
	v, _ := json.Marshal(map[string]any{"allComposers": all})
	if _, err := db.Exec(`INSERT OR REPLACE INTO ItemTable(key,value) VALUES('composer.composerData',?)`, v); err != nil {
		t.Fatal(err)
	}
}

// --- index helper ----------------------------------------------------------

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := openIndex(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}
