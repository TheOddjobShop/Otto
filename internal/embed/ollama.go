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
