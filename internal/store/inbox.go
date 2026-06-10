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

// MaxBusHop caps how many agent-to-agent hops a single conversation chain
// may take before the bus refuses further enqueues. Conversations winding
// down before the cap are nudged by the per-call "HOPS REMAINING" prompt
// the dispatcher injects.
const MaxBusHop = 3

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
	Hop    int // 0 for user-originated; +1 per agent-to-agent forward
}

// ErrBusHopExceeded is returned by Enqueue when a caller tries to push a
// row whose hop would exceed MaxBusHop. The bus dispatcher trusts this to
// halt a conversation chain that's run away, and the MCP tool handlers
// surface it back to the model as a polite refusal so it knows to wind
// down rather than retry.
var ErrBusHopExceeded = errors.New("store: inbox enqueue blocked by hop cap")

// ErrBusLoopGuard is retained as an alias of ErrBusHopExceeded for
// backwards compatibility with callers/tests that referenced the older
// boolean-flavored loop guard. New code should reference ErrBusHopExceeded
// directly.
var ErrBusLoopGuard = ErrBusHopExceeded

// ctxKeyBusHop tags a context with the hop count of the currently
// dispatching bus message. The dispatcher in cmd/otto wraps each agent-
// targeted ctx via WithBusHop(n); downstream tool handlers read it via
// BusHopFromCtx and pass hop+1 to Enqueue so the chain self-counts.
type ctxKeyBusHop struct{}

// WithBusHop returns a child context carrying the bus-hop counter n. The
// dispatcher uses this to thread the current hop through the per-call
// context handed to Otto/Toto/Toot so their tool handlers can increment it
// when enqueueing a follow-up.
func WithBusHop(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, ctxKeyBusHop{}, n)
}

// BusHopFromCtx returns the bus-hop counter set by WithBusHop, and whether
// it was present. Absent counters are treated as zero by callers (the
// initial user-originated turn is hop 0).
func BusHopFromCtx(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(ctxKeyBusHop{}).(int)
	return v, ok
}

// WithAgentHop is the pre-hop-counter compatibility shim. It marks ctx as
// already being inside an agent dispatch so legacy callers that called
// Enqueue without specifying a hop still trip the cap. Equivalent to
// WithBusHop(ctx, MaxBusHop).
func WithAgentHop(ctx context.Context) context.Context {
	return WithBusHop(ctx, MaxBusHop)
}

// Enqueue inserts one row into the inbox and returns its id.
//
// target ∈ {otto,toto,toot}, source ∈ {user,agent}. body must be non-empty
// after trimming whitespace; sender may be empty when source=="user".
// hop is the bus-chain depth this row will carry; the dispatcher injects
// it into the recipient's per-call prompt so the model can wind down
// before hitting the cap.
//
// If hop > MaxBusHop, Enqueue refuses with ErrBusHopExceeded so chained
// agent-to-agent conversations stop after a bounded number of forwards.
func (s *Store) Enqueue(ctx context.Context, target, source, sender, body string, hop int) (int64, error) {
	if hop < 0 {
		return 0, fmt.Errorf("store: inbox enqueue: negative hop %d", hop)
	}
	if hop > MaxBusHop {
		return 0, ErrBusHopExceeded
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
		`INSERT INTO inbox(ts, target, source, sender, body, hop) VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), target, source, sender, body, hop,
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
		SELECT id, ts, target, source, sender, body, hop
		FROM inbox
		WHERE delivered = 0
		ORDER BY id
		LIMIT ?`, inboxDequeueCap)
	if err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: select: %w", err)
	}

	var out []InboxMsg
	var maxID int64
	for rows.Next() {
		var m InboxMsg
		var ts string
		if err := rows.Scan(&m.ID, &ts, &m.Target, &m.Source, &m.Sender, &m.Body, &m.Hop); err != nil {
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
		if m.ID > maxID {
			maxID = m.ID
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: inbox dequeue: rows: %w", err)
	}
	rows.Close()

	if len(out) == 0 {
		return nil, tx.Commit()
	}

	// Mark exactly the rows we read as delivered. The SELECT above fetches
	// rows in ascending id order with LIMIT, so maxID is the largest id in
	// this batch and all delivered=0 rows with id <= maxID are exactly the
	// rows we just scanned. Using an id-range predicate instead of a
	// variable-length IN(...) list avoids SQLITE_LIMIT_VARIABLE_NUMBER
	// (999 on older SQLite builds); should inboxDequeueCap ever be raised
	// above that limit the range form remains correct and safe.
	if _, err := tx.ExecContext(ctx,
		`UPDATE inbox SET delivered = 1 WHERE delivered = 0 AND id <= ?`, maxID,
	); err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: mark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: inbox dequeue: commit: %w", err)
	}
	return out, nil
}

// PruneInbox deletes delivered inbox rows whose id is more than keep rows
// behind the current maximum id, returning the count of rows removed.
// A keep value ≤ 0 is a no-op.
//
// Delivered rows (delivered=1) are functionally dead once the dispatcher has
// processed them, but without periodic removal they accumulate indefinitely
// and inflate disk usage and the cost of full-table scans on older SQLite
// query plans. Undelivered rows (delivered=0) are never touched by this call
// so in-flight messages are always safe.
//
// PruneInbox is safe to call from a background goroutine alongside normal
// message dispatch; the WAL journal allows concurrent readers and writers.
func (s *Store) PruneInbox(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	// Keep the keep most-recent delivered rows; delete the rest. The inner
	// query uses OFFSET keep-1 to find the id of the oldest row we want to
	// keep (the keep-th newest in descending order). Every delivered row
	// with a smaller id is older and can be removed. If there are fewer than
	// keep delivered rows the sub-select returns no row, the outer DELETE
	// finds no match, and nothing is deleted.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inbox
		 WHERE delivered = 1
		   AND id < (
			   SELECT id FROM inbox
			   WHERE delivered = 1
			   ORDER BY id DESC
			   LIMIT 1 OFFSET ?
		   )`, keep-1)
	if err != nil {
		return 0, fmt.Errorf("store: prune inbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune inbox: rows affected: %w", err)
	}
	return n, nil
}
