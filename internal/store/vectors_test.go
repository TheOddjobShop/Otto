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

func TestDecodeOddLengthTruncatesWithoutPanic(t *testing.T) {
	// 5 bytes = one full float32 (4 bytes) + 1 trailing byte that must be
	// dropped, not cause an out-of-bounds read.
	if got := decodeVec([]byte{1, 2, 3, 4, 5}); len(got) != 1 {
		t.Fatalf("decode(5 bytes) len = %d, want 1 (trailing byte dropped)", len(got))
	}
}

func TestDecodeEmptyIsEmpty(t *testing.T) {
	if got := decodeVec(nil); len(got) != 0 {
		t.Fatalf("decode(nil) len = %d, want 0", len(got))
	}
}

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

// TestSchemaHasVectorsDimIndex confirms the vectors_dim index is present so
// SearchSemantic's WHERE v.dim = ? predicate hits an index rather than doing
// a full table scan.
func TestSchemaHasVectorsDimIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	var name string
	err = s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name = 'vectors_dim'`).Scan(&name)
	if err != nil {
		t.Fatalf("vectors_dim index missing: %v", err)
	}
}

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
	seedTurnWithVector(t, s, ctx, "three-dim", []float32{1, 0, 0})

	got, err := s.SearchSemantic(ctx, []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}
	if len(got) != 1 || got[0].Content != "two-dim" {
		t.Fatalf("dim filter failed: got %+v", got)
	}
}

// TestSearchSemanticModelFiltersByModel guards the cross-model bug: two rows
// share a dimension but were written by different embedding models, whose
// vector spaces are unrelated. A model-scoped search must return only the
// row from the queried model, silently ignoring the stale-model row.
func TestSearchSemanticModelFiltersByModel(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	idA, err := s.AppendTurn(ctx, "otto", "assistant", "from-model-a")
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if err := s.PutVector(ctx, idA, "model-a", []float32{1, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}
	idB, err := s.AppendTurn(ctx, "otto", "assistant", "from-model-b")
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if err := s.PutVector(ctx, idB, "model-b", []float32{1, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	got, err := s.SearchSemanticModel(ctx, []float32{1, 0}, "model-a", 10)
	if err != nil {
		t.Fatalf("SearchSemanticModel: %v", err)
	}
	if len(got) != 1 || got[0].Content != "from-model-a" {
		t.Fatalf("model filter failed: got %+v", got)
	}

	// An empty model tag disables the filter and considers both rows.
	all, err := s.SearchSemanticModel(ctx, []float32{1, 0}, "", 10)
	if err != nil {
		t.Fatalf("SearchSemanticModel (no model): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("empty model should not filter: got %d, want 2", len(all))
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
