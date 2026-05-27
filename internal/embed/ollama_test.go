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
		io.WriteString(w, `{"embeddings":[[1,2]]}`)
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
