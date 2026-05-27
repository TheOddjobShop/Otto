// Package embed produces text embeddings via local Ollama models, with an
// ordered fallthrough chain and a cosine-similarity helper for brute-force
// top-k retrieval at single-user scale.
package embed

import (
	"context"
	"math"
)

// Result is one embedding plus the model that produced it. The model tag is
// stored alongside the vector so a backend swap (different dimensions) can be
// detected and the affected rows re-embedded rather than compared across
// incompatible spaces.
type Result struct {
	Vector []float32
	Model  string
}

// Embedder turns text into a vector. Implementations: Ollama (one model) and
// Chain (ordered fallthrough over several Embedders).
type Embedder interface {
	// Embed returns the embedding of text, or an error if the backend is
	// unavailable or returns no vector.
	Embed(ctx context.Context, text string) (Result, error)
	// Name identifies the backend+model, e.g. "ollama:embeddinggemma".
	Name() string
}

// Cosine returns the cosine similarity of a and b in [-1, 1]. It returns 0
// for mismatched lengths or when either vector has zero magnitude (avoiding
// NaN), so callers can treat 0 as "not comparable / no signal".
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
