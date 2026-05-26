package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Index struct {
	db *sql.DB
}

func openIndex(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-65536)")
	if err != nil {
		return nil, err
	}
	if err := bootstrap(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return &Index{db: db}, nil
}

func bootstrap(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id          TEXT PRIMARY KEY,
			source      TEXT NOT NULL,
			source_id   TEXT NOT NULL,
			project     TEXT,
			title       TEXT,
			started_at  INTEGER,
			ended_at    INTEGER,
			msg_count   INTEGER,
			meta        TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project_started
			ON sessions(project, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_started
			ON sessions(started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_source
			ON sessions(source, started_at DESC)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			session_pk UNINDEXED,
			idx        UNINDEXED,
			role       UNINDEXED,
			ts         UNINDEXED,
			text,
			tokenize = 'porter unicode61 remove_diacritics 1'
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
			session_pk UNINDEXED,
			title,
			project,
			tokenize = 'porter unicode61 remove_diacritics 1'
		)`,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

// IngestBatch upserts a batch of sessions + messages in one tx.
// Each session's existing messages are deleted first (cheap given size of FTS).
func (ix *Index) IngestBatch(source string, sessions []Session, msgs []Message) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upsertSess, err := tx.Prepare(`INSERT INTO sessions(id, source, source_id, project, title, started_at, ended_at, msg_count, meta)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project=excluded.project,
			title=excluded.title,
			started_at=excluded.started_at,
			ended_at=excluded.ended_at,
			msg_count=excluded.msg_count,
			meta=excluded.meta`)
	if err != nil {
		return err
	}
	defer upsertSess.Close()

	delFTS, err := tx.Prepare(`DELETE FROM messages_fts WHERE session_pk = ?`)
	if err != nil {
		return err
	}
	defer delFTS.Close()

	insFTS, err := tx.Prepare(`INSERT INTO messages_fts(session_pk, idx, role, ts, text) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFTS.Close()

	delSessFTS, err := tx.Prepare(`DELETE FROM sessions_fts WHERE session_pk = ?`)
	if err != nil {
		return err
	}
	defer delSessFTS.Close()

	insSessFTS, err := tx.Prepare(`INSERT INTO sessions_fts(session_pk, title, project) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer insSessFTS.Close()

	bySession := map[string][]Message{}
	for _, m := range msgs {
		bySession[m.SourceID] = append(bySession[m.SourceID], m)
	}

	for _, s := range sessions {
		pk := source + ":" + s.SourceID
		var metaBlob []byte
		if len(s.Meta) > 0 {
			metaBlob, _ = JSONMarshal(s.Meta)
		}
		if _, err := upsertSess.Exec(pk, source, s.SourceID, s.Project, s.Title,
			s.StartedAt, s.EndedAt, s.MsgCount, string(metaBlob)); err != nil {
			return fmt.Errorf("upsert session %s: %w", pk, err)
		}
		if _, err := delFTS.Exec(pk); err != nil {
			return err
		}
		if _, err := delSessFTS.Exec(pk); err != nil {
			return err
		}
		if _, err := insSessFTS.Exec(pk, s.Title, s.Project); err != nil {
			return fmt.Errorf("insert session_fts %s: %w", pk, err)
		}
		for _, m := range bySession[s.SourceID] {
			text := trimText(m.Text)
			if text == "" {
				continue
			}
			if _, err := insFTS.Exec(pk, m.Idx, m.Role, m.TS, text); err != nil {
				return fmt.Errorf("insert msg %s/%d: %w", pk, m.Idx, err)
			}
		}
	}
	return tx.Commit()
}

func trimText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > excerptMax {
		s = s[:excerptMax]
	}
	return s
}

// BulkMode toggles SQLite pragmas tuned for bulk ingest. Safe because the
// recall index is disposable: a crash mid-`recall index` just means rerun.
// Callers should defer ix.BulkMode(false).
//
//	synchronous=OFF      — skip fsync at commit (~2× faster on APFS)
//	journal_mode=MEMORY  — keep rollback journal off disk for this run
//	cache_size=-262144   — 256 MB page cache
//	FTS5 automerge=0     — defer index merging until 'optimize' below
//
// When BulkMode(false) is called we restore the steady state (WAL + NORMAL +
// 64 MB cache) and run an FTS5 'optimize' once so search performance matches
// what we'd get from incremental writes.
func (ix *Index) BulkMode(on bool) {
	if on {
		_, _ = ix.db.Exec(`PRAGMA synchronous=OFF`)
		_, _ = ix.db.Exec(`PRAGMA journal_mode=MEMORY`)
		_, _ = ix.db.Exec(`PRAGMA cache_size=-262144`)
		// Defer FTS5 segment merges; the integrity-check happens at optimize.
		_, _ = ix.db.Exec(`INSERT INTO messages_fts(messages_fts, rank) VALUES('automerge', 0)`)
		_, _ = ix.db.Exec(`INSERT INTO sessions_fts(sessions_fts, rank) VALUES('automerge', 0)`)
		return
	}
	// Flush any pending merges so query latency is stable.
	_, _ = ix.db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('optimize')`)
	_, _ = ix.db.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('optimize')`)
	_, _ = ix.db.Exec(`PRAGMA synchronous=NORMAL`)
	_, _ = ix.db.Exec(`PRAGMA journal_mode=WAL`)
	_, _ = ix.db.Exec(`PRAGMA cache_size=-65536`)
}

func (ix *Index) SetMeta(k, v string) error {
	_, err := ix.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (ix *Index) GetMeta(k string) string {
	var v string
	if err := ix.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

func (ix *Index) Counts() (map[string]int, error) {
	rows, err := ix.db.Query(`SELECT source, COUNT(*) FROM sessions GROUP BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var src string
		var n int
		if err := rows.Scan(&src, &n); err != nil {
			return nil, err
		}
		out[src] = n
	}
	return out, rows.Err()
}

// Hit is one matched message with its parent session info.
type Hit struct {
	SessionID string  `json:"session_id"`
	Source    string  `json:"source"`
	SourceID  string  `json:"source_id"`
	Project   string  `json:"project"`
	Title     string  `json:"title"`
	StartedAt int64   `json:"started_at_ms"`
	MsgIdx    int     `json:"msg_idx"`
	Role      string  `json:"role"`
	Snippet   string  `json:"snippet"` // FTS-highlighted excerpt
	Rank      float64 `json:"rank"`
}

func (h Hit) StartedTime() time.Time { return time.UnixMilli(h.StartedAt) }

// Search runs FTS5 over messages, joins back to sessions, and ranks by bm25 + recency.
func (ix *Index) Search(query string, opts SearchOpts) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return ix.recent(opts)
	}
	// Escape query for FTS5: wrap in double quotes to avoid syntax errors.
	// opts.AnyTerm switches AND → OR (used by Related where ANDing 6+ terms
	// over-constrains the match).
	ftsQuery := ftsEscape(query, opts.AnyTerm)

	args := []any{ftsQuery}
	where := []string{"messages_fts MATCH ?"}
	if opts.Source != "" {
		where = append(where, "s.source = ?")
		args = append(args, opts.Source)
	}
	if opts.Project != "" {
		where = append(where, "s.project = ?")
		args = append(args, opts.Project)
	}
	if opts.Since > 0 {
		where = append(where, "s.started_at >= ?")
		args = append(args, opts.Since)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	}
	args = append(args, limit)

	// Two FTS sources: message bodies and session titles/projects.
	// We UNION ALL and dedup in Go, keeping the best (most negative bm25) hit per session.
	bodyWhere := append([]string{}, where...)      // messages_fts MATCH ? + filters
	titleWhere := []string{"sessions_fts MATCH ?"} // separate match
	titleArgs := []any{ftsQuery}
	for _, w := range where[1:] {
		titleWhere = append(titleWhere, w)
	}
	titleArgs = append(titleArgs, args[1:len(args)-1]...) // copy filter args (exclude old MATCH + limit)

	q := `
SELECT id, source, source_id, project, title, started_at, idx, role, snippet, rank FROM (
  SELECT s.id AS id, s.source AS source, s.source_id AS source_id,
         COALESCE(s.project,'') AS project, COALESCE(s.title,'') AS title,
         COALESCE(s.started_at,0) AS started_at,
         f.idx AS idx, f.role AS role,
         snippet(messages_fts, 4, '«', '»', '…', 12) AS snippet,
         bm25(messages_fts) AS rank
    FROM messages_fts f JOIN sessions s ON s.id = f.session_pk
   WHERE ` + strings.Join(bodyWhere, " AND ") + `
  UNION ALL
  SELECT s.id, s.source, s.source_id,
         COALESCE(s.project,''), COALESCE(s.title,''),
         COALESCE(s.started_at,0),
         -1 AS idx, 'title' AS role,
         '['||snippet(sessions_fts, 1, '«', '»', '…', 12)||']' AS snippet,
         bm25(sessions_fts) * 1.4 AS rank   -- title hits get a small boost
    FROM sessions_fts JOIN sessions s ON s.id = sessions_fts.session_pk
   WHERE ` + strings.Join(titleWhere, " AND ") + `
)
ORDER BY (rank * 1.0) - (started_at / 1.0e13) ASC
LIMIT ?`

	allArgs := make([]any, 0, len(args)+len(titleArgs))
	allArgs = append(allArgs, args[:len(args)-1]...) // body args without limit
	allArgs = append(allArgs, titleArgs...)
	allArgs = append(allArgs, args[len(args)-1])

	rows, err := ix.db.Query(q, allArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	seen := map[string]int{} // session_id → idx in hits; keep best snippet only
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.SessionID, &h.Source, &h.SourceID, &h.Project, &h.Title,
			&h.StartedAt, &h.MsgIdx, &h.Role, &h.Snippet, &h.Rank); err != nil {
			return nil, err
		}
		if prev, ok := seen[h.SessionID]; ok {
			if h.Rank < hits[prev].Rank {
				hits[prev] = h
			}
			continue
		}
		seen[h.SessionID] = len(hits)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (ix *Index) recent(opts SearchOpts) ([]Hit, error) {
	where := []string{"1=1"}
	var args []any
	if opts.Source != "" {
		where = append(where, "source = ?")
		args = append(args, opts.Source)
	}
	if opts.Project != "" {
		where = append(where, "project = ?")
		args = append(args, opts.Project)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	}
	args = append(args, limit)
	q := `SELECT id, source, source_id, COALESCE(project,''), COALESCE(title,''), COALESCE(started_at,0)
	      FROM sessions WHERE ` + strings.Join(where, " AND ") + `
	      ORDER BY started_at DESC LIMIT ?`
	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.SessionID, &h.Source, &h.SourceID, &h.Project, &h.Title, &h.StartedAt); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

type SearchOpts struct {
	Source  string
	Project string
	Since   int64
	Limit   int
	AnyTerm bool // OR terms instead of AND (used by Related)
}

// ftsEscape turns an arbitrary user string into a safe FTS5 MATCH expression.
// We split on whitespace, strip FTS punctuation, quote each token, and join
// with AND (or OR when anyTerm is set).
func ftsEscape(q string, anyTerm bool) string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Map(func(r rune) rune {
			switch r {
			case '"', '*', ':', '(', ')':
				return -1
			}
			return r
		}, f)
		if f == "" {
			continue
		}
		out = append(out, `"`+f+`"`)
	}
	if len(out) == 0 {
		return `""`
	}
	sep := " AND "
	if anyTerm {
		sep = " OR "
	}
	return strings.Join(out, sep)
}

// StatRow is one bucket of `recall stats` output.
type StatRow struct {
	Source   string `json:"source"`
	Project  string `json:"project,omitempty"`
	Sessions int    `json:"sessions"`
	Messages int    `json:"messages"`
}

// Stats aggregates session/message counts by source × project, optionally
// scoped by the same filters as Search (project, source, since).
func (ix *Index) Stats(opts SearchOpts) ([]StatRow, error) {
	where := []string{"1=1"}
	var args []any
	if opts.Source != "" {
		where = append(where, "source = ?")
		args = append(args, opts.Source)
	}
	if opts.Project != "" {
		where = append(where, "project = ?")
		args = append(args, opts.Project)
	}
	if opts.Since > 0 {
		where = append(where, "started_at >= ?")
		args = append(args, opts.Since)
	}
	q := `SELECT source, COALESCE(project,''), COUNT(*), COALESCE(SUM(msg_count),0)
	        FROM sessions WHERE ` + strings.Join(where, " AND ") + `
	    GROUP BY source, project ORDER BY 4 DESC, 3 DESC`
	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var r StatRow
		if err := rows.Scan(&r.Source, &r.Project, &r.Sessions, &r.Messages); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Related finds other sessions covering similar topics to `id` by sampling
// distinctive terms from its title + early user messages and running them
// back through FTS5. Lightweight pseudo-relevance feedback — no embeddings.
func (ix *Index) Related(id string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	// Pull title + first ~5 user/assistant messages to mine a query from.
	rows, err := ix.db.Query(`
		SELECT COALESCE(s.title,''),
		       f.text
		  FROM sessions s LEFT JOIN messages_fts f ON f.session_pk = s.id
		 WHERE s.id = ?
		 ORDER BY f.idx ASC LIMIT 6`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var title string
	var snippets []string
	for rows.Next() {
		var t, txt string
		if err := rows.Scan(&t, &txt); err != nil {
			return nil, err
		}
		if title == "" {
			title = t
		}
		if txt != "" {
			snippets = append(snippets, txt)
		}
	}
	if title == "" && len(snippets) == 0 {
		return nil, fmt.Errorf("session %s not found or empty", id)
	}
	query := topTerms(title+" "+strings.Join(snippets, " "), 6)
	hits, err := ix.Search(query, SearchOpts{Limit: limit + 1, AnyTerm: true})
	if err != nil {
		return nil, err
	}
	// Drop the seed session itself from results.
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.SessionID == id {
			continue
		}
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// topTerms picks the highest-information tokens from text — words ≥ 4
// letters that aren't common chat boilerplate. Good enough for related-
// session probing without pulling in a real IDF table.
func topTerms(text string, n int) string {
	freq := map[string]int{}
	for _, raw := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) {
		w := strings.ToLower(raw)
		if len(w) < 4 || isStopWord(w) {
			continue
		}
		freq[w]++
	}
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(freq))
	for k, v := range freq {
		pairs = append(pairs, kv{k, v})
	}
	// Sort: higher freq first, alphabetical tiebreak.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	out := make([]string, 0, n)
	for _, p := range pairs {
		out = append(out, p.k)
		if len(out) >= n {
			break
		}
	}
	return strings.Join(out, " ")
}

var stopWords = map[string]bool{
	"this": true, "that": true, "with": true, "from": true, "have": true,
	"will": true, "would": true, "should": true, "could": true, "been": true,
	"were": true, "what": true, "when": true, "where": true, "which": true,
	"there": true, "their": true, "them": true, "they": true, "your": true,
	"into": true, "than": true, "then": true, "more": true, "some": true,
	"like": true, "just": true, "only": true, "also": true, "user": true,
	"used": true, "using": true, "code": true, "make": true, "want": true,
	"need": true, "tool": true,
}

func isStopWord(w string) bool { return stopWords[w] }

func (ix *Index) Close() error { return ix.db.Close() }

// LookupSession returns a session row by composite id (e.g. "cursor:<uuid>").
func (ix *Index) LookupSession(id string) (*Session, error) {
	var s Session
	var meta sql.NullString
	row := ix.db.QueryRow(`SELECT source, source_id, COALESCE(project,''), COALESCE(title,''),
		COALESCE(started_at,0), COALESCE(ended_at,0), COALESCE(msg_count,0), meta
		FROM sessions WHERE id = ?`, id)
	if err := row.Scan(&s.Source, &s.SourceID, &s.Project, &s.Title,
		&s.StartedAt, &s.EndedAt, &s.MsgCount, &meta); err != nil {
		return nil, err
	}
	if meta.Valid && meta.String != "" {
		_ = JSONUnmarshal([]byte(meta.String), &s.Meta)
	}
	return &s, nil
}
