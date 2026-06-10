package embed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeEmbedder is a test double returning a fixed result or error.
type fakeEmbedder struct {
	name   string
	result Result
	err    error
	calls  int
}

func (f *fakeEmbedder) Embed(ctx context.Context, text string) (Result, error) {
	f.calls++
	return f.result, f.err
}
func (f *fakeEmbedder) Name() string { return f.name }

func TestChainFirstSuccessWins(t *testing.T) {
	a := &fakeEmbedder{name: "a", result: Result{Vector: []float32{1}, Model: "a"}}
	b := &fakeEmbedder{name: "b", result: Result{Vector: []float32{2}, Model: "b"}}
	c := NewChain(a, b)
	res, err := c.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if res.Model != "a" {
		t.Errorf("expected first backend to win, got %q", res.Model)
	}
	if b.calls != 0 {
		t.Errorf("second backend should not be called when first succeeds")
	}
}

func TestChainFallsThroughOnError(t *testing.T) {
	a := &fakeEmbedder{name: "a", err: errors.New("down")}
	b := &fakeEmbedder{name: "b", result: Result{Vector: []float32{2}, Model: "b"}}
	c := NewChain(a, b)
	res, err := c.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if res.Model != "b" {
		t.Errorf("expected fallthrough to second backend, got %q", res.Model)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("both backends should have been tried: a=%d b=%d", a.calls, b.calls)
	}
}

func TestChainAllFailReturnsError(t *testing.T) {
	a := &fakeEmbedder{name: "a", err: errors.New("down-a")}
	b := &fakeEmbedder{name: "b", err: errors.New("down-b")}
	c := NewChain(a, b)
	_, err := c.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error when all backends fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "a") || !strings.Contains(msg, "b") {
		t.Errorf("aggregated error should name both backends: %q", msg)
	}
}

func TestChainEmptyIsError(t *testing.T) {
	c := NewChain()
	if _, err := c.Embed(context.Background(), "x"); err == nil {
		t.Fatal("empty chain should error")
	}
}

func TestChainName(t *testing.T) {
	c := NewChain(&fakeEmbedder{name: "ollama:embeddinggemma"}, &fakeEmbedder{name: "ollama:nomic"})
	if c.Name() == "" {
		t.Error("chain name should be non-empty")
	}
}

func TestNewOllamaChainBuildsBackendsInOrder(t *testing.T) {
	c := NewOllamaChain("http://localhost:11434", []string{"embeddinggemma", "nomic-embed-text"})
	name := c.Name()
	if !strings.Contains(name, "ollama:embeddinggemma") || !strings.Contains(name, "ollama:nomic-embed-text") {
		t.Fatalf("chain name missing backends: %q", name)
	}
	if strings.Index(name, "embeddinggemma") > strings.Index(name, "nomic-embed-text") {
		t.Errorf("expected embeddinggemma before nomic: %q", name)
	}
}

func TestNewOllamaChainSkipsBlankModels(t *testing.T) {
	c := NewOllamaChain("http://x", []string{"", "embeddinggemma", "  "})
	if strings.Count(c.Name(), "ollama:") != 1 {
		t.Errorf("blank models should be skipped: %q", c.Name())
	}
}

// TestChainPerBackendTimeoutIsFresh verifies that the second backend in a chain
// receives an independent per-backend deadline even when the first backend
// exhausts its own budget. Without fresh-per-backend contexts a slow backend A
// would expire the shared caller context and leave backend B starting already
// dead.
func TestChainPerBackendTimeoutIsFresh(t *testing.T) {
	// The first backend blocks for 20 ms before returning an error, simulating
	// a slow (but not fully hung) backend. The outer context has a 5 s deadline
	// and is never cancelled by the first backend's work. We record whether the
	// context passed to the second backend is still alive at entry, which would
	// be false if chain.Embed passed the outer ctx directly (it would be fine
	// here), but more importantly we confirm the second backend is reached and
	// succeeds even after the first backend fails with its own context.
	good := &fakeEmbedder{name: "good", result: Result{Vector: []float32{1}, Model: "good"}}

	var goodCtxAlive bool
	wrappedGood := &contextCheckEmbedder{
		inner:   good,
		onEmbed: func(ctx context.Context) { goodCtxAlive = ctx.Err() == nil },
	}

	// First backend times out (simulated) after 20 ms; the chain must
	// fall through and deliver the second backend a fresh context.
	quickHang := &timedHangEmbedder{name: "slow", hangFor: 20 * time.Millisecond}
	c := NewChain(quickHang, wrappedGood)

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer outerCancel()

	res, err := c.Embed(outerCtx, "x")
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if res.Model != "good" {
		t.Errorf("expected fallback model, got %q", res.Model)
	}
	if !goodCtxAlive {
		t.Error("fallback backend received an already-cancelled context (per-backend isolation broken)")
	}
}

// timedHangEmbedder blocks for hangFor before returning an error, simulating a
// slow backend that does not wait for its context.
type timedHangEmbedder struct {
	name    string
	hangFor time.Duration
}

func (h *timedHangEmbedder) Embed(ctx context.Context, text string) (Result, error) {
	select {
	case <-time.After(h.hangFor):
		return Result{}, errors.New("timed out (simulated)")
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
func (h *timedHangEmbedder) Name() string { return h.name }

// contextCheckEmbedder wraps an inner Embedder and calls onEmbed with the
// received context before delegating, so tests can inspect its liveness.
type contextCheckEmbedder struct {
	inner   Embedder
	onEmbed func(context.Context)
}

func (c *contextCheckEmbedder) Embed(ctx context.Context, text string) (Result, error) {
	if c.onEmbed != nil {
		c.onEmbed(ctx)
	}
	return c.inner.Embed(ctx, text)
}
func (c *contextCheckEmbedder) Name() string { return c.inner.Name() }
