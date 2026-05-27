package embed

import (
	"math"
	"testing"
)

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestCosineIdenticalVectorsIsOne(t *testing.T) {
	v := []float32{1, 2, 3}
	if got := Cosine(v, v); !approxEqual(got, 1.0) {
		t.Fatalf("Cosine(v,v) = %v, want 1.0", got)
	}
}

func TestCosineOrthogonalIsZero(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := Cosine(a, b); !approxEqual(got, 0.0) {
		t.Fatalf("Cosine(orthogonal) = %v, want 0", got)
	}
}

func TestCosineOppositeIsNegativeOne(t *testing.T) {
	a := []float32{1, 1}
	b := []float32{-1, -1}
	if got := Cosine(a, b); !approxEqual(got, -1.0) {
		t.Fatalf("Cosine(opposite) = %v, want -1", got)
	}
}

func TestCosineMismatchedLengthsIsZero(t *testing.T) {
	if got := Cosine([]float32{1, 2, 3}, []float32{1, 2}); got != 0 {
		t.Fatalf("mismatched lengths should yield 0, got %v", got)
	}
}

func TestCosineZeroVectorIsZero(t *testing.T) {
	if got := Cosine([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Fatalf("zero vector should yield 0 (no NaN), got %v", got)
	}
}
