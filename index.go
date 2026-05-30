package main

import (
	"context"
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

const (
	sqliteMmapBytes   = 256 << 20 // memory-map the index so FTS reads hit page cache
	sqliteCacheKiB    = 64 << 10  // page cache; negative cache_size means KiB
	sqliteBusyTimeout = 10_000    // ms; safety net — WAL means it should never fire
)

// indexDSN builds the modernc.org/sqlite connection string. Readers and the
// writer share the tuning pragmas but differ where it matters: the writer keeps
// WAL and grabs its lock up front (_txlock=immediate); readers are query_only so
// a search can never take a write lock — under WAL it never blocks the writer.
func indexDSN(path string, readOnly bool) string {
	pragmas := []string{
		"synchronous(NORMAL)",
		"temp_store(MEMORY)",
		fmt.Sprintf("cache_size(-%d)", sqliteCacheKiB),
		fmt.Sprintf("mmap_size(%d)", sqliteMmapBytes),
		fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeout),
	}
	var params []string
	if readOnly {
		pragmas = append(pragmas, "query_only(1)")
	} else {
		params = append(params, "_txlock=immediate")
		pragmas = append(pragmas, "journal_mode(WAL)")
	}
	for _, p := range pragmas {
		params = append(params, "_pragma="+p)
	}
	return path + "?" + strings.Join(params, "&")
}

func openIndex(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", indexDSN(path, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := bootstrap(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return &Index{db: db}, nil
}

func openIndexRead(path string) (*Index, error) {
	db, err := sql.Open("sqlite", indexDSN(path, true))
	if err != nil {
		return nil, err
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

func (ix *Index) IngestBatch(ctx context.Context, source string, sessions []Session, msgs []Message) error {
	tx, err := ix.db.BeginTx(ctx, nil)
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

	updAgg, err := tx.Prepare(`UPDATE sessions
		SET msg_count = COALESCE(msg_count,0) + ?,
		    ended_at  = MAX(COALESCE(ended_at,0), ?)
		WHERE id = ?`)
	if err != nil {
		return err
	}
	defer updAgg.Close()

	bySession := map[string][]Message{}
	for _, m := range msgs {
		bySession[m.SourceID] = append(bySession[m.SourceID], m)
	}

	for _, s := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		pk := source + ":" + s.SourceID

		if s.Append {
			added := 0
			for i, m := range bySession[s.SourceID] {
				if i&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				text := trimText(m.Text)
				if text == "" {
					continue
				}
				if _, err := insFTS.Exec(pk, m.Idx, m.Role, m.TS, text); err != nil {
					return fmt.Errorf("append msg %s/%d: %w", pk, m.Idx, err)
				}
				added++
			}
			if _, err := updAgg.Exec(added, s.EndedAt, pk); err != nil {
				return fmt.Errorf("append agg %s: %w", pk, err)
			}
			continue
		}
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
		for i, m := range bySession[s.SourceID] {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
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

// BulkMode wraps a batch ingest with write-tuning pragmas. `full` distinguishes
// a from-scratch rebuild from a cheap incremental refresh:
//   - full: disable FTS automerge during insert, then run a one-shot `optimize`
//     (rewrites the whole FTS index — O(index size), only acceptable once).
//   - incremental: leave automerge on so new rows merge cheaply, and NEVER
//     optimize — that was rewriting the entire ~200MB index on every run.
func (ix *Index) BulkMode(on, full bool) {
	if on {
		_, _ = ix.db.Exec(`PRAGMA synchronous=OFF`)
		_, _ = ix.db.Exec(`PRAGMA cache_size=-262144`)
		if full {
			_, _ = ix.db.Exec(`INSERT INTO messages_fts(messages_fts, rank) VALUES('automerge', 0)`)
			_, _ = ix.db.Exec(`INSERT INTO sessions_fts(sessions_fts, rank) VALUES('automerge', 0)`)
		}
		return
	}

	if full {
		_, _ = ix.db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('optimize')`)
		_, _ = ix.db.Exec(`INSERT INTO sessions_fts(sessions_fts) VALUES('optimize')`)
	}
	_, _ = ix.db.Exec(`PRAGMA synchronous=NORMAL`)
	_, _ = ix.db.Exec(`PRAGMA cache_size=-65536`)
	if full {
		_, _ = ix.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	} else {
		_, _ = ix.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	}
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

type Hit struct {
	SessionID string  `json:"session_id"`
	Source    string  `json:"source"`
	SourceID  string  `json:"source_id"`
	Project   string  `json:"project"`
	Title     string  `json:"title"`
	StartedAt int64   `json:"started_at_ms"`
	MsgIdx    int     `json:"msg_idx"`
	Role      string  `json:"role"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
}

func (h Hit) StartedTime() time.Time { return time.UnixMilli(h.StartedAt) }

func (ix *Index) Search(query string, opts SearchOpts) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return ix.recent(opts)
	}

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

	bodyWhere := append([]string{}, where...)
	titleWhere := []string{"sessions_fts MATCH ?"}
	titleArgs := []any{ftsQuery}
	for _, w := range where[1:] {
		titleWhere = append(titleWhere, w)
	}
	titleArgs = append(titleArgs, args[1:len(args)-1]...)

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
	allArgs = append(allArgs, args[:len(args)-1]...)
	allArgs = append(allArgs, titleArgs...)
	allArgs = append(allArgs, args[len(args)-1])

	rows, err := ix.db.Query(q, allArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	seen := map[string]int{}
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
	AnyTerm bool
}

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

type StatRow struct {
	Source   string `json:"source"`
	Project  string `json:"project,omitempty"`
	Sessions int    `json:"sessions"`
	Messages int    `json:"messages"`
}

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

func (ix *Index) Related(id string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}

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
