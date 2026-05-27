package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

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
// Returns an aggregated error if the chain is empty or all backends fail.
func (c *Chain) Embed(ctx context.Context, text string) (Result, error) {
	if len(c.backends) == 0 {
		return Result{}, errors.New("embed: empty chain")
	}
	var errs []error
	for _, b := range c.backends {
		res, err := b.Embed(ctx, text)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", b.Name(), err))
	}
	return Result{}, fmt.Errorf("embed: all backends failed: %w", errors.Join(errs...))
}
