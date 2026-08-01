package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
const schemaVersion = "6"

// tailWeightSQL is tailWeight rendered for the bm25() call.
var tailWeightSQL = strconv.FormatFloat(tailWeight, 'f', -1, 64)

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
			model       TEXT,
			tokens_in   INTEGER,
			tokens_out  INTEGER,
			cache_read  INTEGER,
			cache_write INTEGER,
			cost_usd    REAL,
			estimated   INTEGER
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
		// messages_fts.session_pk is UNINDEXED, so deleting a session's rows by
		// that column scans the whole FTS table. A session's messages are inserted
		// consecutively, so their FTS rowids form a contiguous run: record the run
		// and the delete becomes a seek on the FTS primary key instead.
		`CREATE TABLE IF NOT EXISTS msg_ranges (
			session_pk TEXT NOT NULL,
			lo         INTEGER NOT NULL,
			hi         INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_msg_ranges_pk ON msg_ranges(session_pk)`,
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
		// `tail` holds everything past the first excerptMax characters of a
		// message, in windows, as extra rows. It is weighted down at query time
		// (see tailWeight) so that deep text is findable without competing with
		// the opening of a message, which is where the topic is usually stated.
		// Indexing it at equal weight was measured three ways and cost 7-12% of
		// MRR every time.
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			session_pk UNINDEXED,
			idx        UNINDEXED,
			role       UNINDEXED,
			text,
			tail,
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
//   - →v4: add the usage columns (model, tokens, cost). ALTER-ADD is
//     non-destructive, but existing rows have no usage data, so checkpoints
//     are cleared to force one full rescan that backfills them.
//   - →v5: add msg_ranges (created by bootstrap). Pre-v5 sessions have no range
//     recorded; the delete falls back to the old scan for those and records a
//     range on the way through, so the index heals itself as sessions are
//     touched. No migration step and no forced rescan.
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

	// v4 usage columns. bootstrap's CREATE IF NOT EXISTS can't reach an
	// existing table, so add them one by one and re-scan to fill them.
	for _, col := range []struct{ name, decl string }{
		{"model", "TEXT"},
		{"tokens_in", "INTEGER"},
		{"tokens_out", "INTEGER"},
		{"cache_read", "INTEGER"},
		{"cache_write", "INTEGER"},
		{"cost_usd", "REAL"},
		{"estimated", "INTEGER"},
	} {
		has, err := tableHasColumn(db, "sessions", col.name)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col.name + ` ` + col.decl); err != nil {
			return fmt.Errorf("add sessions.%s: %w", col.name, err)
		}
		dirty = true
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

	upsertSess, err := tx.Prepare(`INSERT INTO sessions(id, source, source_id, project, title, started_at, ended_at, msg_count,
			model, tokens_in, tokens_out, cache_read, cache_write, cost_usd, estimated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project=excluded.project,
			title=excluded.title,
			started_at=excluded.started_at,
			ended_at=excluded.ended_at,
			msg_count=excluded.msg_count,
			model=excluded.model,
			tokens_in=excluded.tokens_in,
			tokens_out=excluded.tokens_out,
			cache_read=excluded.cache_read,
			cache_write=excluded.cache_write,
			cost_usd=excluded.cost_usd,
			estimated=excluded.estimated`)
	if err != nil {
		return err
	}
	defer upsertSess.Close()

	// messages_fts.session_pk is UNINDEXED, so "DELETE ... WHERE session_pk = ?"
	// cannot seek — it scans the whole FTS table, once per session. Measured on a
	// cold build of 3,583 sessions that was 110s of a 155s run, against 3.3s to
	// insert all 436k messages. Nothing exists to delete on a first index, so ask
	// the sessions table (which does have a primary key) before paying for it.
	sessionExists, err := tx.Prepare(`SELECT 1 FROM sessions WHERE id = ?`)
	if err != nil {
		return err
	}
	defer sessionExists.Close()

	selRanges, err := tx.Prepare(`SELECT lo, hi FROM msg_ranges WHERE session_pk = ?`)
	if err != nil {
		return err
	}
	defer selRanges.Close()
	delRange, err := tx.Prepare(`DELETE FROM messages_fts WHERE rowid BETWEEN ? AND ?`)
	if err != nil {
		return err
	}
	defer delRange.Close()
	delRangeRows, err := tx.Prepare(`DELETE FROM msg_ranges WHERE session_pk = ?`)
	if err != nil {
		return err
	}
	defer delRangeRows.Close()
	insRange, err := tx.Prepare(`INSERT INTO msg_ranges(session_pk, lo, hi) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer insRange.Close()

	// Fallback for sessions indexed before v5, which have no recorded range.
	delFTS, err := tx.Prepare(`DELETE FROM messages_fts WHERE session_pk = ?`)
	if err != nil {
		return err
	}
	defer delFTS.Close()

	insFTS, err := tx.Prepare(`INSERT INTO messages_fts(session_pk, idx, role, text, tail) VALUES(?,?,?,?,?)`)
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
		SET msg_count   = COALESCE(msg_count,0) + ?,
		    ended_at    = MAX(COALESCE(ended_at,0), ?),
		    model       = COALESCE(NULLIF(?,''), model),
		    tokens_in   = COALESCE(tokens_in,0)   + ?,
		    tokens_out  = COALESCE(tokens_out,0)  + ?,
		    cache_read  = COALESCE(cache_read,0)  + ?,
		    cache_write = COALESCE(cache_write,0) + ?,
		    cost_usd    = COALESCE(cost_usd,0.0)  + ?,
		    estimated   = ?
		WHERE id = ?`)
	if err != nil {
		return err
	}
	defer updAgg.Close()

	// dropSessionRows removes a session's FTS rows. It seeks via the recorded
	// rowid ranges; a session indexed before v5 has none, so it falls back to the
	// scanning delete once and gets a range recorded on reinsert.
	dropSessionRows := func(pk string) error {
		rows, err := selRanges.Query(pk)
		if err != nil {
			return err
		}
		type span struct{ lo, hi int64 }
		var spans []span
		for rows.Next() {
			var sp span
			if err := rows.Scan(&sp.lo, &sp.hi); err != nil {
				rows.Close()
				return err
			}
			spans = append(spans, sp)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(spans) == 0 {
			_, err := delFTS.Exec(pk) // pre-v5 session: no range to seek with
			return err
		}
		for _, sp := range spans {
			if _, err := delRange.Exec(sp.lo, sp.hi); err != nil {
				return err
			}
		}
		_, err = delRangeRows.Exec(pk)
		return err
	}

	// Group by session, and keep one message per (session, idx). An adapter can
	// legitimately emit the same session id twice in a batch — cursor-agent
	// derives the id from a filename, and 7 transcripts exist both standalone and
	// under subagents/ — which otherwise inserts every message twice under the
	// same idx. Later wins: the fuller, more recent copy.
	bySession := map[string][]Message{}
	seenMsg := map[string]int{} // "sid\x00idx" -> position in bySession[sid]
	for _, m := range msgs {
		key := m.SourceID + "\x00" + strconv.Itoa(m.Idx)
		if pos, dup := seenMsg[key]; dup {
			bySession[m.SourceID][pos] = m
			continue
		}
		seenMsg[key] = len(bySession[m.SourceID])
		bySession[m.SourceID] = append(bySession[m.SourceID], m)
	}

	doneSession := map[string]bool{}
	for _, s := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		pk := source + ":" + s.SourceID
		// Same reason: a repeated session in one batch would delete the rows the
		// earlier pass just wrote and insert them again.
		if !s.Append {
			if doneSession[pk] {
				continue
			}
			doneSession[pk] = true
		}

		if s.Append {
			added := 0
			var aLo, aHi int64 = -1, -1
			for i, m := range bySession[s.SourceID] {
				if i&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				head, tails := splitForIndex(m.Text)
				if head == "" {
					continue
				}
				rowsFor := make([][2]string, 0, 1+len(tails))
				rowsFor = append(rowsFor, [2]string{head, ""})
				for _, t := range tails {
					rowsFor = append(rowsFor, [2]string{"", t})
				}
				var res sql.Result
				var err error
				for _, r := range rowsFor {
					res, err = insFTS.Exec(pk, m.Idx, m.Role, r[0], r[1])
					if err != nil {
						return fmt.Errorf("append msg %s/%d: %w", pk, m.Idx, err)
					}
					if rid, e := res.LastInsertId(); e == nil {
						if aLo < 0 {
							aLo = rid
						}
						aHi = rid
					}
				}
				// Appended rows are a separate run from the session's earlier ones;
				// the range is recorded above, or a later delete would leave them
				// orphaned in FTS.
				added++
			}
			if aLo >= 0 {
				if _, err := insRange.Exec(pk, aLo, aHi); err != nil {
					return err
				}
			}
			u := withEstimate(s)
			if _, err := updAgg.Exec(added, s.EndedAt, u.Model,
				u.TokensIn, u.TokensOut, u.CacheRead, u.CacheWrite, u.CostUSD, u.Estimated,
				pk); err != nil {
				return fmt.Errorf("append agg %s: %w", pk, err)
			}
			continue
		}
		// Check before the upsert creates the row.
		var one int
		hadRows := sessionExists.QueryRow(pk).Scan(&one) == nil

		u := withEstimate(s)
		if _, err := upsertSess.Exec(pk, source, s.SourceID, s.Project, s.Title,
			s.StartedAt, s.EndedAt, s.MsgCount,
			u.Model, u.TokensIn, u.TokensOut, u.CacheRead, u.CacheWrite, u.CostUSD, u.Estimated); err != nil {
			return fmt.Errorf("upsert session %s: %w", pk, err)
		}
		if hadRows {
			if err := dropSessionRows(pk); err != nil {
				return err
			}
			if _, err := delSessFTS.Exec(pk); err != nil {
				return err
			}
		}
		if _, err := insSessFTS.Exec(pk, s.Title, s.Project); err != nil {
			return fmt.Errorf("insert session_fts %s: %w", pk, err)
		}
		var lo, hi int64 = -1, -1
		for i, m := range bySession[s.SourceID] {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			head, tails := splitForIndex(m.Text)
			if head == "" {
				continue
			}
			rowsFor := make([][2]string, 0, 1+len(tails))
			rowsFor = append(rowsFor, [2]string{head, ""})
			for _, t := range tails {
				rowsFor = append(rowsFor, [2]string{"", t})
			}
			for _, r := range rowsFor {
				res, err := insFTS.Exec(pk, m.Idx, m.Role, r[0], r[1])
				if err != nil {
					return fmt.Errorf("insert msg %s/%d: %w", pk, m.Idx, err)
				}
				// Rows for one session go in consecutively, so the run is [first,last].
				if rid, err := res.LastInsertId(); err == nil {
					if lo < 0 {
						lo = rid
					}
					hi = rid
				}
			}
		}
		if lo >= 0 {
			if _, err := insRange.Exec(pk, lo, hi); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// charsPerToken is the fallback ratio for sources that report no usage.
// Calibrated against Cursor's contextTokensUsed over 40 real sessions
// (median 2.7 chars/token) and the common BPE rule of thumb (~4 for prose,
// ~2.5 for code); agent transcripts are code-heavy, so 3 splits the
// difference. Only ever a ballpark — rows derived this way carry
// Estimated=true so callers can mark them.
//
// ponytail: one global ratio, not per-model tokenizers. Swap in tiktoken
// only if someone needs billing-grade numbers from an unreported source.
const charsPerToken = 3

// withEstimate fills in a token estimate for sources that report no usage.
// A source that reported anything real is returned untouched.
func withEstimate(s Session) Session {
	if s.TokensIn+s.TokensOut+s.CacheRead+s.CacheWrite > 0 || s.Chars <= 0 {
		return s
	}
	s.TokensIn = s.Chars / charsPerToken
	s.Estimated = true
	return s
}

// toolArgSuffix matches the clipped argument an adapter appends after a
// tool-call marker: "[tool:bash] git log --oneline" -> "[tool:bash]".
var toolArgSuffix = regexp.MustCompile(`(?m)^(\[(?:tool|tool_use):[^\]]+\]|\[call [^\]]+\]) .*$`)

// trimText prepares a message for the search index. Transcripts render from the
// adapter's Fetch, so this affects search only.
//
// The tool-call argument is dropped here while staying visible in a rendered
// transcript. Indexing it is actively harmful: measured against the sessions
// agents actually went on to read, indexing command text collapsed rank@1 from
// 10 to 1 and halved MRR, because a shell command shares enough vocabulary with
// an unrelated query to win the AND pass and crowd out the right session.
// Only the argument is stripped, never the marker itself — 13% of messages are
// nothing but a marker, and removing those entirely drops them from the index.
// splitForIndex divides a message into the opening that is indexed normally and
// the windows of everything after it.
//
// Truncating at excerptMax left 44% of all human and model prose unsearchable:
// only ~5% of parts are long enough to cut, but they carry nearly half the
// characters, because long messages are where the substance is. Indexing all of
// it at equal weight was tried three ways — a bigger cap, and windows 3 and 40
// deep — and cost 7% to 12.5% of MRR every time: to a two-word query the tail of
// a long message is mostly noise, and it pushes the right session down.
//
// So the opening keeps its place and the rest goes into a column the ranking
// discounts. Windows overlap so a phrase across a boundary survives whole.
func splitForIndex(s string) (head string, tail []string) {
	s = toolArgSuffix.ReplaceAllString(s, "$1")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) <= excerptMax {
		return s, nil
	}
	cut := boundary(s, 0, excerptMax)
	head = strings.TrimSpace(s[:cut])
	// Repetitive text — a progress bar, a log line retried fifty times — yields
	// windows identical to each other. They add no searchable content, only
	// rows and term-frequency noise.
	seen := map[string]bool{head: true}
	emit := func(w string) {
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		tail = append(tail, w)
	}
	for start := cut - chunkOverlap; start < len(s) && len(tail) < maxChunks; {
		for start > 0 && !utf8.RuneStart(s[start]) {
			start--
		}
		end := start + excerptMax
		if end >= len(s) {
			emit(strings.TrimSpace(s[start:]))
			break
		}
		e := boundary(s, start, end)
		emit(strings.TrimSpace(s[start:e]))
		next := e - chunkOverlap
		if next <= start {
			next = e
		}
		start = next
	}
	return head, tail
}

// boundary returns a cut point at or before end that falls on a word break and
// never inside a rune, so tokens are not sliced in half.
func boundary(s string, start, end int) int {
	if end >= len(s) {
		return len(s)
	}
	cut := end
	if i := strings.LastIndexAny(s[start:end], " \n\t"); i > excerptMax-chunkSnap {
		cut = start + i
	}
	for cut > start && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}

func trimText(s string) string {
	s = toolArgSuffix.ReplaceAllString(s, "$1")
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
	// An unfiltered search can use FTS5's top-n fast path; a filtered one wants
	// SQLite to push the filter into the scan instead. They need different
	// query shapes, and using either shape for both is a 10x loss one way or
	// the other.
	if !ix.filtered(opts) {
		return ix.searchPooled(ftsQuery, opts)
	}

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

	// A title match says "this session is relevant". Scoped to one session that
	// is already known, so the row is pure noise: it occupies a hit slot and, with
	// --context, an expansion budget, while pointing at no message (idx -1).
	titleUnion := `
  UNION ALL
  SELECT s.id, s.source, s.source_id,
         COALESCE(s.project,''), COALESCE(s.title,''),
         COALESCE(s.started_at,0),
         -1 AS idx, 'title' AS role,
         '['||snippet(sessions_fts, 1, '«', '»', '…', 12)||']' AS snippet,
         bm25(sessions_fts) * 1.4 AS rank   -- title hits get a small boost
    FROM sessions_fts JOIN sessions s ON s.id = sessions_fts.session_pk
   WHERE ` + strings.Join(titleWhere, " AND ")
	if opts.SessionID != "" {
		titleUnion = ""
		titleArgs = nil
	}

	q := `
SELECT id, source, source_id, project, title, started_at, idx, role, snippet, rank FROM (
  SELECT s.id AS id, s.source AS source, s.source_id AS source_id,
         COALESCE(s.project,'') AS project, COALESCE(s.title,'') AS title,
         COALESCE(s.started_at,0) AS started_at,
         f.idx AS idx, f.role AS role,
         snippet(messages_fts, -1, '«', '»', '…', 12) AS snippet,
         bm25(messages_fts, 0.0, 0.0, 0.0, 1.0, ` + tailWeightSQL + `) AS rank
    FROM messages_fts f JOIN sessions s ON s.id = f.session_pk
   WHERE ` + strings.Join(bodyWhere, " AND ") + titleUnion + `
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
	return ix.scanHits(rows, opts)
}

// scanHits reads result rows and collapses them the way the caller expects.
func (ix *Index) scanHits(rows *sql.Rows, opts SearchOpts) ([]Hit, error) {
	hits, _, err := ix.scanHitsCounted(rows, opts)
	return hits, err
}

// scanHitsCounted also reports how many rows it read, so a caller can tell a
// short result from a truncated one.
func (ix *Index) scanHitsCounted(rows *sql.Rows, opts SearchOpts) ([]Hit, int, error) {
	var hits []Hit
	scanned := 0
	seen := map[string]int{}
	for rows.Next() {
		scanned++
		var h Hit
		if err := rows.Scan(&h.SessionID, &h.Source, &h.SourceID, &h.Project, &h.Title,
			&h.StartedAt, &h.MsgIdx, &h.Role, &h.Snippet, &h.Rank); err != nil {
			return nil, 0, err
		}
		// Collapsing to one hit per session is right when ranking sessions against
		// each other. Inside a single session it is wrong: the caller has already
		// chosen the session and is asking *where* in it — every match is a distinct
		// location, and reporting one of five leaves them guessing at the rest.
		key := h.SessionID
		if opts.SessionID != "" {
			key = fmt.Sprintf("%s#%d", h.SessionID, h.MsgIdx)
		}
		if prev, ok := seen[key]; ok {
			if h.Rank < hits[prev].Rank {
				hits[prev] = h
			}
			continue
		}
		seen[key] = len(hits)
		hits = append(hits, h)
	}
	return hits, scanned, rows.Err()
}

// filtered reports whether the caller narrowed the search to a subset of
// sessions. Such a search is already cheap — restricting to a session or a repo
// cuts the candidate set before ranking — and it cannot be pooled, because the
// best rows overall may contain none from the subset asked for.
func (ix *Index) filtered(opts SearchOpts) bool {
	return opts.SessionID != "" || opts.Source != "" || opts.Project != "" ||
		opts.After > 0 || opts.Before > 0 || len(opts.Tags) > 0
}

// searchPooled answers an unfiltered search without materialising every match.
//
// FTS5 resolves `ORDER BY rank LIMIT n` with a top-n heap. Ordering by an
// expression instead — the recency tiebreak — defeats that, so every matching
// row has to be built and sorted: 112,000 of them for a query like "post
// engineering channel staging is free taking". That was search p95 1,500ms
// against a 21ms median.
//
// Take a pool of the best rows by pure bm25, which FTS5 does cheaply, and apply
// the tiebreak to those. The tiebreak only ever reorders near-ties, so the top
// of the ranking is the only place it was doing anything: across 223 real
// searches this returns the identical result order.
func (ix *Index) searchPooled(ftsQuery string, opts SearchOpts) ([]Hit, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	}
	pool := limit * poolFactor
	if pool < poolMin {
		pool = poolMin
	}
	q := `
SELECT id, source, source_id, project, title, started_at, idx, role, snippet, rank FROM (
  SELECT s.id AS id, s.source AS source, s.source_id AS source_id,
         COALESCE(s.project,'') AS project, COALESCE(s.title,'') AS title,
         COALESCE(s.started_at,0) AS started_at,
         t.idx AS idx, t.role AS role, t.snippet AS snippet, t.rank AS rank
    FROM (SELECT f.session_pk AS pk, f.idx AS idx, f.role AS role,
                 snippet(messages_fts, -1, '«', '»', '…', 12) AS snippet,
                 bm25(messages_fts, 0.0, 0.0, 0.0, 1.0, ` + tailWeightSQL + `) AS rank
            FROM messages_fts f
           WHERE messages_fts MATCH ?
           ORDER BY rank LIMIT ?) t
    JOIN sessions s ON s.id = t.pk
  UNION ALL
  SELECT s.id, s.source, s.source_id,
         COALESCE(s.project,''), COALESCE(s.title,''),
         COALESCE(s.started_at,0),
         -1 AS idx, 'title' AS role, t.snippet, t.rank
    FROM (SELECT sessions_fts.session_pk AS pk,
                 '['||snippet(sessions_fts, 1, '«', '»', '…', 12)||']' AS snippet,
                 bm25(sessions_fts) * 1.4 AS rank   -- title hits get a small boost
            FROM sessions_fts
           WHERE sessions_fts MATCH ?
           ORDER BY rank LIMIT ?) t
    JOIN sessions s ON s.id = t.pk
)
ORDER BY (rank * 1.0) - (started_at / 1.0e13) ASC
LIMIT ?`
	// Rows collapse to one hit per session, and a pool can be dominated by a
	// handful of talkative sessions: a search for 15 came back with 6 while 30
	// other sessions matched every term. When that happens, look deeper rather
	// than under-deliver — the caller asked for more and more exists.
	for attempt := 0; ; attempt++ {
		rows, err := ix.db.Query(q, ftsQuery, pool, ftsQuery, pool, pool)
		if err != nil {
			return nil, err
		}
		hits, scanned, err := ix.scanHitsCounted(rows, opts)
		rows.Close()
		if err != nil {
			return nil, err
		}
		// Stop when the page is full, when the matches ran out (a short scan
		// means there was nothing more to read), or when digging further stops
		// being worth the scan.
		if len(hits) >= limit || scanned < pool || attempt >= poolGrowthSteps {
			if len(hits) > limit {
				hits = hits[:limit]
			}
			return hits, nil
		}
		pool *= poolGrowthFactor
	}
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

	Tokens    int64   `json:"tokens"`
	CacheRead int64   `json:"cache_read"`
	CostUSD   float64 `json:"cost_usd"`
	// Estimated is true when no session in the group reported real usage.
	Estimated bool `json:"estimated,omitempty"`
}

// ModelRow is usage rolled up by model rather than by project — the "what did
// I actually spend it on" view.
type ModelRow struct {
	Model     string  `json:"model"`
	Source    string  `json:"source"`
	Sessions  int     `json:"sessions"`
	Tokens    int64   `json:"tokens"`
	CacheRead int64   `json:"cache_read"`
	CostUSD   float64 `json:"cost_usd"`
	Estimated bool    `json:"estimated,omitempty"`
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
	q := `SELECT source, COALESCE(project,''), COUNT(*), COALESCE(SUM(msg_count),0),
	             COALESCE(SUM(tokens_in + tokens_out + cache_read + cache_write),0),
	             COALESCE(SUM(cache_read),0),
	             COALESCE(SUM(cost_usd),0),
	             MIN(COALESCE(estimated,1))
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
		var est int
		if err := rows.Scan(&r.Source, &r.Project, &r.Sessions, &r.Messages,
			&r.Tokens, &r.CacheRead, &r.CostUSD, &est); err != nil {
			return nil, err
		}
		r.Estimated = est == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ModelStats rolls usage up by model. Sessions whose source never records a
// model still land here under "(unknown)" — dropping them hid real usage
// (every codex session, ~771M tokens) from a table that is supposed to
// account for all of it.
func (ix *Index) ModelStats(opts SearchOpts) ([]ModelRow, error) {
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
	q := `SELECT COALESCE(NULLIF(model,''),'(unknown)') AS m, source, COUNT(*),
	             COALESCE(SUM(tokens_in + tokens_out + cache_read + cache_write),0),
	             COALESCE(SUM(cache_read),0),
	             COALESCE(SUM(cost_usd),0),
	             MIN(COALESCE(estimated,1))
	        FROM sessions WHERE ` + strings.Join(where, " AND ") + `
	    GROUP BY m, source ORDER BY 6 DESC, 4 DESC`
	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelRow
	for rows.Next() {
		var r ModelRow
		var est int
		if err := rows.Scan(&r.Model, &r.Source, &r.Sessions,
			&r.Tokens, &r.CacheRead, &r.CostUSD, &est); err != nil {
			return nil, err
		}
		r.Estimated = est == 1
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
	err := row.Scan(&s.Source, &s.SourceID, &s.Project, &s.Title,
		&s.StartedAt, &s.EndedAt, &s.MsgCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ix.missingSessionError(id)
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// missingSessionError turns "sql: no rows in result set" into something a
// caller can act on. A raw driver error tells an agent nothing about whether it
// mistyped the id, the session was pruned, or the id came from another machine
// — so it retries blindly. Truncated ids are the common case in practice, and
// a unique prefix match can simply name the one that was meant.
func (ix *Index) missingSessionError(id string) error {
	// Try the whole id as a prefix first (plain truncation), then a shorter one.
	// A dropped character in the middle is just as common as a lost tail, and
	// both leave enough of the head to identify the session uniquely.
	for _, n := range []int{len(id), 24} {
		if n > len(id) {
			continue
		}
		matches := ix.idsWithPrefix(id[:n], 2)
		if len(matches) == 1 && matches[0] != id {
			return fmt.Errorf("session %s not found — did you mean %s? (ids differ after %q)",
				id, matches[0], id[:n])
		}
		if len(matches) > 1 {
			return fmt.Errorf("session %s not found — %s and at least one other id start with %q; pass the full id",
				id, matches[0], id[:n])
		}
	}
	return fmt.Errorf("session %s not found — it may have been deleted at the source, "+
		"or the id is from another machine. Find it with: recall find <query>", id)
}

// idsWithPrefix returns up to limit session ids starting with prefix.
func (ix *Index) idsWithPrefix(prefix string, limit int) []string {
	rows, err := ix.db.Query(`SELECT id FROM sessions WHERE id LIKE ? || '%' LIMIT ?`, prefix, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if rows.Scan(&m) == nil {
			out = append(out, m)
		}
	}
	return out
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

// ForgetSession removes one session and its FTS rows. It is called only when an
// adapter has *proved* the session is gone (its source is available, but the
// session is not), so the index converges on the truth instead of accumulating
// hits that error when an agent opens them. Tags are deliberately left alone:
// they are durable user data keyed on the id, and the session may come back.
func (ix *Index) ForgetSession(id string) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM messages_fts WHERE session_pk = ?`,
		`DELETE FROM sessions_fts WHERE session_pk = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneMissing removes sessions of one source that a completed full scan never
// emitted — they exist in the index but no longer in the tool that wrote them.
// Callers must only pass the result of a *clean, full* scan of an *available*
// source; anything less and "not seen" means "not looked at", which would delete
// a working index. Tags are left in place: they are user data keyed on the id.
func (ix *Index) PruneMissing(source string, seen map[string]bool) (int, error) {
	if len(seen) == 0 {
		return 0, nil // a scan that emitted nothing proves nothing
	}
	rows, err := ix.db.Query(`SELECT source_id FROM sessions WHERE source = ?`, source)
	if err != nil {
		return 0, err
	}
	var gone []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[sid] {
			gone = append(gone, sid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, sid := range gone {
		if err := ix.ForgetSession(source + ":" + sid); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}
