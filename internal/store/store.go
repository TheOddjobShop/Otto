// Package store persists Otto's conversation turns in SQLite and provides
// FTS5 keyword search over them (the session_search retrieval primitive).
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database holding the append-only turn log and its
// FTS5 search mirror.
type Store struct {
	db *sql.DB
}

// schema is run on every Open; every statement is idempotent so reopening an
// existing database is a no-op.
const schema = `
CREATE TABLE IF NOT EXISTS turns (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	persona TEXT    NOT NULL,
	role    TEXT    NOT NULL,
	content TEXT    NOT NULL,
	ts      INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS turns_fts
	USING fts5(content, content='turns', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS turns_ai AFTER INSERT ON turns BEGIN
	INSERT INTO turns_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS turns_ad AFTER DELETE ON turns BEGIN
	INSERT INTO turns_fts(turns_fts, rowid, content) VALUES ('delete', old.id, old.content);
END;
CREATE TRIGGER IF NOT EXISTS turns_au AFTER UPDATE ON turns BEGIN
	INSERT INTO turns_fts(turns_fts, rowid, content) VALUES ('delete', old.id, old.content);
	INSERT INTO turns_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TABLE IF NOT EXISTS vectors (
	turn_id INTEGER PRIMARY KEY REFERENCES turns(id) ON DELETE CASCADE,
	model   TEXT    NOT NULL,
	dim     INTEGER NOT NULL,
	vec     BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS vectors_dim ON vectors(dim);
CREATE TABLE IF NOT EXISTS inbox (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	ts        TEXT    NOT NULL,
	target    TEXT    NOT NULL,
	source    TEXT    NOT NULL,
	sender    TEXT    NOT NULL,
	body      TEXT    NOT NULL,
	delivered INTEGER NOT NULL DEFAULT 0,
	hop       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS inbox_undelivered ON inbox(delivered, id);
CREATE TABLE IF NOT EXISTS token_usage (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	source          TEXT    NOT NULL,
	model           TEXT    NOT NULL,
	input_tokens    INTEGER NOT NULL,
	output_tokens   INTEGER NOT NULL,
	cache_creation  INTEGER NOT NULL,
	cache_read      INTEGER NOT NULL,
	ts              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS token_usage_source ON token_usage(source);
CREATE TABLE IF NOT EXISTS activity (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	ts       INTEGER NOT NULL,
	persona  TEXT    NOT NULL,
	turn_key TEXT    NOT NULL,
	kind     TEXT    NOT NULL,
	tool     TEXT    NOT NULL,
	detail   TEXT    NOT NULL,
	is_error INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS activity_turn ON activity(turn_key, id);
`

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema is present. WAL + a busy timeout let Otto's multiple goroutines share
// the handle without "database is locked" errors.
func Open(path string) (*Store, error) {
	// foreign_keys(1) makes the vectors→turns ON DELETE CASCADE live, so a
	// future turn-deletion path cleans up orphaned embeddings automatically.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	// Idempotent column migration: older DBs predate the inbox.hop column.
	// Sniff with a SELECT; if the column is missing, ALTER. Re-running on a
	// DB that already has the column hits the SELECT happy path and skips.
	if _, err := db.Exec(`SELECT hop FROM inbox LIMIT 1`); err != nil {
		if _, aerr := db.Exec(`ALTER TABLE inbox ADD COLUMN hop INTEGER NOT NULL DEFAULT 0`); aerr != nil {
			db.Close()
			return nil, fmt.Errorf("store: migrate inbox.hop: %w", aerr)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }
