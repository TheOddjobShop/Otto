package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
)

// Memory core character caps (rough token proxies). Promote to config in a
// later plan if they need to be tunable per deployment.
const (
	memCapChars  = 2200 // MEMORY.md ~800 tokens
	userCapChars = 1375 // USER.md   ~500 tokens
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	memDir := flag.String("memory-dir", "", "directory holding USER.md and MEMORY.md (required)")
	stateDB := flag.String("state-db", "", "path to the SQLite turn-log database (required)")
	embedURL := flag.String("embed-url", "http://localhost:11434", "Ollama base URL for semantic search embeddings")
	embedModels := flag.String("embed-models", "embeddinggemma,nomic-embed-text", "comma-separated Ollama embedding models, tried in order")
	flag.Parse()

	if *memDir == "" || *stateDB == "" {
		return fmt.Errorf("otto-memory: --memory-dir and --state-db are required")
	}

	st, err := store.Open(*stateDB)
	if err != nil {
		return fmt.Errorf("otto-memory: open store: %w", err)
	}
	defer st.Close()

	srv := &memoryServer{
		core:  memory.NewCore(*memDir, memCapChars, userCapChars),
		store: st,
	}

	var models []string
	for _, m := range strings.Split(*embedModels, ",") {
		if s := strings.TrimSpace(m); s != "" {
			models = append(models, s)
		}
	}
	srv.embedder = embed.NewOllamaChain(*embedURL, models)

	server := mcp.NewServer(&mcp.Implementation{Name: "otto-memory", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_add",
		Description: "Save a durable fact to long-term memory. Use for corrections, discovered preferences, environment facts, project conventions, and lessons — not ephemera. target is \"user\" (about the person) or \"memory\" (everything else).",
	}, srv.handleAdd)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_replace",
		Description: "Replace a unique existing memory entry with updated text. Used to consolidate or correct facts. Matching is raw substring; pass a distinctive snippet.",
	}, srv.handleReplace)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_remove",
		Description: "Delete a unique existing memory entry. Matching is raw substring; pass a distinctive snippet.",
	}, srv.handleRemove)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "session_search",
		Description: "Search past conversation turns by meaning AND keyword (semantic + keyword retrieval). Use for \"what did we discuss about X\" and fuzzy recall where you don't remember the exact words.",
	}, srv.handleSearch)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "forward_to_otto",
		Description: "Hand off a user message to Otto. Use when the user's request is actual work Otto should handle (running code, sending email, files, calendar, anything beyond chat). message = the user's request in their voice; reason = a one-line \"why I forwarded\" shown to the user in the visible banner. Do NOT forward chitchat or questions about Toto himself. Refused inside a nested agent dispatch.",
	}, srv.handleForward)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("otto-memory: server exited: %w", err)
	}
	return nil
}
