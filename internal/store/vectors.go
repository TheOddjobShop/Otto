package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	"otto/internal/embed"
)

// encodeVec serializes a float32 vector as little-endian IEEE-754 bytes
// (4 bytes per element) for BLOB storage.
func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec is the inverse of encodeVec. A nil/empty slice decodes to an
// empty vector. Trailing bytes that don't form a full float32 are ignored.
func decodeVec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// PutVector stores (or replaces) the embedding for a turn. dim is recorded so
// SearchSemantic can skip vectors from a different-dimensioned model.
func (s *Store) PutVector(ctx context.Context, turnID int64, model string, vec []float32) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO vectors(turn_id, model, dim, vec) VALUES (?, ?, ?, ?)`,
		turnID, model, len(vec), encodeVec(vec),
	)
	if err != nil {
		return fmt.Errorf("store: put vector: %w", err)
	}
	return nil
}

// SearchSemantic returns the turns whose stored embeddings are most cosine-
// similar to query, most-similar first, capped at limit (default 10). Only
// vectors with the same dimension as query are considered, so a model swap
// silently ignores stale-dimension rows until they are re-embedded. A
// zero-length query returns no rows.
func (s *Store) SearchSemantic(ctx context.Context, query []float32, limit int) ([]Turn, error) {
	if len(query) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.persona, t.role, t.content, t.ts, v.vec
		FROM vectors v
		JOIN turns t ON t.id = v.turn_id
		WHERE v.dim = ?`, len(query))
	if err != nil {
		return nil, fmt.Errorf("store: semantic query: %w", err)
	}
	defer rows.Close()

	type scored struct {
		turn  Turn
		score float64
	}
	var all []scored
	for rows.Next() {
		var tr Turn
		var ts int64
		var blob []byte
		if err := rows.Scan(&tr.ID, &tr.Persona, &tr.Role, &tr.Content, &ts, &blob); err != nil {
			return nil, fmt.Errorf("store: semantic scan: %w", err)
		}
		tr.TS = time.Unix(ts, 0)
		all = append(all, scored{turn: tr, score: embed.Cosine(query, decodeVec(blob))})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: semantic rows: %w", err)
	}

	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]Turn, len(all))
	for i, sc := range all {
		out[i] = sc.turn
	}
	return out, nil
}
