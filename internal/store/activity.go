package store

import (
	"context"
	"fmt"
	"time"
)

// The activity log is Otto's write-ahead record of what he DID, as distinct
// from the `turns` table's record of what was SAID. A long coding turn emits
// no assistant text for minutes while it runs tools; during exactly that
// window the user asks "what's Otto doing?", and without this the only honest
// answer is "something".
//
// Rows are append-only and grouped by turn_key so a reader can ask for "this
// turn" rather than "the last N rows globally" — the difference matters when a
// bus turn and a Telegram turn interleave.

// Activity kinds. Kept as a closed set so a reader can switch on them.
const (
	ActivityTurnStart = "turn_start" // a turn began; Detail is the prompt
	ActivityTool      = "tool"       // a tool was invoked; Tool + Detail describe it
	ActivityResult    = "result"     // a tool returned; IsError marks failure
	ActivityTurnEnd   = "turn_end"   // a turn finished; Detail carries the outcome
)

// ActivityEntry is one row of the activity log.
type ActivityEntry struct {
	ID      int64
	TS      time.Time
	Persona string // "otto" — pets have no tool surface worth logging today
	TurnKey string // groups rows produced by a single turn
	Kind    string // one of the Activity* constants
	Tool    string // tool name for Kind == ActivityTool/ActivityResult, else ""
	Detail  string // human-readable one-liner
	IsError bool
}

// activityDetailCap bounds a stored detail string. These are one-line summaries
// destined for a Haiku prompt, not a transcript; a tool that echoes a large
// argument shouldn't be able to bloat the table or the prompt.
const activityDetailCap = 300

// AppendActivity writes one activity row. Best-effort by contract: callers log
// and continue, because losing an activity line must never break a reply.
func (s *Store) AppendActivity(ctx context.Context, e ActivityEntry) error {
	detail := e.Detail
	if len(detail) > activityDetailCap {
		r := []rune(detail)
		if len(r) > activityDetailCap {
			detail = string(r[:activityDetailCap]) + "…"
		}
	}
	errFlag := 0
	if e.IsError {
		errFlag = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO activity(ts, persona, turn_key, kind, tool, detail, is_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), e.Persona, e.TurnKey, e.Kind, e.Tool, detail, errFlag,
	)
	if err != nil {
		return fmt.Errorf("store: append activity: %w", err)
	}
	return nil
}

// ActivityForTurn returns up to limit of the most recent rows for one turn, in
// chronological order. Ordering is ascending on the way out even though the
// query takes the newest rows, so a reader sees the sequence as it happened.
func (s *Store) ActivityForTurn(ctx context.Context, turnKey string, limit int) ([]ActivityEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, persona, turn_key, kind, tool, detail, is_error
		FROM (
			SELECT id, ts, persona, turn_key, kind, tool, detail, is_error
			FROM activity
			WHERE turn_key = ?
			ORDER BY id DESC
			LIMIT ?
		)
		ORDER BY id ASC`, turnKey, limit)
	if err != nil {
		return nil, fmt.Errorf("store: activity for turn: %w", err)
	}
	defer rows.Close()
	return scanActivity(rows)
}

// RecentActivity returns up to limit of the most recent rows across all turns,
// in chronological order. Used for "what has Otto been up to" rather than
// "what is he doing right now".
func (s *Store) RecentActivity(ctx context.Context, limit int) ([]ActivityEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, persona, turn_key, kind, tool, detail, is_error
		FROM (
			SELECT id, ts, persona, turn_key, kind, tool, detail, is_error
			FROM activity
			ORDER BY id DESC
			LIMIT ?
		)
		ORDER BY id ASC`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent activity: %w", err)
	}
	defer rows.Close()
	return scanActivity(rows)
}

// rowScanner is the subset of *sql.Rows the activity scanner needs, so the two
// query helpers above can share one loop.
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanActivity(rows rowScanner) ([]ActivityEntry, error) {
	var out []ActivityEntry
	for rows.Next() {
		var e ActivityEntry
		var ts int64
		var errFlag int
		if err := rows.Scan(&e.ID, &ts, &e.Persona, &e.TurnKey, &e.Kind, &e.Tool, &e.Detail, &errFlag); err != nil {
			return nil, fmt.Errorf("store: scan activity: %w", err)
		}
		e.TS = time.Unix(ts, 0)
		e.IsError = errFlag != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneActivity keeps the most-recent keep rows and deletes the rest,
// returning the number removed. keep <= 0 is a no-op.
//
// The activity log is the highest-volume table Otto writes — a single agentic
// turn can emit dozens of rows — so it needs a tighter bound than `turns`.
func (s *Store) PruneActivity(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM activity
		 WHERE id < (
			 SELECT id FROM activity ORDER BY id DESC LIMIT 1 OFFSET ?
		 )`, keep-1)
	if err != nil {
		return 0, fmt.Errorf("store: prune activity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune activity: rows affected: %w", err)
	}
	return n, nil
}
