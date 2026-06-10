package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// perBackendTimeout is the deadline given to each backend call in a Chain.
// Using a fresh context per backend (rather than a shared one) ensures a
// slow or cold-loading first backend does not starve the fallback: even if
// backend A consumes its full 60 s, backend B still gets an independent 60 s
// budget derived from the original (outer) ctx.
const perBackendTimeout = 60 * time.Second

// Compile-time assertion that *Chain satisfies Embedder.
var _ Embedder = (*Chain)(nil)

// Chain is an Embedder that tries its backends in order and returns the first
// success. When every backend fails it returns an aggregated error; the caller
// treats that as the signal to fall back to non-semantic (keyword) search.
type Chain struct {
	backends []Embedder
}

// NewChain builds a chain from the given backends, tried in argument order.
func NewChain(backends ...Embedder) *Chain {
	return &Chain{backends: backends}
}

// NewOllamaChain builds a Chain of Ollama backends, one per model, all hitting
// the same baseURL, tried in the given order. Blank/whitespace model names are
// skipped. With no usable models the chain is empty and Embed will error
// (caller falls back to keyword search).
func NewOllamaChain(baseURL string, models []string) *Chain {
	backends := make([]Embedder, 0, len(models))
	for _, m := range models {
		if strings.TrimSpace(m) == "" {
			continue
		}
		backends = append(backends, NewOllama(baseURL, m))
	}
	return NewChain(backends...)
}

// Name lists the chained backends in order, e.g.
// "chain[ollama:embeddinggemma,ollama:nomic-embed-text]".
func (c *Chain) Name() string {
	names := make([]string, len(c.backends))
	for i, b := range c.backends {
		names[i] = b.Name()
	}
	return "chain[" + strings.Join(names, ",") + "]"
}

// Embed tries each backend in order, returning the first successful Result.
// Each backend receives its own context.WithTimeout(ctx, perBackendTimeout)
// derived from the caller's ctx. This prevents a slow (e.g. cold-loading)
// first backend from exhausting a shared deadline and leaving the fallback
// with an already-expired context.
// Returns an aggregated error if the chain is empty or all backends fail.
func (c *Chain) Embed(ctx context.Context, text string) (Result, error) {
	if len(c.backends) == 0 {
		return Result{}, errors.New("embed: empty chain")
	}
	var errs []error
	for _, b := range c.backends {
		bctx, cancel := context.WithTimeout(ctx, perBackendTimeout)
		res, err := b.Embed(bctx, text)
		cancel()
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", b.Name(), err))
	}
	return Result{}, fmt.Errorf("embed: all backends failed: %w", errors.Join(errs...))
}
