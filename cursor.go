package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type CursorAdapter struct {
	UserDir string // ~/Library/Application Support/Cursor/User
}

func (a *CursorAdapter) ID() string { return "cursor" }
func (a *CursorAdapter) Available() bool {
	_, err := os.Stat(filepath.Join(a.UserDir, "globalStorage", "state.vscdb"))
	return err == nil
}

// Scan reads:
//  1. Every workspaceStorage/<hash>/state.vscdb to build composerId -> workspaceFolder map.
//  2. globalStorage/state.vscdb for composerData:* and bubbleId:* blobs.
//
// Workspace folders are recovered from each workspace.json (folder URI).
func (a *CursorAdapter) Scan() ([]Session, []Message, error) {
	cidToProject, err := a.mapComposersToProjects()
	if err != nil {
		return nil, nil, fmt.Errorf("workspace scan: %w", err)
	}

	gpath := filepath.Join(a.UserDir, "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", gpath+"?mode=ro&_pragma=query_only(true)&immutable=1")
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	// Load all composerData rows first (small — 2-3k typically, ~150MB blob total).
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return nil, nil, err
	}

	type composerHeader struct {
		BubbleID string `json:"bubbleId"`
		Type     any    `json:"type"` // sometimes int, sometimes string
		ServerID string `json:"serverBubbleId"`
	}
	type composerBlob struct {
		ComposerID                   string           `json:"composerId"`
		Text                         string           `json:"text"`
		CreatedAt                    int64            `json:"createdAt"`
		Name                         string           `json:"name"`
		FullConversationHeadersOnly  []composerHeader `json:"fullConversationHeadersOnly"`
	}

	type composer struct {
		blob   composerBlob
		nameOv string // from workspace allComposers (often present when blob.Name empty)
	}

	composers := map[string]*composer{}

	for rows.Next() {
		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var cb composerBlob
		if err := json.Unmarshal(val, &cb); err != nil {
			continue
		}
		cid := strings.TrimPrefix(key, "composerData:")
		if cb.ComposerID == "" {
			cb.ComposerID = cid
		}
		composers[cid] = &composer{blob: cb}
	}
	rows.Close()

	// Now pull every bubble. They are keyed bubbleId:<composerId>:<msgId>.
	// We do one big query and bucket in memory.
	type bubbleRow struct {
		composerID string
		msgID      string
		text       string
		ttype      int
	}
	bubblesByComp := map[string][]bubbleRow{}

	br, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'bubbleId:%'`)
	if err != nil {
		return nil, nil, err
	}
	for br.Next() {
		var key string
		var val []byte
		if err := br.Scan(&key, &val); err != nil {
			br.Close()
			return nil, nil, err
		}
		// key = bubbleId:<cid>:<mid>
		s := key[len("bubbleId:"):]
		i := strings.Index(s, ":")
		if i <= 0 {
			continue
		}
		cid := s[:i]
		mid := s[i+1:]
		text, ttype := extractBubbleText(val)
		if text == "" {
			continue
		}
		bubblesByComp[cid] = append(bubblesByComp[cid], bubbleRow{
			composerID: cid, msgID: mid, text: text, ttype: ttype,
		})
	}
	br.Close()

	// Compose sessions in the order specified by composerData.fullConversationHeadersOnly.
	// Fall back to discovered bubble order otherwise.
	var sessions []Session
	var msgs []Message

	for cid, c := range composers {
		var firstUser string
		bubbles := bubblesByComp[cid]
		bubbleByID := map[string]bubbleRow{}
		for _, b := range bubbles {
			bubbleByID[b.msgID] = b
		}

		var ordered []bubbleRow
		seen := map[string]bool{}
		for _, h := range c.blob.FullConversationHeadersOnly {
			if b, ok := bubbleByID[h.BubbleID]; ok {
				ordered = append(ordered, b)
				seen[h.BubbleID] = true
			}
		}
		for _, b := range bubbles {
			if !seen[b.msgID] {
				ordered = append(ordered, b)
			}
		}
		if len(ordered) == 0 {
			continue
		}

		for idx, b := range ordered {
			role := bubbleRole(b.ttype)
			if role == "user" && firstUser == "" && !looksLikeWrapper(b.text) {
				firstUser = b.text
			}
			msgs = append(msgs, Message{
				SourceID: cid, Idx: idx, Role: role, TS: 0, Text: b.text,
			})
		}

		title := c.blob.Name
		if title == "" {
			title = titleFromPrompt(firstUser)
		}

		sessions = append(sessions, Session{
			Source:    "cursor",
			SourceID:  cid,
			Project:   cidToProject[cid], // may be "" if orphaned
			Title:     title,
			StartedAt: c.blob.CreatedAt,
			EndedAt:   c.blob.CreatedAt,
			MsgCount:  len(ordered),
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt < sessions[j].StartedAt
	})
	return sessions, msgs, nil
}

func bubbleRole(t int) string {
	switch t {
	case 1:
		return "user"
	case 2:
		return "assistant"
	}
	return "tool"
}

// extractBubbleText pulls the human-readable body out of a bubble value blob.
// Bubbles have many shapes; we try `text`, `richText` plaintext, then `content`.
func extractBubbleText(raw []byte) (string, int) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", 0
	}
	ttype := 0
	if t, ok := m["type"].(float64); ok {
		ttype = int(t)
	}
	if s, _ := m["text"].(string); s != "" {
		return s, ttype
	}
	// richText is sometimes a JSON string of a draft-js doc.
	if rt, ok := m["richText"].(string); ok && rt != "" {
		if plain := plainFromRichText(rt); plain != "" {
			return plain, ttype
		}
	}
	// content slot used in some assistant bubbles
	if c, ok := m["content"].(string); ok && c != "" {
		return c, ttype
	}
	return "", ttype
}

// plainFromRichText walks a draft-js style payload looking for any "text" leaves.
func plainFromRichText(s string) string {
	var doc any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return s // raw string fallback
	}
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if txt, ok := t["text"].(string); ok && txt != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(txt)
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	return b.String()
}

// mapComposersToProjects iterates every workspaceStorage/<hash>/state.vscdb,
// reads composer.composerData → allComposers[], joined with workspace.json folder URI.
func (a *CursorAdapter) mapComposersToProjects() (map[string]string, error) {
	out := map[string]string{}
	wsRoot := filepath.Join(a.UserDir, "workspaceStorage")
	entries, err := os.ReadDir(wsRoot)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(wsRoot, e.Name())
		folder := readWorkspaceFolder(filepath.Join(dir, "workspace.json"))
		dbPath := filepath.Join(dir, "state.vscdb")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		db, err := sql.Open("sqlite", dbPath+"?mode=ro&immutable=1")
		if err != nil {
			continue
		}
		row := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'composer.composerData'`)
		var blob []byte
		if err := row.Scan(&blob); err == nil {
			var data struct {
				AllComposers []struct {
					ComposerID string `json:"composerId"`
				} `json:"allComposers"`
			}
			if err := json.Unmarshal(blob, &data); err == nil {
				for _, c := range data.AllComposers {
					if c.ComposerID != "" && folder != "" {
						out[c.ComposerID] = folder
					}
				}
			}
		}
		db.Close()
	}
	return out, nil
}

func readWorkspaceFolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var w struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return ""
	}
	if w.Folder == "" {
		return ""
	}
	if u, err := url.Parse(w.Folder); err == nil && u.Scheme == "file" {
		return u.Path
	}
	return w.Folder
}
