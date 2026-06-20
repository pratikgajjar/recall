-- recall index schema (SQLite + FTS5)
--
-- Source of truth: bootstrap() in index.go. This file is a sqlite3 `.schema`
-- export of a freshly-created index, kept for review/diffing only — recall
-- creates the schema programmatically at runtime, it does NOT read this file.
--
-- Regenerate:
--   go build -o /tmp/recall . && HOME=$(mktemp -d) /tmp/recall index
--   sqlite3 $HOME/.recall/index.sqlite .schema
--
-- schema_version: 3 (see migrateSchema() for upgrade path)
--
-- session_tags is durable user/agent state: ingest never rebuilds it, so
-- tags survive reindex. Tables prefixed messages_fts_/sessions_fts_ are FTS5
-- shadow tables, created and managed by SQLite — never written to directly.

CREATE TABLE sessions (
			id          TEXT PRIMARY KEY,
			source      TEXT NOT NULL,
			source_id   TEXT NOT NULL,
			project     TEXT,
			title       TEXT,
			started_at  INTEGER,
			ended_at    INTEGER,
			msg_count   INTEGER
		);
CREATE INDEX idx_sessions_project_started
			ON sessions(project, started_at DESC);
CREATE INDEX idx_sessions_started
			ON sessions(started_at DESC);
CREATE INDEX idx_sessions_source
			ON sessions(source, started_at DESC);
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE session_tags (
			session_id TEXT NOT NULL,
			tag        TEXT NOT NULL,
			created_at INTEGER,
			PRIMARY KEY (session_id, tag)
		);
CREATE INDEX idx_session_tags_tag ON session_tags(tag);
CREATE VIRTUAL TABLE messages_fts USING fts5(
			session_pk UNINDEXED,
			idx        UNINDEXED,
			role       UNINDEXED,
			text,
			tokenize = 'porter unicode61 remove_diacritics 1'
		)
/* messages_fts(session_pk,idx,role,text) */;
CREATE VIRTUAL TABLE sessions_fts USING fts5(
			session_pk UNINDEXED,
			title,
			project,
			tokenize = 'porter unicode61 remove_diacritics 1'
		)
/* sessions_fts(session_pk,title,project) */;

-- FTS5 shadow tables (auto-managed by SQLite):
CREATE TABLE IF NOT EXISTS 'messages_fts_data'(id INTEGER PRIMARY KEY, block BLOB);
CREATE TABLE IF NOT EXISTS 'messages_fts_idx'(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS 'messages_fts_content'(id INTEGER PRIMARY KEY, c0, c1, c2, c3);
CREATE TABLE IF NOT EXISTS 'messages_fts_docsize'(id INTEGER PRIMARY KEY, sz BLOB);
CREATE TABLE IF NOT EXISTS 'messages_fts_config'(k PRIMARY KEY, v) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS 'sessions_fts_data'(id INTEGER PRIMARY KEY, block BLOB);
CREATE TABLE IF NOT EXISTS 'sessions_fts_idx'(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS 'sessions_fts_content'(id INTEGER PRIMARY KEY, c0, c1, c2);
CREATE TABLE IF NOT EXISTS 'sessions_fts_docsize'(id INTEGER PRIMARY KEY, sz BLOB);
CREATE TABLE IF NOT EXISTS 'sessions_fts_config'(k PRIMARY KEY, v) WITHOUT ROWID;
