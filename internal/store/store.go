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

// schema creates tables, triggers, and the FTS5 virtual table. Every statement
// is idempotent so reopening an existing database is a no-op.
//
// Indexes deliberately live in schemaIndexes, applied AFTER column migrations:
// an index over a migrated column (inbox.deliver_after) would fail here on an
// upgraded database, because CREATE TABLE IF NOT EXISTS leaves the old table
// untouched and the column does not exist yet. That ordering bug would brick
// startup for every existing install.
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
CREATE TABLE IF NOT EXISTS inbox (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	ts            TEXT    NOT NULL,
	target        TEXT    NOT NULL,
	source        TEXT    NOT NULL,
	sender        TEXT    NOT NULL,
	body          TEXT    NOT NULL,
	delivered     INTEGER NOT NULL DEFAULT 0,
	hop           INTEGER NOT NULL DEFAULT 0,
	deliver_after INTEGER NOT NULL DEFAULT 0,
	attempts      INTEGER NOT NULL DEFAULT 0
);
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
`

// schemaIndexes is applied after column migrations, so indexes may reference
// columns added by those migrations.
const schemaIndexes = `
CREATE INDEX IF NOT EXISTS vectors_dim ON vectors(dim);
CREATE INDEX IF NOT EXISTS inbox_undelivered ON inbox(delivered, deliver_after, id);
CREATE INDEX IF NOT EXISTS token_usage_source ON token_usage(source);
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
	// Idempotent column migrations for DBs created before a column existed.
	// CREATE TABLE IF NOT EXISTS does not add columns to an existing table, so
	// each addition needs an explicit ALTER guarded by a probe.
	for _, m := range columnMigrations {
		if err := addColumnIfMissing(db, m.table, m.column, m.definition); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Indexes last: some cover columns the migrations above just added.
	if _, err := db.Exec(schemaIndexes); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create indexes: %w", err)
	}
	return &Store{db: db}, nil
}

// columnMigrations lists every column added to a table after that table first
// shipped. Order is irrelevant — each entry is independent and idempotent.
//
// A new install gets these columns from `schema` above and every probe here
// succeeds immediately; an upgraded install picks them up by ALTER. Both paths
// converge on the same layout, which is what keeps `schema` readable as the
// authoritative description rather than a historical artifact.
var columnMigrations = []struct{ table, column, definition string }{
	{"inbox", "hop", "INTEGER NOT NULL DEFAULT 0"},
	// deliver_after / attempts back deferred bus delivery: a message for a
	// busy Otto is returned to the queue with a future deliver_after instead
	// of being dropped, and attempts bounds how long that can go on.
	{"inbox", "deliver_after", "INTEGER NOT NULL DEFAULT 0"},
	{"inbox", "attempts", "INTEGER NOT NULL DEFAULT 0"},
}

// addColumnIfMissing probes for a column with a cheap SELECT and adds it when
// absent. The probe is the check rather than pragma table_info because it is
// one statement and fails precisely when the column is missing.
func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	// #nosec — table/column/definition are compile-time constants from
	// columnMigrations, never user input; SQLite cannot parameterize DDL.
	if _, err := db.Exec(fmt.Sprintf(`SELECT %s FROM %s LIMIT 1`, column, table)); err == nil {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition)); err != nil {
		return fmt.Errorf("store: migrate %s.%s: %w", table, column, err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }
