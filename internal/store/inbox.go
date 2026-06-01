package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// inboxDequeueCap bounds the number of rows DequeueAll returns in a single
// drain. A runaway producer (buggy MCP tool, retry storm) shouldn't be able
// to make one drain call do unbounded work — better to leave the tail for
// the next tick than to wedge the bus loop on a single iteration.
const inboxDequeueCap = 64

// validTargets / validSources enumerate the closed sets of recipients and
// origins the bus accepts. New values added here must also be wired into
// the dispatch path in cmd/otto.
var (
	validTargets = map[string]struct{}{"otto": {}, "toto": {}, "toot": {}}
	validSources = map[string]struct{}{"user": {}, "agent": {}}
)

// InboxMsg is one row from the inbox table after dequeue.
type InboxMsg struct {
	ID     int64
	TS     time.Time
	Target string // "otto" | "toto" | "toot"
	Source string // "user" | "agent"
	Sender string // "otto" | "toto" | "toot" | "" (user)
	Body   string
}

// ErrBusLoopGuard is returned by Enqueue when the caller's context indicates
// it is already running on behalf of an agent-sourced bus message. Without
// this guard, dispatch of an agent message could itself enqueue another
// agent message, producing an unbounded ping-pong between bots. Bus drain
// reads rows directly from SQLite and is unaffected — only the in-process
// Enqueue path needs to refuse.
var ErrBusLoopGuard = errors.New("store: inbox enqueue blocked by agent-hop guard")

// ctxKeyAgentHop tags a context as "currently dispatching an agent-sourced
// message". Set by the bus drain when source=="agent"; read by Enqueue so
// nested enqueues from downstream tool handlers (PR-C/D will plug in here)
// fail fast with ErrBusLoopGuard.
type ctxKeyAgentHop struct{}

// WithAgentHop marks ctx as carrying an in-flight agent-sourced bus message.
// Exported so cmd/otto can wrap its dispatch context.
func WithAgentHop(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyAgentHop{}, true)
}

// IsAgentHop reports whether ctx was tagged by WithAgentHop.
func IsAgentHop(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyAgentHop{}).(bool)
	return v
}

// Enqueue inserts one row into the inbox and returns its id.
//
// target ∈ {otto,toto,toot}, source ∈ {user,agent}. body must be non-empty
// after trimming whitespace; sender may be empty when source=="user".
//
// If ctx is tagged via WithAgentHop, Enqueue refuses with ErrBusLoopGuard:
// an agent-dispatch is already in flight, and letting it queue another
// agent message would let the bus chase its own tail.
func (s *Store) Enqueue(ctx context.Context, target, source, sender, body string) (int64, error) {
	if IsAgentHop(ctx) {
		return 0, ErrBusLoopGuard
	}
	if _, ok := validTargets[target]; !ok {
		return 0, fmt.Errorf("store: inbox enqueue: invalid target %q", target)
	}
	if _, ok := validSources[source]; !ok {
		return 0, fmt.Errorf("store: inbox enqueue: invalid source %q", source)
	}
	if strings.TrimSpace(body) == "" {
		return 0, fmt.Errorf("store: inbox enqueue: empty body")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO inbox(ts, target, source, sender, body) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), target, source, sender, body,
	)
	if err != nil {
		return 0, fmt.Errorf("store: inbox enqueue: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: inbox enqueue: last insert id: %w", err)
	}
	return id, nil
}

// DequeueAll returns up to inboxDequeueCap undelivered messages in id order
// and marks them delivered in the same transaction so a crashed dispatcher
// can't re-deliver them on the next boot. A second call with no new rows
// returns an empty slice.
func (s *Store) DequeueAll(ctx context.Context) ([]InboxMsg, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, ts, target, source, sender, body
		FROM inbox
		WHERE delivered = 0
		ORDER BY id
		LIMIT ?`, inboxDequeueCap)
	if err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: select: %w", err)
	}

	var out []InboxMsg
	var ids []int64
	for rows.Next() {
		var m InboxMsg
		var ts string
		if err := rows.Scan(&m.ID, &ts, &m.Target, &m.Source, &m.Sender, &m.Body); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: inbox dequeue: scan: %w", err)
		}
		// Parse RFC3339Nano; tolerate plain RFC3339 written by older callers.
		if parsed, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
			m.TS = parsed
		} else if parsed, perr := time.Parse(time.RFC3339, ts); perr == nil {
			m.TS = parsed
		}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: inbox dequeue: rows: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	// Mark exactly the rows we read as delivered. Using an IN(...) over the
	// ids we just scanned (rather than a blanket delivered=0 update) avoids
	// silently flipping rows that arrived between the SELECT and the UPDATE.
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inbox SET delivered = 1 WHERE id IN (`+placeholders+`)`, args...,
	); err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: mark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: commit: %w", err)
	}
	return out, nil
}
