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

// Fetch returns the untruncated transcript for a single composer.
// Faster than Scan() because it only touches the rows for this composerId.
func (a *CursorAdapter) Fetch(sourceID string) ([]Message, error) {
	gpath := filepath.Join(a.UserDir, "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", gpath+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// composerData blob
	var blob []byte
	if err := db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`,
		"composerData:"+sourceID).Scan(&blob); err != nil {
		return nil, fmt.Errorf("composerData %s: %w", sourceID, err)
	}
	var cb struct {
		FullConversationHeadersOnly []struct {
			BubbleID string `json:"bubbleId"`
		} `json:"fullConversationHeadersOnly"`
	}
	if err := json.Unmarshal(blob, &cb); err != nil {
		return nil, err
	}

	// All bubbles for this composer (uses key index, fast).
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV
		WHERE key >= ? AND key < ?`,
		"bubbleId:"+sourceID+":", "bubbleId:"+sourceID+";")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type entry struct {
		text  string
		ttype int
	}
	bubbles := map[string]entry{}
	for rows.Next() {
		var key string
		var v []byte
		if err := rows.Scan(&key, &v); err != nil {
			return nil, err
		}
		s := key[len("bubbleId:"):]
		i := strings.Index(s, ":")
		if i <= 0 {
			continue
		}
		mid := s[i+1:]
		text, ttype := extractBubbleText(v)
		bubbles[mid] = entry{text: text, ttype: ttype}
	}

	var msgs []Message
	seen := map[string]bool{}
	idx := 0
	for _, h := range cb.FullConversationHeadersOnly {
		e, ok := bubbles[h.BubbleID]
		if !ok || e.text == "" {
			continue
		}
		seen[h.BubbleID] = true
		msgs = append(msgs, Message{
			SourceID: sourceID, Idx: idx, Role: bubbleRole(e.ttype), TS: 0, Text: e.text,
		})
		idx++
	}
	// Any bubbles not in headers (rare; shouldn't happen for healthy sessions)
	for mid, e := range bubbles {
		if seen[mid] || e.text == "" {
			continue
		}
		msgs = append(msgs, Message{
			SourceID: sourceID, Idx: idx, Role: bubbleRole(e.ttype), TS: 0, Text: e.text,
		})
		idx++
	}
	return msgs, nil
}

// OpenURL — Cursor exposes a `cursor://anysphere.cursor-deeplink/composer/<id>` deep-link
// but the practical UI is to relaunch Cursor with the workspace folder; we return the
// deep-link form and let the caller `open` it on macOS.
func (a *CursorAdapter) OpenURL(sourceID string) string {
	return "cursor://anysphere.cursor-deeplink/composer/" + sourceID
}

// Scan reads:
//  1. Every workspaceStorage/<hash>/state.vscdb to build composerId -> workspaceFolder map.
//  2. globalStorage/state.vscdb for composerData:* and bubbleId:* blobs.
//
// Workspace folders are recovered from each workspace.json (folder URI).
//
// Checkpoint shape: {"rowid": N}. If prev is set, only composers whose
// composerData or bubble rows have rowid > N are re-ingested.
func (a *CursorAdapter) Scan(prev string) ([]Session, []Message, string, error) {
	prevRowID := parseCursorCkpt(prev)
	cidToProject, err := a.mapComposersToProjects()
	if err != nil {
		return nil, nil, "", fmt.Errorf("workspace scan: %w", err)
	}

	gpath := filepath.Join(a.UserDir, "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", gpath+"?mode=ro&_pragma=query_only(true)&immutable=1")
	if err != nil {
		return nil, nil, "", err
	}
	defer db.Close()

	// Determine the set of composerIds to (re)ingest.
	// For a full scan (prevRowID == 0), this is everything in composerData.
	// For incremental, it's any composerId whose composerData OR bubble rows
	// have rowid > prevRowID.
	touched := map[string]bool{}
	if prevRowID > 0 {
		ids, maxR, err := a.collectTouchedComposers(db, prevRowID)
		if err != nil {
			return nil, nil, "", err
		}
		for _, id := range ids {
			touched[id] = true
		}
		// Capture the new max rowid early so we can emit it even on empty diff.
		_ = maxR
	}

	// Load composerData rows. On incremental, restrict to touched composers.
	var rows *sql.Rows
	if prevRowID > 0 && len(touched) > 0 {
		// IN-list with quoted ids
		ids := make([]string, 0, len(touched))
		args := make([]any, 0, len(touched))
		for id := range touched {
			ids = append(ids, "?")
			args = append(args, "composerData:"+id)
		}
		q := `SELECT key, value FROM cursorDiskKV WHERE key IN (` + strings.Join(ids, ",") + `)`
		rows, err = db.Query(q, args...)
	} else if prevRowID == 0 {
		rows, err = db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	} else {
		// incremental but nothing changed
		next, _ := a.currentMaxRowID(db)
		return nil, nil, encodeCursorCkpt(next), nil
	}
	if err != nil {
		return nil, nil, "", err
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
			return nil, nil, "", err
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

	// Now pull bubbles. On incremental scan, restrict to the touched composer set;
	// otherwise pull every bubble in one sweep.
	bubblesByComp := map[string][]bubbleRow{}

	var br *sql.Rows
	if prevRowID > 0 && len(touched) > 0 {
		// One range query per composer (uses the key index efficiently).
		// In practice "touched" is tiny (1-10), so this is much cheaper than a
		// full scan of 245k bubbles.
		for cid := range touched {
			rs, err := db.Query(`SELECT key, value FROM cursorDiskKV
				WHERE key >= ? AND key < ?`,
				"bubbleId:"+cid+":", "bubbleId:"+cid+";")
			if err != nil {
				return nil, nil, "", err
			}
			collectBubbles(rs, bubblesByComp)
		}
	} else {
		br, err = db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'bubbleId:%'`)
		if err != nil {
			return nil, nil, "", err
		}
		collectBubbles(br, bubblesByComp)
	}

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
	nextRowID, _ := a.currentMaxRowID(db)
	return sessions, msgs, encodeCursorCkpt(nextRowID), nil
}

type bubbleRow struct {
	composerID string
	msgID      string
	text       string
	ttype      int
}

// collectBubbles drains a row iterator and buckets bubbles by composerId.
func collectBubbles(rs *sql.Rows, into map[string][]bubbleRow) {
	defer rs.Close()
	for rs.Next() {
		var key string
		var val []byte
		if err := rs.Scan(&key, &val); err != nil {
			return
		}
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
		into[cid] = append(into[cid], bubbleRow{cid, mid, text, ttype})
	}
}

// collectTouchedComposers returns composerIds whose composerData or bubble rows
// have rowid > prevRowID, plus the current max rowid.
func (a *CursorAdapter) collectTouchedComposers(db *sql.DB, prevRowID int64) ([]string, int64, error) {
	seen := map[string]bool{}
	rows, err := db.Query(`SELECT key FROM cursorDiskKV
		WHERE rowid > ? AND (key LIKE 'composerData:%' OR key LIKE 'bubbleId:%')`, prevRowID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, 0, err
		}
		switch {
		case strings.HasPrefix(key, "composerData:"):
			seen[strings.TrimPrefix(key, "composerData:")] = true
		case strings.HasPrefix(key, "bubbleId:"):
			s := key[len("bubbleId:"):]
			if i := strings.Index(s, ":"); i > 0 {
				seen[s[:i]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	maxR, err := a.currentMaxRowID(db)
	return out, maxR, err
}

func (a *CursorAdapter) currentMaxRowID(db *sql.DB) (int64, error) {
	var n sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(rowid) FROM cursorDiskKV`).Scan(&n); err != nil {
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
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
