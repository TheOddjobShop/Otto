package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Turn is one logged exchange row.
type Turn struct {
	ID      int64
	Persona string // "otto" | "toto" | "toot"
	Role    string // "user" | "assistant"
	Content string
	TS      time.Time
}

// AppendTurn inserts one turn and returns its row id. The AFTER INSERT trigger
// keeps the FTS5 mirror in sync automatically.
func (s *Store) AppendTurn(ctx context.Context, persona, role, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO turns(persona, role, content, ts) VALUES (?, ?, ?, ?)`,
		persona, role, content, time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("store: append turn: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: append turn: last insert id: %w", err)
	}
	return id, nil
}

// SearchFTS runs an FTS5 keyword search over logged turns, most-relevant first.
// The raw user query is converted into a single FTS5 phrase so arbitrary
// punctuation (error codes, quotes, parens) can never produce a syntax error.
// A blank query returns no rows.
func (s *Store) SearchFTS(ctx context.Context, query string, limit int) ([]Turn, error) {
	q := ftsPhrase(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.persona, t.role, t.content, t.ts
		FROM turns_fts f
		JOIN turns t ON t.id = f.rowid
		WHERE turns_fts MATCH ?
		ORDER BY rank
		LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var tr Turn
		var ts int64
		if err := rows.Scan(&tr.ID, &tr.Persona, &tr.Role, &tr.Content, &ts); err != nil {
			return nil, fmt.Errorf("store: scan: %w", err)
		}
		tr.TS = time.Unix(ts, 0)
		out = append(out, tr)
	}
	return out, rows.Err()
}

// PruneTurns deletes the oldest turns beyond the keep most-recent rows,
// returning the count of rows removed. A keep value ≤ 0 is a no-op.
//
// The DELETE fires the turns_ad trigger (added alongside turns_ai) which
// removes the corresponding FTS5 entries so keyword search never returns
// ghost results for pruned turns. The vectors → turns ON DELETE CASCADE
// foreign key removes the matching embedding rows automatically, so a single
// PruneTurns call keeps all three tables (turns, turns_fts, vectors) in sync
// without additional bookkeeping.
//
// PruneTurns is safe to call from a background goroutine while the main
// message loop is running; the WAL journal allows concurrent readers.
func (s *Store) PruneTurns(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM turns WHERE id NOT IN (
			SELECT id FROM turns ORDER BY id DESC LIMIT ?
		)`, keep)
	if err != nil {
		return 0, fmt.Errorf("store: prune turns: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune turns: rows affected: %w", err)
	}
	return n, nil
}

// ftsPhrase converts a raw user query into a safe FTS5 MATCH expression.
// Each whitespace-separated token becomes its own quoted phrase (with embedded
// double quotes doubled, FTS5's escape), and the tokens are OR-ed together.
// Quoting every token means arbitrary punctuation (error codes, quotes, parens)
// is treated as literal text and can never produce a syntax error, while OR
// keeps the search permissive enough that any single token can match.
// Returns "" when the query has no usable content.
func ftsPhrase(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR ")
}
