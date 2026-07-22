//go:build unix

package main

import (
	"context"
	"log"
	"time"

	"otto/internal/store"
)

const (
	// pruneInterval is how often the store pruner runs. Hourly is plenty:
	// a single-user bot adds at most a few hundred rows an hour, so the
	// tables can never grow meaningfully between passes.
	pruneInterval = time.Hour
	// pruneKeepTurns bounds the turns table (and, via triggers/cascades,
	// turns_fts and vectors) to the most recent N rows. 2000 is generous
	// for a single-user bot — weeks of history, and well above the
	// limit*10 candidate window SearchSemantic cosine-ranks, so pruning
	// improves (never degrades) semantic recall while capping the ~3 KB
	// per-row vector BLOB growth.
	pruneKeepTurns = 2000
	// pruneKeepInbox bounds delivered inbox rows to the most recent N.
	// Delivered rows are functionally dead once dispatched; 500 keeps a
	// generous debugging window without unbounded growth. Undelivered
	// rows are never touched by PruneInbox.
	pruneKeepInbox = 500
	// pruneKeepActivity bounds the activity log. It is the highest-volume
	// table Otto writes — one agentic turn can emit dozens of rows, where a
	// turn contributes exactly two rows to `turns` — so it gets a larger
	// absolute cap that still covers far less wall-clock history.
	pruneKeepActivity = 5000
)

// runStorePruner is a long-lived goroutine (started from main) that keeps
// the turn log and inbox bounded. It prunes once at startup — so processes
// that restart more often than pruneInterval still prune — then on every
// tick until ctx is cancelled. main tracks it with the same WaitGroup as
// the other store-writing goroutines so memStore.Close() never fires while
// a prune DELETE is mid-flight.
func runStorePruner(ctx context.Context, st *store.Store) {
	pruneStoreOnce(ctx, st, pruneKeepTurns, pruneKeepInbox, pruneKeepActivity)
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneStoreOnce(ctx, st, pruneKeepTurns, pruneKeepInbox, pruneKeepActivity)
		}
	}
}

// pruneStoreOnce runs one maintenance pass: prune old turns, then old
// delivered inbox rows. Errors are logged (except plain context
// cancellation during shutdown) rather than propagated — a failed prune
// just retries next tick.
func pruneStoreOnce(ctx context.Context, st *store.Store, keepTurns, keepInbox, keepActivity int) {
	if n, err := st.PruneTurns(ctx, keepTurns); err != nil {
		if ctx.Err() == nil {
			log.Printf("pruner: prune turns: %v", err)
		}
	} else if n > 0 {
		log.Printf("pruner: removed %d old turns (keeping %d most recent)", n, keepTurns)
	}
	if n, err := st.PruneInbox(ctx, keepInbox); err != nil {
		if ctx.Err() == nil {
			log.Printf("pruner: prune inbox: %v", err)
		}
	} else if n > 0 {
		log.Printf("pruner: removed %d delivered inbox rows (keeping %d most recent)", n, keepInbox)
	}
	if n, err := st.PruneActivity(ctx, keepActivity); err != nil {
		if ctx.Err() == nil {
			log.Printf("pruner: prune activity: %v", err)
		}
	} else if n > 0 {
		log.Printf("pruner: removed %d old activity rows (keeping %d most recent)", n, keepActivity)
	}
}
