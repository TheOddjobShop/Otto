package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/embed"
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

func TestHandleAddCapacityErrorPassesThroughAsIsError(t *testing.T) {
	s := newTestServer(t) // userCap = 1375 → 80% threshold = 1100 chars
	big := strings.Repeat("x", 1101)
	res, _, err := s.handleAdd(context.Background(), nil, addArgs{Target: "user", Content: big})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("over-capacity Add should be an IsError result")
	}
	if !strings.Contains(resultText(res), "consolidate") {
		t.Fatalf("capacity error should tell the model to consolidate, got: %q", resultText(res))
	}
}

func TestHandleSearchTruncatesLongContent(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	long := "Tokyo " + strings.Repeat("z", 1000)
	if _, err := s.store.AppendTurn(ctx, "otto", "assistant", long); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "Tokyo"})
	if err != nil || res.IsError {
		t.Fatalf("search failed: err=%v res=%q", err, resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "…") {
		t.Fatalf("long content should be truncated with an ellipsis: %q", text)
	}
	if strings.Count(text, "z") >= 1000 {
		t.Fatalf("full long content should not be echoed; got %d z's", strings.Count(text, "z"))
	}
}

func TestHandleSearchDefaultLimit(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if _, err := s.store.AppendTurn(ctx, "otto", "user", "Tokyo trip note"); err != nil {
			t.Fatalf("seed turn %d: %v", i, err)
		}
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "Tokyo"}) // no Limit → default 8
	if err != nil || res.IsError {
		t.Fatalf("search failed: err=%v res=%q", err, resultText(res))
	}
	if !strings.Contains(resultText(res), "8 matching") {
		t.Fatalf("default limit should cap at 8 results, got: %q", resultText(res))
	}
}

// fakeEmbedder returns a fixed vector regardless of input.
type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(ctx context.Context, text string) (embed.Result, error) {
	return embed.Result{Vector: f.vec, Model: "fake"}, nil
}
func (f fakeEmbedder) Name() string { return "fake" }

func TestHandleSearchMergesSemanticAndFTS(t *testing.T) {
	s := newTestServer(t)
	s.embedder = fakeEmbedder{vec: []float32{1, 0}}
	ctx := context.Background()
	kwID, _ := s.store.AppendTurn(ctx, "otto", "user", "keyword apple")
	semID, _ := s.store.AppendTurn(ctx, "otto", "assistant", "totally unrelated wording")
	if err := s.store.PutVector(ctx, semID, "fake", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	_ = kwID

	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "apple"})
	if err != nil || res.IsError {
		t.Fatalf("handleSearch: err=%v res=%q", err, resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "unrelated wording") {
		t.Errorf("semantic hit missing: %q", text)
	}
	if !strings.Contains(text, "keyword apple") {
		t.Errorf("keyword hit missing: %q", text)
	}
}

func TestHandleSearchNoEmbedderIsKeywordOnly(t *testing.T) {
	s := newTestServer(t) // embedder nil
	ctx := context.Background()
	if _, err := s.store.AppendTurn(ctx, "otto", "user", "keyword banana"); err != nil {
		t.Fatal(err)
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "banana"})
	if err != nil || res.IsError {
		t.Fatalf("handleSearch: err=%v", err)
	}
	if !strings.Contains(resultText(res), "banana") {
		t.Errorf("keyword-only search failed: %q", resultText(res))
	}
}

func TestMergeTurnsDedupesByIDSemanticFirst(t *testing.T) {
	semantic := []store.Turn{{ID: 1, Content: "a"}, {ID: 2, Content: "b"}}
	fts := []store.Turn{{ID: 2, Content: "b"}, {ID: 3, Content: "c"}}
	got := mergeTurns(semantic, fts, 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 unique, got %d: %+v", len(got), got)
	}
	if got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Errorf("merge order wrong: %+v", got)
	}
}

func TestMergeTurnsRespectsLimit(t *testing.T) {
	semantic := []store.Turn{{ID: 1}, {ID: 2}}
	fts := []store.Turn{{ID: 3}, {ID: 4}}
	got := mergeTurns(semantic, fts, 3)
	if len(got) != 3 {
		t.Fatalf("limit not respected: got %d", len(got))
	}
}

// failEmbedder always errors, simulating Ollama being down.
type failEmbedder struct{}

func (failEmbedder) Embed(ctx context.Context, text string) (embed.Result, error) {
	return embed.Result{}, context.DeadlineExceeded
}
func (failEmbedder) Name() string { return "fail" }

func TestHandleSearchFailingEmbedderFallsBackToKeyword(t *testing.T) {
	s := newTestServer(t)
	s.embedder = failEmbedder{}
	ctx := context.Background()
	if _, err := s.store.AppendTurn(ctx, "otto", "user", "keyword cherry"); err != nil {
		t.Fatal(err)
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "cherry"})
	if err != nil || res.IsError {
		t.Fatalf("failing embedder should degrade, not error: err=%v res=%q", err, resultText(res))
	}
	if !strings.Contains(resultText(res), "cherry") {
		t.Errorf("keyword fallback failed: %q", resultText(res))
	}
}

func TestHandleForwardEnqueuesForOtto(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	res, _, err := s.handleForward(ctx, nil, forwardArgs{
		Message: "send the gmail summary",
		Reason:  "user wants gmail summary",
	})
	if err != nil {
		t.Fatalf("handleForward transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleForward reported tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "Queued for Otto") {
		t.Fatalf("expected success text, got %q", resultText(res))
	}

	// One row should now be in the inbox, targeted at otto, sender=toto,
	// source=agent, with the body prefixed by the (from toto — reason) hint.
	msgs, err := s.store.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 enqueued message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Target != "otto" || m.Source != "agent" || m.Sender != "toto" {
		t.Errorf("bad routing: target=%q source=%q sender=%q", m.Target, m.Source, m.Sender)
	}
	if !strings.HasPrefix(m.Body, "(from toto — user wants gmail summary)") {
		t.Errorf("body missing reason prefix: %q", m.Body)
	}
	if !strings.Contains(m.Body, "send the gmail summary") {
		t.Errorf("body missing user message: %q", m.Body)
	}
}

func TestHandleForwardEmptyMessageIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleForward(context.Background(), nil, forwardArgs{
		Message: "   ",
		Reason:  "anything",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty message should be an IsError result")
	}
	if !strings.Contains(resultText(res), "message is empty") {
		t.Errorf("expected message-empty diagnostic, got %q", resultText(res))
	}
}

func TestHandleForwardEmptyReasonIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleForward(context.Background(), nil, forwardArgs{
		Message: "do the thing",
		Reason:  "",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty reason should be an IsError result")
	}
	if !strings.Contains(resultText(res), "reason is empty") {
		t.Errorf("expected reason-empty diagnostic, got %q", resultText(res))
	}
}

func TestHandleForwardRefusesInsideAgentHop(t *testing.T) {
	s := newTestServer(t)
	ctx := store.WithAgentHop(context.Background())

	res, _, err := s.handleForward(ctx, nil, forwardArgs{
		Message: "do the thing",
		Reason:  "user wants the thing",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("loop-guarded forward should be an IsError result")
	}
	if !strings.Contains(resultText(res), "3-hop cap") {
		t.Errorf("expected hop-cap diagnostic, got %q", resultText(res))
	}
}

func TestHandleMessageTotoEnqueuesForToto(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	res, _, err := s.handleMessageToto(ctx, nil, messageArgs{
		Message: "you cover this one, vibe check",
		Reason:  "user wants vibes",
	})
	if err != nil {
		t.Fatalf("handleMessageToto transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleMessageToto reported tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "Sent to Toto") {
		t.Fatalf("expected success text, got %q", resultText(res))
	}

	msgs, err := s.store.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 enqueued message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Target != "toto" || m.Source != "agent" || m.Sender != "otto" {
		t.Errorf("bad routing: target=%q source=%q sender=%q", m.Target, m.Source, m.Sender)
	}
	if !strings.HasPrefix(m.Body, "(from otto — user wants vibes)") {
		t.Errorf("body missing reason prefix: %q", m.Body)
	}
	if !strings.Contains(m.Body, "vibe check") {
		t.Errorf("body missing message: %q", m.Body)
	}
}

func TestHandleMessageTotoEmptyMessageIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleMessageToto(context.Background(), nil, messageArgs{
		Message: "   ",
		Reason:  "anything",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty message should be an IsError result")
	}
	if !strings.Contains(resultText(res), "message_toto refused") {
		t.Errorf("expected tool-named refusal, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "message is empty") {
		t.Errorf("expected message-empty diagnostic, got %q", resultText(res))
	}
}

func TestHandleMessageTotoEmptyReasonIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleMessageToto(context.Background(), nil, messageArgs{
		Message: "say hi",
		Reason:  "",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty reason should be an IsError result")
	}
	if !strings.Contains(resultText(res), "reason is empty") {
		t.Errorf("expected reason-empty diagnostic, got %q", resultText(res))
	}
}

func TestHandleMessageTotoRefusesInsideAgentHop(t *testing.T) {
	s := newTestServer(t)
	ctx := store.WithAgentHop(context.Background())

	res, _, err := s.handleMessageToto(ctx, nil, messageArgs{
		Message: "say hi",
		Reason:  "feeling friendly",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("loop-guarded message should be an IsError result")
	}
	if !strings.Contains(resultText(res), "message_toto refused: agent-to-agent conversation reached its 3-hop cap") {
		t.Errorf("expected hop-cap diagnostic, got %q", resultText(res))
	}
}

func TestHandleMessageTootEnqueuesForToot(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	res, _, err := s.handleMessageToot(ctx, nil, messageArgs{
		Message: "report items: 1) done. 2) done. 3) done.",
		Reason:  "finishing report",
	})
	if err != nil {
		t.Fatalf("handleMessageToot transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleMessageToot reported tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "Sent to Toot") {
		t.Fatalf("expected success text, got %q", resultText(res))
	}

	msgs, err := s.store.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 enqueued message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Target != "toot" || m.Source != "agent" || m.Sender != "otto" {
		t.Errorf("bad routing: target=%q source=%q sender=%q", m.Target, m.Source, m.Sender)
	}
	if !strings.HasPrefix(m.Body, "(from otto — finishing report)") {
		t.Errorf("body missing reason prefix: %q", m.Body)
	}
	if !strings.Contains(m.Body, "report items") {
		t.Errorf("body missing message: %q", m.Body)
	}
}

func TestHandleMessageTootEmptyMessageIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleMessageToot(context.Background(), nil, messageArgs{
		Message: "   ",
		Reason:  "anything",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty message should be an IsError result")
	}
	if !strings.Contains(resultText(res), "message_toot refused") {
		t.Errorf("expected tool-named refusal, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "message is empty") {
		t.Errorf("expected message-empty diagnostic, got %q", resultText(res))
	}
}

func TestHandleMessageTootEmptyReasonIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleMessageToot(context.Background(), nil, messageArgs{
		Message: "say hi",
		Reason:  "",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty reason should be an IsError result")
	}
	if !strings.Contains(resultText(res), "reason is empty") {
		t.Errorf("expected reason-empty diagnostic, got %q", resultText(res))
	}
}

func TestHandleMessageTootRefusesInsideAgentHop(t *testing.T) {
	s := newTestServer(t)
	ctx := store.WithAgentHop(context.Background())

	res, _, err := s.handleMessageToot(ctx, nil, messageArgs{
		Message: "filing one",
		Reason:  "bureaucratic vibes",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("loop-guarded message should be an IsError result")
	}
	if !strings.Contains(resultText(res), "message_toot refused: agent-to-agent conversation reached its 3-hop cap") {
		t.Errorf("expected hop-cap diagnostic, got %q", resultText(res))
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
