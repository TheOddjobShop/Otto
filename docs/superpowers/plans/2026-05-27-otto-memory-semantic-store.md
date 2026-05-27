# Otto Memory Semantic Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add semantic vector search to `internal/store`: a `vectors` table holding one embedding per turn, a float32↔blob codec, `PutVector` to persist an embedding, and brute-force cosine `SearchSemantic` to rank turns by similarity to a query vector. This is the storage half of semantic retrieval; the wiring (embed-on-log, merging into `session_search`, config/setup) is a later sub-plan.

**Architecture:** Embeddings are stored as little-endian float32 BLOBs in a `vectors` table keyed by `turn_id` (one vector per turn), tagged with `model` and `dim`. `SearchSemantic` loads only vectors whose `dim` matches the query (so a model/dimension swap silently ignores stale rows rather than comparing across incompatible spaces), scores each with `embed.Cosine`, sorts descending, and returns the top-k joined `Turn` rows. Brute-force is correct at single-user scale (a few thousand rows → sub-millisecond). Reuses the existing `embed.Cosine` (pure leaf package, no cycle).

**Tech Stack:** Go 1.26, existing `internal/store` (modernc.org/sqlite) + `internal/embed` (for `Cosine`), stdlib `encoding/binary`, `math`, `sort`.

---

## Context on existing code

- `internal/store/store.go`: `Store{db *sql.DB}`, `Open(path)`, `Close()`, and a `const schema` string run on every `Open` (idempotent `CREATE ... IF NOT EXISTS`). The existing schema has `turns(id, persona, role, content, ts)` + `turns_fts` + an AFTER INSERT trigger.
- `internal/store/turns.go`: `Turn{ID int64; Persona, Role, Content string; TS time.Time}`, `AppendTurn(ctx, persona, role, content) (int64, error)`, `SearchFTS(ctx, query, limit) ([]Turn, error)`.
- `internal/embed/embed.go`: `func Cosine(a, b []float32) float64` (returns 0 for mismatched/empty/zero vectors).

## File Structure

- `internal/store/store.go` (modify) — append a `vectors` table to the `schema` const.
- `internal/store/vectors.go` (create) — `encodeVec`/`decodeVec` codec, `PutVector`, `SearchSemantic`.
- `internal/store/vectors_test.go` (create) — codec round-trip, schema presence, PutVector+SearchSemantic ranking + dim-filter.

---

## Task 1: float32 ↔ blob codec

**Files:**
- Create: `internal/store/vectors.go`
- Test: `internal/store/vectors_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/store/vectors_test.go`:
```go
package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEncodeDecodeVecRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, 3.14159, -2.71828}
	out := decodeVec(encodeVec(in))
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("out[%d] = %v, want %v", i, out[i], in[i])
		}
	}
}

func TestEncodeVecLengthIsFourBytesPerFloat(t *testing.T) {
	if got := len(encodeVec([]float32{1, 2, 3})); got != 12 {
		t.Fatalf("encoded length = %d, want 12", got)
	}
}

func TestDecodeEmptyIsEmpty(t *testing.T) {
	if got := decodeVec(nil); len(got) != 0 {
		t.Fatalf("decode(nil) len = %d, want 0", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestEncode|TestDecode' -v`
Expected: FAIL — `undefined: decodeVec` / `undefined: encodeVec`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/store/vectors.go`:
```go
package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestEncode|TestDecode' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/vectors.go internal/store/vectors_test.go
git commit -m "feat(store): float32 vector blob codec"
```

---

## Task 2: vectors table schema

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/vectors_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/vectors_test.go`:
```go
func TestSchemaHasVectorsTable(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	var name string
	err = s.db.QueryRow(`SELECT name FROM sqlite_master WHERE name = 'vectors'`).Scan(&name)
	if err != nil {
		t.Fatalf("vectors table missing: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSchemaHasVectors -v`
Expected: FAIL — `vectors table missing: sql: no rows in result set`.

- [ ] **Step 3: Write minimal implementation**

In `internal/store/store.go`, append the `vectors` table to the `schema` const (add it inside the backtick string, after the existing trigger):
```sql
CREATE TABLE IF NOT EXISTS vectors (
	turn_id INTEGER PRIMARY KEY REFERENCES turns(id) ON DELETE CASCADE,
	model   TEXT    NOT NULL,
	dim     INTEGER NOT NULL,
	vec     BLOB    NOT NULL
);
```
(One vector per turn → `turn_id` is the primary key. `dim` lets `SearchSemantic` filter to comparable vectors after a model swap.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSchemaHasVectors -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/vectors_test.go
git commit -m "feat(store): add vectors table to schema"
```

---

## Task 3: PutVector + SearchSemantic

**Files:**
- Modify: `internal/store/vectors.go`
- Modify: `internal/store/vectors_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/vectors_test.go`:
```go
func seedTurnWithVector(t *testing.T, s *Store, ctx context.Context, content string, vec []float32) {
	t.Helper()
	id, err := s.AppendTurn(ctx, "otto", "assistant", content)
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if err := s.PutVector(ctx, id, "test-model", vec); err != nil {
		t.Fatalf("PutVector: %v", err)
	}
}

func TestSearchSemanticRanksByCosine(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	seedTurnWithVector(t, s, ctx, "east", []float32{1, 0})
	seedTurnWithVector(t, s, ctx, "north", []float32{0, 1})
	seedTurnWithVector(t, s, ctx, "east-ish", []float32{0.9, 0.1})

	got, err := s.SearchSemantic(ctx, []float32{1, 0}, 3)
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	// Closest to [1,0] is "east", then "east-ish", then "north".
	if got[0].Content != "east" {
		t.Errorf("rank 0 = %q, want east", got[0].Content)
	}
	if got[1].Content != "east-ish" {
		t.Errorf("rank 1 = %q, want east-ish", got[1].Content)
	}
	if got[2].Content != "north" {
		t.Errorf("rank 2 = %q, want north", got[2].Content)
	}
}

func TestSearchSemanticRespectsLimit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	seedTurnWithVector(t, s, ctx, "a", []float32{1, 0})
	seedTurnWithVector(t, s, ctx, "b", []float32{0.8, 0.2})
	seedTurnWithVector(t, s, ctx, "c", []float32{0.6, 0.4})
	got, err := s.SearchSemantic(ctx, []float32{1, 0}, 2)
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit not respected: got %d, want 2", len(got))
	}
}

func TestSearchSemanticIgnoresMismatchedDim(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	seedTurnWithVector(t, s, ctx, "two-dim", []float32{1, 0})
	seedTurnWithVector(t, s, ctx, "three-dim", []float32{1, 0, 0}) // different dim

	got, err := s.SearchSemantic(ctx, []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}
	if len(got) != 1 || got[0].Content != "two-dim" {
		t.Fatalf("dim filter failed: got %+v", got)
	}
}

func TestPutVectorReplaces(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	id, err := s.AppendTurn(ctx, "otto", "user", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutVector(ctx, id, "m", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutVector(ctx, id, "m", []float32{0, 1}); err != nil {
		t.Fatal(err)
	}
	// Only one vector row should exist for the turn (REPLACE, not duplicate).
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vectors WHERE turn_id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 vector row after replace, got %d", count)
	}
}

func TestSearchSemanticEmptyQueryReturnsNothing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	got, err := s.SearchSemantic(context.Background(), nil, 5)
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty query should return nothing, got %d", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestSearchSemantic|TestPutVector' -v`
Expected: FAIL — `undefined: (*Store).PutVector` / `undefined: (*Store).SearchSemantic`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/store/vectors.go`:
```go
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
```
Add `"time"` to `vectors.go`'s import block (used for `time.Unix`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: PASS (entire package — new vector tests + all existing store tests).

- [ ] **Step 5: Run the race detector**

Run: `go test -race ./internal/store/`
Expected: PASS, no races.

- [ ] **Step 6: Commit**

```bash
git add internal/store/vectors.go internal/store/vectors_test.go
git commit -m "feat(store): PutVector + brute-force cosine SearchSemantic"
```

---

## Task 4: Final verification

- [ ] **Step 1: Vet + format**

Run:
```bash
go vet ./internal/store/
gofmt -l internal/store/
```
Expected: vet exit 0; gofmt prints nothing.

- [ ] **Step 2: Full module build + test**

Run:
```bash
go build ./...
go test ./...
go test -race ./internal/store/
```
Expected: all pass. (`internal/store` now imports `internal/embed`; confirm no import cycle — there isn't, embed is a leaf.)

- [ ] **Step 3: Confirm no consumer wiring yet**

Run: `git grep -n "PutVector\|SearchSemantic" -- ':!internal/store'`
Expected: no matches — these are not yet called by the bot or otto-memory server. That wiring is the next sub-plan.

---

## Self-Review notes

- **Spec coverage (this slice):** `state.db` vectors table (Task 2), float32 blob codec (Task 1), `PutVector` persist (Task 3), brute-force cosine `SearchSemantic` with dim-filtering + top-k + limit (Task 3). Deferred to later sub-plans (correctly out of scope): embedding turns on log (bot calls `PutVector` after `AppendTurn`), merging semantic + FTS5 in the otto-memory `session_search` + query embedding, the `embed`-chain construction from config in both binaries, `setup.sh` Ollama install/pull, and the idle rotator.
- **Type consistency:** `encodeVec([]float32) []byte`, `decodeVec([]byte) []float32`, `(*Store).PutVector(ctx, int64, string, []float32) error`, `(*Store).SearchSemantic(ctx, []float32, int) ([]Turn, error)`. Reuses existing `Turn` fields and `AppendTurn`, and `embed.Cosine`. The `vectors.go` import block needs `context`, `encoding/binary`, `fmt`, `math`, `sort`, `time`, and `otto/internal/embed`.
- **Dependency note:** `internal/store` now imports `internal/embed` (for `Cosine`). `embed` imports no `otto` packages, so this is acyclic.
- **Note for the wiring sub-plan:** after `logTurn` → `AppendTurn` returns the turn id, the bot should embed the content (via the `embed.Chain`) and call `PutVector(ctx, id, result.Model, result.Vector)` — best-effort, off the reply path. `session_search` in otto-memory should embed the query, call `SearchSemantic` + `SearchFTS`, and merge/dedup by `Turn.ID`. On embed failure, fall back to FTS5-only.
```
