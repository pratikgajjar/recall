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
	// A read handle is query_only, so it can't run migrateSchema (which DROPs/
	// ALTERs). Guard instead: a stale on-disk schema would make queries return
	// wrong data (e.g. snippet column indices shift), so refuse rather than lie.
	// Write opens (index, mcp) migrate on open, so this only trips after a binary
	// upgrade before the first reindex.
	var v string
	_ = db.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&v)
	if v != schemaVersion {
		db.Close()
		have := v
		if have == "" {
			have = "legacy"
		}
		return nil, fmt.Errorf("index schema is outdated (%s, want v%s) — run `recall index` to rebuild", have, schemaVersion)
	}
	return &Index{db: db}, nil
}

// schemaVersion bumps when the on-disk schema changes in a way bootstrap can't
// reach via CREATE IF NOT EXISTS. Each bump triggers a one-shot migration in
// migrateSchema() (drop legacy FTS, drop dead columns, clear checkpoints).
const schemaVersion = "3"

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
			msg_count   INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project_started
			ON sessions(project, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_started
			ON sessions(started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_source
			ON sessions(source, started_at DESC)`,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)`,
		// session_tags holds user/agent-authored labels. It is durable state:
		// ingest and migrateSchema NEVER touch it, so tags survive every reindex
		// (keyed on the stable session id, source:source_id).
		`CREATE TABLE IF NOT EXISTS session_tags (
			session_id TEXT NOT NULL,
			tag        TEXT NOT NULL,
			created_at INTEGER,
			PRIMARY KEY (session_id, tag)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tags_tag ON session_tags(tag)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	if err := migrateSchema(db); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	ftsStmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			session_pk UNINDEXED,
			idx        UNINDEXED,
			role       UNINDEXED,
			text,
			tokenize = 'porter unicode61 remove_diacritics 1'
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
			session_pk UNINDEXED,
			title,
			project,
			tokenize = 'porter unicode61 remove_diacritics 1'
		)`,
	}
	for _, s := range ftsStmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

// migrateSchema brings legacy on-disk schemas up to the current version.
//
// History:
//   - →v2: drop dead columns (messages_fts.ts, sessions.meta). FTS tables
//     can't be ALTERed, so drop+recreate and clear adapter checkpoints; the
//     next index run rebuilds FTS rows from source transcripts.
//   - →v3: add session_tags (created by bootstrap, non-destructive). The bump
//     just re-stamps the version; no migration step needed here.
func migrateSchema(db *sql.DB) error {
	var version string
	_ = db.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&version)
	if version == schemaVersion {
		return nil
	}

	dirty := false
	legacy, err := tableHasColumn(db, "messages_fts", "ts")
	if err != nil {
		return err
	}
	if legacy {
		if _, err := db.Exec(`DROP TABLE messages_fts`); err != nil {
			return err
		}
		dirty = true
	}
	legacy, err = tableHasColumn(db, "sessions", "meta")
	if err != nil {
		return err
	}
	if legacy {
		if _, err := db.Exec(`ALTER TABLE sessions DROP COLUMN meta`); err != nil {
			return fmt.Errorf("drop sessions.meta: %w", err)
		}
	}
	if dirty {
		// FTS was wiped — force a full reindex by clearing per-adapter checkpoints.
		if _, err := db.Exec(`DELETE FROM meta WHERE k LIKE 'ckpt:%'`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(
		`INSERT INTO meta(k,v) VALUES('schema_version',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		schemaVersion); err != nil {
		return err
	}
	return nil
}

// tableHasColumn returns true if the named column exists on the table. Works
// for ordinary tables and FTS5 virtual tables (which expose schema via
// pragma_table_info).
func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
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

	upsertSess, err := tx.Prepare(`INSERT INTO sessions(id, source, source_id, project, title, started_at, ended_at, msg_count)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project=excluded.project,
			title=excluded.title,
			started_at=excluded.started_at,
			ended_at=excluded.ended_at,
			msg_count=excluded.msg_count`)
	if err != nil {
		return err
	}
	defer upsertSess.Close()

	delFTS, err := tx.Prepare(`DELETE FROM messages_fts WHERE session_pk = ?`)
	if err != nil {
		return err
	}
	defer delFTS.Close()

	insFTS, err := tx.Prepare(`INSERT INTO messages_fts(session_pk, idx, role, text) VALUES(?,?,?,?)`)
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
				if _, err := insFTS.Exec(pk, m.Idx, m.Role, text); err != nil {
					return fmt.Errorf("append msg %s/%d: %w", pk, m.Idx, err)
				}
				added++
			}
			if _, err := updAgg.Exec(added, s.EndedAt, pk); err != nil {
				return fmt.Errorf("append agg %s: %w", pk, err)
			}
			continue
		}
		if _, err := upsertSess.Exec(pk, source, s.SourceID, s.Project, s.Title,
			s.StartedAt, s.EndedAt, s.MsgCount); err != nil {
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
			if _, err := insFTS.Exec(pk, m.Idx, m.Role, text); err != nil {
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
	terms := ftsTerms(query)
	// AND first: cheap and precise. Only when nothing matches all terms does
	// the expensive any-of-these OR pass run (keyword dumps, partial recall).
	hits, err := ix.searchMatch(strings.Join(terms, " "), opts)
	if err == nil && len(hits) == 0 && len(terms) > 1 {
		hits, err = ix.searchMatch(strings.Join(terms, " OR "), opts)
	}
	return hits, err
}

func (ix *Index) searchMatch(ftsQuery string, opts SearchOpts) ([]Hit, error) {

	args := []any{ftsQuery}
	where := []string{"messages_fts MATCH ?"}
	if opts.SessionID != "" {
		where = append(where, "s.id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.Source != "" {
		where = append(where, "s.source = ?")
		args = append(args, opts.Source)
	}
	if opts.Project != "" {
		where = append(where, "(s.project = ? OR s.project LIKE ? || '/%')")
		args = append(args, opts.Project, opts.Project)
	}
	if opts.After > 0 {
		where = append(where, "s.started_at >= ?")
		args = append(args, opts.After)
	}
	if opts.Before > 0 {
		where = append(where, "s.started_at < ?")
		args = append(args, opts.Before)
	}
	if clause, tagArgs := tagFilterClause("s.id", opts.Tags); clause != "" {
		where = append(where, clause)
		args = append(args, tagArgs...)
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
         snippet(messages_fts, 3, '«', '»', '…', 12) AS snippet,
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
	if opts.SessionID != "" {
		where = append(where, "id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.Source != "" {
		where = append(where, "source = ?")
		args = append(args, opts.Source)
	}
	if opts.Project != "" {
		where = append(where, "(project = ? OR project LIKE ? || '/%')")
		args = append(args, opts.Project, opts.Project)
	}
	if opts.After > 0 {
		where = append(where, "started_at >= ?")
		args = append(args, opts.After)
	}
	if opts.Before > 0 {
		where = append(where, "started_at < ?")
		args = append(args, opts.Before)
	}
	if clause, tagArgs := tagFilterClause("id", opts.Tags); clause != "" {
		where = append(where, clause)
		args = append(args, tagArgs...)
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
	Source    string
	Project   string
	After     int64 // started_at >= After (lower bound; --since is an alias)
	Before    int64 // started_at <  Before (upper bound; for keyset paging)
	Limit     int
	Tags      []string // session must carry ALL of these tags (AND)
	SessionID string   // restrict to one session (search inside a transcript)
}

// tagFilterClause builds a WHERE fragment restricting idCol to sessions that
// carry every tag in tags (AND semantics). Returns "" when no tags are set.
// idCol is the qualified id column for the surrounding query ("s.id" or "id").
func tagFilterClause(idCol string, tags []string) (string, []any) {
	norm := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		n := normalizeTag(t)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		norm = append(norm, n)
	}
	if len(norm) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(norm)), ",")
	clause := idCol + ` IN (SELECT session_id FROM session_tags WHERE tag IN (` +
		placeholders + `) GROUP BY session_id HAVING COUNT(DISTINCT tag) = ?)`
	args := make([]any, 0, len(norm)+1)
	for _, n := range norm {
		args = append(args, n)
	}
	args = append(args, len(norm))
	return clause, args
}

// ftsTerms splits a query into FTS5-safe quoted units. User-quoted spans stay
// exact phrases; bare words become individual terms.
func ftsTerms(q string) []string {
	var out []string
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch r {
			case '"', '*', ':', '(', ')':
				return -1
			}
			return r
		}, s)
	}
	for i, span := range strings.Split(q, `"`) {
		if i%2 == 1 { // inside user quotes
			if p := strings.TrimSpace(clean(span)); p != "" {
				out = append(out, `"`+p+`"`)
			}
			continue
		}
		for _, f := range strings.Fields(span) {
			if f = clean(f); f != "" {
				out = append(out, `"`+f+`"`)
			}
		}
	}
	if len(out) == 0 {
		out = append(out, `""`)
	}
	return out
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
		where = append(where, "(project = ? OR project LIKE ? || '/%')")
		args = append(args, opts.Project, opts.Project)
	}
	if opts.After > 0 {
		where = append(where, "started_at >= ?")
		args = append(args, opts.After)
	}
	if opts.Before > 0 {
		where = append(where, "started_at < ?")
		args = append(args, opts.Before)
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
	hits, err := ix.Search(query, SearchOpts{Limit: limit + 1})
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
	row := ix.db.QueryRow(`SELECT source, source_id, COALESCE(project,''), COALESCE(title,''),
		COALESCE(started_at,0), COALESCE(ended_at,0), COALESCE(msg_count,0)
		FROM sessions WHERE id = ?`, id)
	if err := row.Scan(&s.Source, &s.SourceID, &s.Project, &s.Title,
		&s.StartedAt, &s.EndedAt, &s.MsgCount); err != nil {
		return nil, err
	}
	return &s, nil
}

// ─── Tags ─────────────────────────────────────────────────────────────────
//
// Tags are durable user/agent state, decoupled from the disposable FTS index:
// AddTags/RemoveTags write session_tags, which ingest never rebuilds. A tagged
// session keeps its tags across `recall index --full`.

type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// sessionExists reports whether a session id is present in the index. Tagging a
// non-indexed id is almost always a typo, so callers reject it up front.
func (ix *Index) sessionExists(id string) (bool, error) {
	var n int
	if err := ix.db.QueryRow(`SELECT 1 FROM sessions WHERE id = ? LIMIT 1`, id).Scan(&n); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// AddTags attaches one or more tags to a session. Tags are normalised (trimmed,
// lower-cased); duplicates and blanks are skipped. Returns how many new tags
// were stored (already-present tags don't count). Errors if the session is not
// indexed.
func (ix *Index) AddTags(sessionID string, tags []string) (int, error) {
	ok, err := ix.sessionExists(sessionID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("session %q is not indexed (run `recall index`?)", sessionID)
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO session_tags(session_id, tag, created_at) VALUES(?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	added := 0
	for _, raw := range tags {
		tag := normalizeTag(raw)
		if tag == "" {
			continue
		}
		res, err := stmt.Exec(sessionID, tag, now)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// RemoveTags detaches tags from a session. Returns how many rows were removed.
func (ix *Index) RemoveTags(sessionID string, tags []string) (int, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`DELETE FROM session_tags WHERE session_id = ? AND tag = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	removed := 0
	for _, raw := range tags {
		tag := normalizeTag(raw)
		if tag == "" {
			continue
		}
		res, err := stmt.Exec(sessionID, tag)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed++
		}
	}
	return removed, tx.Commit()
}

// SessionTags returns the tags on one session, alphabetically.
func (ix *Index) SessionTags(sessionID string) ([]string, error) {
	rows, err := ix.db.Query(`SELECT tag FROM session_tags WHERE session_id = ? ORDER BY tag`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllTags returns every tag with its session count, most-used first.
func (ix *Index) AllTags() ([]TagCount, error) {
	rows, err := ix.db.Query(`SELECT tag, COUNT(*) FROM session_tags
		GROUP BY tag ORDER BY 2 DESC, 1 ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// normalizeTag canonicalises a tag: trimmed, lower-cased, inner whitespace
// collapsed to single dashes so `Deploy RCA` and `deploy-rca` are one tag.
func normalizeTag(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), "-")
}
