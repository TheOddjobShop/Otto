package store

import (
	"context"
	"fmt"
	"time"
)

// Totals is an aggregate of token_usage rows.
type Totals struct {
	InputTokens   int
	OutputTokens  int
	CacheCreation int
	CacheRead     int
}

// SourceTotals is Totals for one source label.
type SourceTotals struct {
	Source string
	Totals
}

// ModelTotals is Totals for one model id. Cost can only be computed per model
// — rates differ per tier — so this is the aggregate the pricing layer reads,
// not SourceTotals (a single source such as "main" spans several models
// because the per-turn router picks one).
type ModelTotals struct {
	Model string
	Totals
}

// RecordUsage appends one token-usage row. It stamps ts with the current unix
// time, mirroring AppendTurn. Best-effort callers may ignore the error after
// logging — a failed usage write must never break a reply.
func (s *Store) RecordUsage(ctx context.Context, source, model string, in, out, cacheCreation, cacheRead int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO token_usage(source, model, input_tokens, output_tokens, cache_creation, cache_read, ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		source, model, in, out, cacheCreation, cacheRead, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: record usage: %w", err)
	}
	return nil
}

// UsageTotals returns the grand total across all rows. Zero values on an empty
// table (COALESCE turns the NULL SUM of no rows into 0).
func (s *Store) UsageTotals(ctx context.Context) (Totals, error) {
	var t Totals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0), COALESCE(SUM(cache_read), 0)
		FROM token_usage`).
		Scan(&t.InputTokens, &t.OutputTokens, &t.CacheCreation, &t.CacheRead)
	if err != nil {
		return Totals{}, fmt.Errorf("store: usage totals: %w", err)
	}
	return t, nil
}

// UsageByModel returns one aggregate row per model id, ordered by model name
// for stable rendering. Used by the cost estimator, which needs per-model
// token counts because each tier bills at a different rate.
func (s *Store) UsageByModel(ctx context.Context) ([]ModelTotals, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0), COALESCE(SUM(cache_read), 0)
		FROM token_usage
		GROUP BY model
		ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("store: usage by model: %w", err)
	}
	defer rows.Close()

	var out []ModelTotals
	for rows.Next() {
		var mt ModelTotals
		if err := rows.Scan(&mt.Model, &mt.InputTokens, &mt.OutputTokens,
			&mt.CacheCreation, &mt.CacheRead); err != nil {
			return nil, fmt.Errorf("store: scan usage by model: %w", err)
		}
		out = append(out, mt)
	}
	return out, rows.Err()
}

// UsageBySource returns one aggregate row per source, ordered by source name
// for stable rendering.
func (s *Store) UsageBySource(ctx context.Context) ([]SourceTotals, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0), COALESCE(SUM(cache_read), 0)
		FROM token_usage
		GROUP BY source
		ORDER BY source`)
	if err != nil {
		return nil, fmt.Errorf("store: usage by source: %w", err)
	}
	defer rows.Close()

	var out []SourceTotals
	for rows.Next() {
		var st SourceTotals
		if err := rows.Scan(&st.Source, &st.InputTokens, &st.OutputTokens,
			&st.CacheCreation, &st.CacheRead); err != nil {
			return nil, fmt.Errorf("store: scan usage: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
