package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type CursorAdapter struct {
	UserDir string
	// chunk is how many composers to read + emit per batch (0 = default).
	// Exposed for tests to force per-composer batching on small fixtures.
	chunk int
}

const defaultCursorChunk = 64

func (a *CursorAdapter) chunkSize() int {
	if a.chunk > 0 {
		return a.chunk
	}
	return defaultCursorChunk
}

func (a *CursorAdapter) ID() string { return "cursor" }
func (a *CursorAdapter) Available() bool {
	_, err := os.Stat(filepath.Join(a.UserDir, "globalStorage", "state.vscdb"))
	return err == nil
}

// sourceSQLiteDSN builds a strictly read-only DSN for a source database.
// The file: URI form (path URL-encoded, so spaces in "Application Support"
// survive) is REQUIRED for modernc/SQLite to honor mode=ro+immutable. A bare
// "path?params" DSN is silently opened read-write and CHECKPOINTS a WAL
// database on close — mutating the user's source data. immutable=1 also stops
// SQLite from creating/altering -wal/-shm sidecars.
func sourceSQLiteDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String() + "?mode=ro&immutable=1"
}

func (a *CursorAdapter) Fetch(ctx context.Context, sourceID string) ([]Message, error) {
	gpath := filepath.Join(a.UserDir, "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", sourceSQLiteDSN(gpath))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var blob []byte
	if err := db.QueryRowContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`,
		"composerData:"+sourceID).Scan(&blob); err != nil {
		return nil, fmt.Errorf("composerData %s: %w", sourceID, err)
	}
	var cb struct {
		FullConversationHeadersOnly []struct {
			BubbleID string `json:"bubbleId"`
		} `json:"fullConversationHeadersOnly"`
	}
	if err := JSONUnmarshal(blob, &cb); err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `SELECT key, value FROM cursorDiskKV
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

func (a *CursorAdapter) OpenURL(sourceID string) string {
	return "cursor://anysphere.cursor-deeplink/composer/" + sourceID
}

func (a *CursorAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	return collectScan(ctx, a, prev)
}

type cursorComposerHeader struct {
	BubbleID string `json:"bubbleId"`
}

type cursorComposerBlob struct {
	ComposerID                  string                 `json:"composerId"`
	CreatedAt                   int64                  `json:"createdAt"`
	Name                        string                 `json:"name"`
	FullConversationHeadersOnly []cursorComposerHeader `json:"fullConversationHeadersOnly"`
}

func (a *CursorAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) error {
	ck := parseCursorCkpt(prev)

	gpath := filepath.Join(a.UserDir, "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", sourceSQLiteDSN(gpath))
	if err != nil {
		return err
	}
	defer db.Close()

	curMax, err := a.currentMaxRowID(ctx, db)
	if err != nil {
		return err
	}

	// Decide the work list and the watermark to record once the pass completes.
	var todo []string
	passRowID := curMax
	switch {
	case len(ck.Todo) > 0:
		// Resume an interrupted pass from exactly where it stopped.
		todo, passRowID = ck.Todo, ck.RowID
	case ck.RowID == 0:
		// First full pass: every composer.
		if todo, err = a.allComposerIDs(ctx, db); err != nil {
			return err
		}
	default:
		// Incremental: only composers touched since the last completed pass.
		if todo, _, err = a.collectTouchedComposers(ctx, db, ck.RowID); err != nil {
			return err
		}
		if len(todo) == 0 {
			return emit(nil, nil, encodeCursorCkpt(cursorCkpt{RowID: curMax}))
		}
	}
	sort.Strings(todo) // stable order → deterministic resume

	cidToProject, err := a.mapComposersToProjects(ctx)
	if err != nil {
		return fmt.Errorf("workspace scan: %w", err)
	}

	if len(todo) == 0 {
		return emit(nil, nil, encodeCursorCkpt(cursorCkpt{RowID: passRowID}))
	}
	chunk := a.chunkSize()
	for i := 0; i < len(todo); i += chunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(i+chunk, len(todo))
		sessions, msgs, err := a.readComposers(ctx, db, todo[i:end], cidToProject)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err // don't commit a chunk that may be partial
		}
		out := cursorCkpt{RowID: passRowID}
		if remaining := todo[end:]; len(remaining) > 0 {
			out.Todo = remaining
		}
		if err := emit(sessions, msgs, encodeCursorCkpt(out)); err != nil {
			return err
		}
	}
	return nil
}

func (a *CursorAdapter) allComposerIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimPrefix(key, "composerData:"))
	}
	return out, rows.Err()
}

// readComposers loads composerData + bubbles for a chunk of composer ids and
// builds their sessions/messages.
func (a *CursorAdapter) readComposers(ctx context.Context, db *sql.DB, cids []string, cidToProject map[string]string) ([]Session, []Message, error) {
	if len(cids) == 0 {
		return nil, nil, nil
	}
	ph := make([]string, len(cids))
	args := make([]any, len(cids))
	for i, cid := range cids {
		ph[i] = "?"
		args[i] = "composerData:" + cid
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM cursorDiskKV WHERE key IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, nil, err
	}
	composers := map[string]*cursorComposerBlob{}
	for rows.Next() {
		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var cb cursorComposerBlob
		if JSONUnmarshal(val, &cb) != nil {
			continue
		}
		cid := strings.TrimPrefix(key, "composerData:")
		if cb.ComposerID == "" {
			cb.ComposerID = cid
		}
		composers[cid] = &cb
	}
	rows.Close()

	bubblesByComp := map[string][]bubbleRow{}
	for _, cid := range cids {
		rs, err := db.QueryContext(ctx, `SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ?`,
			"bubbleId:"+cid+":", "bubbleId:"+cid+";")
		if err != nil {
			return nil, nil, err
		}
		collectBubbles(ctx, rs, bubblesByComp)
	}

	var sessions []Session
	var msgs []Message
	for cid, c := range composers {
		bubbles := bubblesByComp[cid]
		bubbleByID := make(map[string]bubbleRow, len(bubbles))
		for _, b := range bubbles {
			bubbleByID[b.msgID] = b
		}
		var ordered []bubbleRow
		seen := map[string]bool{}
		for _, h := range c.FullConversationHeadersOnly {
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
		var firstUser string
		for idx, b := range ordered {
			role := bubbleRole(b.ttype)
			if role == "user" && firstUser == "" && !looksLikeWrapper(b.text) {
				firstUser = b.text
			}
			msgs = append(msgs, Message{SourceID: cid, Idx: idx, Role: role, TS: 0, Text: b.text})
		}
		title := c.Name
		if title == "" {
			title = titleFromPrompt(firstUser)
		}
		sessions = append(sessions, Session{
			Source: "cursor", SourceID: cid, Project: cidToProject[cid],
			Title: title, StartedAt: c.CreatedAt, EndedAt: c.CreatedAt, MsgCount: len(ordered),
		})
	}
	return sessions, msgs, nil
}

type bubbleRow struct {
	composerID string
	msgID      string
	text       string
	ttype      int
}

func collectBubbles(ctx context.Context, rs *sql.Rows, into map[string][]bubbleRow) {
	defer rs.Close()
	n := 0
	for rs.Next() {
		if n&4095 == 0 && ctx.Err() != nil {
			return
		}
		n++
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

func (a *CursorAdapter) collectTouchedComposers(ctx context.Context, db *sql.DB, prevRowID int64) ([]string, int64, error) {
	seen := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT key FROM cursorDiskKV
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
	maxR, err := a.currentMaxRowID(ctx, db)
	return out, maxR, err
}

func (a *CursorAdapter) currentMaxRowID(ctx context.Context, db *sql.DB) (int64, error) {
	var n sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(rowid) FROM cursorDiskKV`).Scan(&n); err != nil {
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

type CursorBubble struct {
	Type     int    `json:"type"`
	Text     string `json:"text"`
	RichText string `json:"richText"`
	Content  string `json:"content"`
}

func extractBubbleText(raw []byte) (string, int) {
	var b CursorBubble
	if err := JSONUnmarshal(raw, &b); err != nil {
		return "", 0
	}
	if b.Text != "" {
		return b.Text, b.Type
	}
	if b.RichText != "" {
		if plain := plainFromRichText(b.RichText); plain != "" {
			return plain, b.Type
		}
	}
	if b.Content != "" {
		return b.Content, b.Type
	}
	return "", b.Type
}

type richTextDoc struct {
	Root struct {
		Children []richTextNode `json:"children"`
	} `json:"root"`
}

type richTextNode struct {
	Text     string         `json:"text"`
	Children []richTextNode `json:"children"`
}

func plainFromRichText(s string) string {
	var doc richTextDoc
	if err := JSONUnmarshal([]byte(s), &doc); err != nil || len(doc.Root.Children) == 0 {
		return s
	}
	var sb strings.Builder
	var walk func(n richTextNode)
	walk = func(n richTextNode) {
		if n.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(n.Text)
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	for _, c := range doc.Root.Children {
		walk(c)
	}
	if sb.Len() == 0 {
		return s
	}
	return sb.String()
}

func (a *CursorAdapter) mapComposersToProjects(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	wsRoot := filepath.Join(a.UserDir, "workspaceStorage")
	entries, err := os.ReadDir(wsRoot)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(wsRoot, e.Name())
		folder := readWorkspaceFolder(filepath.Join(dir, "workspace.json"))
		dbPath := filepath.Join(dir, "state.vscdb")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		db, err := sql.Open("sqlite", sourceSQLiteDSN(dbPath))
		if err != nil {
			continue
		}
		row := db.QueryRowContext(ctx, `SELECT value FROM ItemTable WHERE key = 'composer.composerData'`)
		var blob []byte
		if err := row.Scan(&blob); err == nil {
			var data struct {
				AllComposers []struct {
					ComposerID string `json:"composerId"`
				} `json:"allComposers"`
			}
			if err := JSONUnmarshal(blob, &data); err == nil {
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
	if err := JSONUnmarshal(b, &w); err != nil {
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
