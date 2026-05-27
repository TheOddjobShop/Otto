package embed

import (
	"context"
	"errors"
	"strings"
	"testing"
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
