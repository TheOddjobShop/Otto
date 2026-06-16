package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
	"unicode/utf8"
)

// maxEmbedInputBytes caps the text sent to the embedding model. Local embedding
// models have small context windows (~2k tokens); a giant input — e.g. a turn
// that dumped a full Notion backlog (~700KB) — makes Ollama stall and the
// request hit its deadline. Truncating keeps embeds fast and is no real loss:
// one vector for hundreds of KB is a meaningless average, and the leading slice
// is a fine representative for semantic recall. ~8KB ≈ the models' window.
const maxEmbedInputBytes = 8 * 1024

// truncateForEmbed clamps text to maxEmbedInputBytes on a UTF-8 rune boundary.
func truncateForEmbed(text string) string {
	if len(text) <= maxEmbedInputBytes {
		return text
	}
	cut := maxEmbedInputBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	// If the whole prefix was continuation bytes (malformed UTF-8), cut==0 would
	// drop everything and trigger a downstream 'empty embeddings' failure. Fall
	// back to the byte-capped prefix instead; JSON marshalling will sanitize the
	// invalid bytes, keeping the input non-empty and the size guard intact.
	if cut == 0 {
		return text[:maxEmbedInputBytes]
	}
	return text[:cut]
}

// Compile-time assertion that *Ollama satisfies Embedder.
var _ Embedder = (*Ollama)(nil)

// Ollama embeds text via a local Ollama server's /api/embed endpoint.
type Ollama struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllama returns an Ollama backend for the given server base URL (e.g.
// "http://localhost:11434") and model (e.g. "embeddinggemma"). The http.Client
// timeout is set to 90s — comfortably above the per-backend context budget
// (60s) assigned by Chain.Embed — so the context cancel wins rather than a
// racing transport-level deadline.
func NewOllama(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 90 * time.Second},
	}
}

// Name returns "ollama:<model>".
func (o *Ollama) Name() string { return "ollama:" + o.model }

type ollamaEmbedRequest struct {
	Model     string `json:"model"`
	Input     string `json:"input"`
	KeepAlive string `json:"keep_alive"` // e.g. "2h" — prevents cold-load on every turn
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed POSTs {model, input, keep_alive} to <baseURL>/api/embed and returns
// the first embedding. The keep_alive field tells Ollama to hold the model in
// memory for 2 hours, avoiding the cold-load (~1–14s) that would otherwise
// happen after Ollama's default 5-minute idle eviction.
// Errors on transport failure, non-200 status, unparseable body, or an empty
// embeddings array.
func (o *Ollama) Embed(ctx context.Context, text string) (Result, error) {
	text = truncateForEmbed(text)
	body, err := json.Marshal(ollamaEmbedRequest{Model: o.model, Input: text, KeepAlive: "2h"})
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
	// Decode the response body as it streams rather than buffering up to 8 MB
	// into an intermediate []byte — a single embedding vector is a few KB, so
	// the buffer was never needed. The LimitReader preserves the size guard.
	var parsed ollamaEmbedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&parsed); err != nil {
		return Result{}, fmt.Errorf("embed: parse: %w", err)
	}
	if len(parsed.Embeddings) == 0 || len(parsed.Embeddings[0]) == 0 {
		return Result{}, fmt.Errorf("embed: %s: empty embeddings", o.Name())
	}
	vec := parsed.Embeddings[0]
	for _, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return Result{}, fmt.Errorf("embed: %s: non-finite embedding component", o.Name())
		}
	}
	model := parsed.Model
	if model == "" {
		model = o.model
	}
	return Result{Vector: vec, Model: model}, nil
}
