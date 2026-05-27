# Otto Memory Embedder Chain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/embed` — a local text-embedding package backed by Ollama's `/api/embed` endpoint, with an ordered fallthrough `Chain` (try embeddinggemma, then nomic-embed-text, then the next backend) and a `Cosine` similarity helper. This is the semantic-retrieval foundation for Plan 4; nothing wires it into the bot or store yet (later sub-plans do).

**Architecture:** An `Embedder` interface (`Embed(ctx, text) (Result, error)` + `Name()`). `Ollama` implements it by POSTing `{model, input}` to `<baseURL>/api/embed` and reading back `{embeddings: [[float]]}`. `Chain` is itself an `Embedder` that tries its children in order and returns the first success — the caller (a later sub-plan) treats an all-backends-failed error as the signal to fall back to FTS5 keyword search. `Cosine` is a pure function for brute-force top-k ranking over stored vectors (single-user scale). No vector DB, no CGO, no new dependencies — stdlib `net/http` + `encoding/json` only.

**Tech Stack:** Go 1.26, stdlib (`net/http`, `encoding/json`, `math`, `testing`, `net/http/httptest`). Ollama API verified: `POST /api/embed` body `{"model": "...", "input": "..."}` → `{"embeddings": [[0.1, ...]], "model": "..."}`.

---

## File Structure

- `internal/embed/embed.go` — `Embedder` interface, `Result` struct, `Cosine` helper.
- `internal/embed/embed_test.go` — `Cosine` tests.
- `internal/embed/ollama.go` — `Ollama` backend (`NewOllama`, `Embed`, `Name`).
- `internal/embed/ollama_test.go` — `Ollama` tests against an httptest fake.
- `internal/embed/chain.go` — `Chain` fallthrough embedder (`NewChain`, `Embed`, `Name`).
- `internal/embed/chain_test.go` — `Chain` tests with fake embedders.

Each file has one responsibility; the package is independent of all other `otto` packages.

---

## Task 1: Embedder interface, Result, and Cosine

**Files:**
- Create: `internal/embed/embed.go`
- Test: `internal/embed/embed_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/embed/embed_test.go`:
```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/embed/ -run TestCosine -v`
Expected: FAIL — `undefined: Cosine`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/embed/embed.go`:
```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/embed/ -run TestCosine -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/embed/embed.go internal/embed/embed_test.go
git commit -m "feat(embed): Embedder interface, Result, and Cosine helper"
```

---

## Task 2: Ollama backend

**Files:**
- Create: `internal/embed/ollama.go`
- Test: `internal/embed/ollama_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/embed/ollama_test.go`:
```go
package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaEmbedParsesVector(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"embeddinggemma","embeddings":[[0.1,0.2,0.3]]}`)
	}))
	defer srv.Close()

	o := NewOllama(srv.URL, "embeddinggemma")
	res, err := o.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/api/embed" {
		t.Errorf("path = %q, want /api/embed", gotPath)
	}
	// Request body must carry model + input.
	if !strings.Contains(gotBody, `"model":"embeddinggemma"`) || !strings.Contains(gotBody, `"input":"hello world"`) {
		t.Errorf("request body missing model/input: %s", gotBody)
	}
	if len(res.Vector) != 3 || res.Vector[0] != 0.1 {
		t.Errorf("vector = %v, want [0.1 0.2 0.3]", res.Vector)
	}
	if res.Model != "embeddinggemma" {
		t.Errorf("model = %q, want embeddinggemma", res.Model)
	}
}

func TestOllamaEmbedNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	o := NewOllama(srv.URL, "embeddinggemma")
	if _, err := o.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestOllamaEmbedEmptyEmbeddingsIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"m","embeddings":[]}`)
	}))
	defer srv.Close()
	o := NewOllama(srv.URL, "m")
	if _, err := o.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error when no embedding returned")
	}
}

func TestOllamaModelFallsBackWhenResponseModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"embeddings":[[1,2]]}`) // no model field
	}))
	defer srv.Close()
	o := NewOllama(srv.URL, "configured-model")
	res, err := o.Embed(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "configured-model" {
		t.Errorf("model = %q, want configured-model (fallback to request model)", res.Model)
	}
}

func TestOllamaName(t *testing.T) {
	o := NewOllama("http://x", "nomic-embed-text")
	if o.Name() != "ollama:nomic-embed-text" {
		t.Errorf("Name() = %q", o.Name())
	}
}

// ensure the fake server's JSON is well-formed (guards the test fixtures).
func TestFixtureJSONValid(t *testing.T) {
	var v struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Embeddings) != 1 || len(v.Embeddings[0]) != 3 {
		t.Fatal("fixture shape wrong")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/embed/ -run TestOllama -v`
Expected: FAIL — `undefined: NewOllama`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/embed/ollama.go`:
```go
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Ollama embeds text via a local Ollama server's /api/embed endpoint.
type Ollama struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllama returns an Ollama backend for the given server base URL (e.g.
// "http://localhost:11434") and model (e.g. "embeddinggemma"). A 30s timeout
// bounds a hung server so the chain can fall through.
func NewOllama(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Name returns "ollama:<model>".
func (o *Ollama) Name() string { return "ollama:" + o.model }

type ollamaEmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed POSTs {model, input} to <baseURL>/api/embed and returns the first
// embedding. Errors on transport failure, non-200 status, unparseable body,
// or an empty embeddings array.
func (o *Ollama) Embed(ctx context.Context, text string) (Result, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: o.model, Input: text})
	if err != nil {
		return Result{}, fmt.Errorf("embed: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("embed: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("embed: %s: %w", o.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("embed: %s: status %d", o.Name(), resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return Result{}, fmt.Errorf("embed: read: %w", err)
	}
	var parsed ollamaEmbedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, fmt.Errorf("embed: parse: %w", err)
	}
	if len(parsed.Embeddings) == 0 || len(parsed.Embeddings[0]) == 0 {
		return Result{}, fmt.Errorf("embed: %s: empty embeddings", o.Name())
	}
	model := parsed.Model
	if model == "" {
		model = o.model
	}
	return Result{Vector: parsed.Embeddings[0], Model: model}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/embed/ -run 'TestOllama|TestFixture' -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/embed/ollama.go internal/embed/ollama_test.go
git commit -m "feat(embed): Ollama /api/embed backend"
```

---

## Task 3: Fallthrough Chain

**Files:**
- Create: `internal/embed/chain.go`
- Test: `internal/embed/chain_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/embed/chain_test.go`:
```go
package embed

import (
	"context"
	"errors"
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
	// Aggregated error should mention both backends so logs are actionable.
	msg := err.Error()
	if !contains(msg, "a") || !contains(msg, "b") {
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
	// Name reflects the ordered chain for diagnostics.
	if c.Name() == "" {
		t.Error("chain name should be non-empty")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/embed/ -run TestChain -v`
Expected: FAIL — `undefined: NewChain`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/embed/chain.go`:
```go
package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/embed/ -run TestChain -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/embed/chain.go internal/embed/chain_test.go
git commit -m "feat(embed): ordered fallthrough Chain embedder"
```

---

## Task 4: Final verification

- [ ] **Step 1: Vet + format**

Run:
```bash
go vet ./internal/embed/
gofmt -l internal/embed/
```
Expected: vet exit 0; gofmt prints nothing.

- [ ] **Step 2: Full package test + race + whole module**

Run:
```bash
go test ./internal/embed/ -v
go test -race ./internal/embed/
go build ./...
go test ./...
```
Expected: all pass; whole module unaffected (no other package imports `embed` yet).

- [ ] **Step 3: Confirm isolation**

Run: `git grep -l '"otto/internal/embed"' || echo "(not yet imported — correct)"`
Expected: only files inside `internal/embed/` reference the package (its own tests). No bot/store wiring yet — that's the next sub-plan.

---

## Self-Review notes

- **Spec coverage (this slice):** local embeddings via Ollama `/api/embed` (Task 2); ordered fallthrough chain embeddinggemma→nomic→… with all-fail signaling for the FTS5 floor decision (Task 3); cosine for brute-force top-k (Task 1). Deferred to later Plan-4 sub-plans (correctly out of scope): `state.db` vectors table + `SearchSemantic`, merging semantic+FTS5 in the otto-memory `session_search`, embedding turns on log, the idle rotator + token capture, and `setup.sh` Ollama install/pull.
- **Type consistency:** `Embedder{Embed(ctx,string)(Result,error); Name()string}`, `Result{Vector []float32; Model string}`, `Cosine([]float32,[]float32) float64`, `NewOllama(baseURL,model)`, `NewChain(...Embedder)`. `Ollama` and `Chain` both satisfy `Embedder`. Ollama response uses `[][]float32` so no float64→float32 conversion step is needed (encoding/json decodes JSON numbers straight into float32).
- **No new dependencies:** stdlib only; keeps the binary CGo-free and the dependency footprint unchanged.
- **Note for next sub-plan:** the chain returns an error when all backends fail — `session_search` should catch that and fall back to FTS5-only (the "keyword floor"), logging the degradation. The `Result.Model` tag must be persisted with each stored vector so a model/dimension change can trigger lazy re-embedding.
```
