package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/memory"
	"otto/internal/store"
)

func newTestServer(t *testing.T) *memoryServer {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/state.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &memoryServer{
		core:  memory.NewCore(dir, 2200, 1375),
		store: st,
	}
}

func TestHandleAddThenItAppearsInFiles(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	res, _, err := s.handleAdd(ctx, nil, addArgs{Target: "user", Content: "User is named Justin."})
	if err != nil {
		t.Fatalf("handleAdd returned transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleAdd reported tool error: %s", resultText(res))
	}
	user, _, _ := s.core.Load()
	if !strings.Contains(user, "Justin") {
		t.Fatalf("added content not persisted: %q", user)
	}
}

func TestHandleAddRejectsBadTarget(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleAdd(context.Background(), nil, addArgs{Target: "bogus", Content: "x"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError result for bad target")
	}
}

func TestHandleAddSurfacesDomainErrorAsIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleAdd(context.Background(), nil, addArgs{Target: "user", Content: "sk-ant-api03-shouldBeRejected"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("secret content should produce an IsError tool result")
	}
}

func TestHandleReplaceAndRemove(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if _, _, err := s.handleAdd(ctx, nil, addArgs{Target: "memory", Content: "Server runs Ubuntu."}); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	res, _, err := s.handleReplace(ctx, nil, replaceArgs{Target: "memory", OldText: "Ubuntu", Content: "Arch Linux"})
	if err != nil || res.IsError {
		t.Fatalf("handleReplace failed: err=%v res=%q", err, resultText(res))
	}
	_, mem, _ := s.core.Load()
	if !strings.Contains(mem, "Arch Linux") || strings.Contains(mem, "Ubuntu") {
		t.Fatalf("replace not applied: %q", mem)
	}
	res, _, err = s.handleRemove(ctx, nil, removeArgs{Target: "memory", OldText: "Server runs Arch Linux."})
	if err != nil || res.IsError {
		t.Fatalf("handleRemove failed: err=%v res=%q", err, resultText(res))
	}
	_, mem, _ = s.core.Load()
	if strings.Contains(mem, "Arch Linux") {
		t.Fatalf("entry not removed: %q", mem)
	}
}

func TestHandleRemoveMissingIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleRemove(context.Background(), nil, removeArgs{Target: "memory", OldText: "not there"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("removing missing text should be an IsError result")
	}
}

func TestHandleSearchFindsTurns(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if _, err := s.store.AppendTurn(ctx, "otto", "user", "remind me about the Tokyo trip"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	if _, err := s.store.AppendTurn(ctx, "otto", "assistant", "your Tokyo flight is at 9am"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "Tokyo"})
	if err != nil {
		t.Fatalf("handleSearch transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleSearch reported error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "Tokyo") {
		t.Fatalf("search result should mention the matched content: %q", text)
	}
}

func TestHandleSearchNoMatchesIsNotError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleSearch(context.Background(), nil, searchArgs{Query: "nonexistent"})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatal("a no-match search is a normal empty result, not an error")
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "no") {
		t.Fatalf("empty search should say so, got: %q", resultText(res))
	}
}

// resultText extracts the concatenated text of a tool result for assertions.
func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
