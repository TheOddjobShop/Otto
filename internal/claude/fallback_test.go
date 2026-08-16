package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The local backstop, tested through the seam that matters: what reaches the
// caller's event channel. Everything downstream of Runner — the handler, the
// token tracker, the voice bridge — only ever sees that channel, so if the
// events are right the rest of the tree cannot tell which brain answered.

// ─── Stubs ───────────────────────────────────────────────────────────────

// stubRunner plays a scripted turn: some events, then an error or nil.
type stubRunner struct {
	events []Event
	err    error
	calls  int
	gotEnv map[string]string
}

func (s *stubRunner) Run(ctx context.Context, args RunArgs) error {
	s.calls++
	for _, ev := range s.events {
		args.Events <- ev
	}
	return s.err
}

func (s *stubRunner) WithEnv(extra map[string]string) Runner {
	return &stubRunner{events: s.events, err: s.err, gotEnv: extra}
}

// stubFallback records what it was asked and returns a canned reply.
type stubFallback struct {
	mu        sync.Mutex
	reply     string
	availErr  error
	genErr    error
	calls     int
	gotPrompt string
	gotSystem string
}

func (f *stubFallback) Available(ctx context.Context) error { return f.availErr }
func (f *stubFallback) Name() string                        { return "stub-fallback" }

func (f *stubFallback) Generate(ctx context.Context, args RunArgs, events chan<- Event) error {
	f.mu.Lock()
	f.calls++
	f.gotPrompt = args.Prompt
	f.gotSystem = args.AppendSystemPrompt
	f.mu.Unlock()
	if f.genErr != nil {
		return f.genErr
	}
	events <- AssistantTextEvent{Text: f.reply}
	events <- ResultEvent{Subtype: "success"}
	return nil
}

func (f *stubFallback) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// runTurn drives a runner and returns everything it emitted.
func runTurn(t *testing.T, r Runner, args RunArgs) ([]Event, error) {
	t.Helper()
	events := make(chan Event, 64)
	args.Events = events
	var got []Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			got = append(got, ev)
		}
	}()
	err := r.Run(context.Background(), args)
	close(events)
	<-done
	return got, err
}

func textOf(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if t, ok := ev.(AssistantTextEvent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// ─── When the primary works ──────────────────────────────────────────────

func TestPrimarySuccessNeverTouchesTheFallback(t *testing.T) {
	primary := &stubRunner{events: []Event{
		AssistantTextEvent{Text: "here you go"},
		ResultEvent{Subtype: "success", InputTokens: 10},
	}}
	fb := &stubFallback{reply: "local"}
	r := NewFallbackRunner(primary, fb, quietLogger())

	got, err := runTurn(t, r, RunArgs{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fb.count() != 0 {
		t.Error("the fallback ran even though Claude succeeded")
	}
	if txt := textOf(got); txt != "here you go" {
		t.Errorf("text = %q, want Claude's reply untouched", txt)
	}
}

// A nil fallback must leave the primary exactly as it was, so callers need no
// conditional around the wrap.
func TestNilFallbackReturnsPrimaryUnchanged(t *testing.T) {
	primary := &stubRunner{}
	if got := NewFallbackRunner(primary, nil, quietLogger()); got != Runner(primary) {
		t.Error("a nil fallback should return the primary itself")
	}
}

// ─── When the primary fails ──────────────────────────────────────────────

func TestClaudeFailureIsAnsweredLocally(t *testing.T) {
	primary := &stubRunner{err: errors.New("claude: exit status 1: overloaded_error")}
	fb := &stubFallback{reply: "local answer"}
	r := NewFallbackRunner(primary, fb, quietLogger())

	got, err := runTurn(t, r, RunArgs{Prompt: "what's up", AppendSystemPrompt: "you are otto"})
	if err != nil {
		t.Fatalf("Run should succeed via the fallback, got %v", err)
	}
	if fb.count() != 1 {
		t.Fatalf("fallback ran %d times, want 1", fb.count())
	}
	if fb.gotPrompt != "what's up" || fb.gotSystem != "you are otto" {
		t.Errorf("fallback got prompt=%q system=%q, want the originals forwarded", fb.gotPrompt, fb.gotSystem)
	}
	txt := textOf(got)
	if !strings.Contains(txt, "local answer") {
		t.Errorf("text = %q, want the local reply", txt)
	}
	// The user must be able to tell which brain answered.
	if !strings.Contains(strings.ToLower(txt), "unreachable") {
		t.Errorf("text = %q, want a notice that this is the degraded path", txt)
	}
	if !strings.HasPrefix(txt, "Heads up") {
		t.Errorf("text = %q, want the notice first so it is read (and spoken) first", txt)
	}
}

// A result event that says the turn failed is a failure even though Run
// returned nil.
func TestNonSuccessResultFallsBack(t *testing.T) {
	primary := &stubRunner{events: []Event{
		ResultEvent{Subtype: "error_during_execution", Error: "boom"},
	}}
	fb := &stubFallback{reply: "local answer"}
	r := NewFallbackRunner(primary, fb, quietLogger())

	got, err := runTurn(t, r, RunArgs{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fb.count() != 1 {
		t.Fatalf("fallback ran %d times, want 1", fb.count())
	}
	// The success result must come last, or the handler would still treat the
	// turn as failed and drop the reply it just produced.
	var lastResult ResultEvent
	for _, ev := range got {
		if r, ok := ev.(ResultEvent); ok {
			lastResult = r
		}
	}
	if lastResult.Subtype != "success" {
		t.Errorf("final result = %q, want the fallback's success to be last", lastResult.Subtype)
	}
}

// ─── When falling back would be wrong ────────────────────────────────────

// Cancellation is somebody deciding the turn should stop: /restart, shutdown,
// the hang watchdog. Answering it anyway on a second brain ignores that.
func TestCancellationDoesNotFallBack(t *testing.T) {
	primary := &stubRunner{err: context.Canceled}
	fb := &stubFallback{reply: "local"}
	r := NewFallbackRunner(primary, fb, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := make(chan Event, 8)
	err := r.Run(ctx, RunArgs{Prompt: "hi", Events: events})

	if fb.count() != 0 {
		t.Error("the fallback ran on a cancelled turn")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation surfaced", err)
	}
}

// Claude can fail partway through a reply that was largely fine. Appending a
// second, worse answer would leave the user with two and no way to choose.
func TestPartialClaudeReplyIsNotSecondGuessed(t *testing.T) {
	primary := &stubRunner{
		events: []Event{AssistantTextEvent{Text: "I checked your calendar and"}},
		err:    errors.New("claude: exit status 1"),
	}
	fb := &stubFallback{reply: "local"}
	r := NewFallbackRunner(primary, fb, quietLogger())

	got, err := runTurn(t, r, RunArgs{Prompt: "hi"})
	if fb.count() != 0 {
		t.Error("the fallback overwrote a partial answer that had already been sent")
	}
	if err == nil {
		t.Error("the original failure should still surface")
	}
	if txt := textOf(got); txt != "I checked your calendar and" {
		t.Errorf("text = %q, want the partial reply preserved", txt)
	}
}

// ─── When the fallback is also broken ────────────────────────────────────

func TestUnavailableFallbackSurfacesBothFailures(t *testing.T) {
	primary := &stubRunner{err: errors.New("claude: exit status 1")}
	fb := &stubFallback{availErr: errors.New("ollama unreachable")}
	r := NewFallbackRunner(primary, fb, quietLogger())

	_, err := runTurn(t, r, RunArgs{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected an error when neither brain works")
	}
	for _, want := range []string{"exit status 1", "ollama unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

func TestFallbackGenerationFailureSurfacesBoth(t *testing.T) {
	primary := &stubRunner{err: errors.New("claude: exit status 1")}
	fb := &stubFallback{genErr: errors.New("model crashed")}
	r := NewFallbackRunner(primary, fb, quietLogger())

	_, err := runTurn(t, r, RunArgs{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected an error when the fallback fails too")
	}
	if !strings.Contains(err.Error(), "model crashed") {
		t.Errorf("err = %q, want the fallback failure named", err)
	}
}

// ─── Bookkeeping ─────────────────────────────────────────────────────────

func TestStatsCountServedTurns(t *testing.T) {
	primary := &stubRunner{err: errors.New("claude: 429 rate limit")}
	fb := &stubFallback{reply: "ok"}
	r := NewFallbackRunner(primary, fb, quietLogger()).(*FallbackRunner)

	runTurn(t, r, RunArgs{Prompt: "one"})
	runTurn(t, r, RunArgs{Prompt: "two"})

	used, reason := r.Stats()
	if used != 2 {
		t.Errorf("served = %d, want 2", used)
	}
	if !strings.Contains(reason, "rate-limited") {
		t.Errorf("reason = %q, want the classified cause", reason)
	}
}

func TestWithEnvKeepsTheWrapper(t *testing.T) {
	r := NewFallbackRunner(&stubRunner{}, &stubFallback{}, quietLogger())
	if _, ok := r.WithEnv(map[string]string{"A": "B"}).(*FallbackRunner); !ok {
		t.Error("WithEnv unwrapped the fallback, so an env-scoped runner would lose it")
	}
}

func TestClassifyFailureNamesTheCause(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"exec: \"claude\": executable file not found in $PATH", "claude binary missing"},
		{"claude: exit status 1: 401 Unauthorized", "claude auth failed"},
		{"claude: 429 rate_limit_error", "claude rate-limited or overloaded"},
		{"claude: overloaded_error", "claude rate-limited or overloaded"},
		{"claude: 503 Service Unavailable", "claude api error"},
		{"dial tcp 1.2.3.4:443: connect: connection refused", "network unreachable"},
		{"claude: something nobody predicted", "claude failed"},
	} {
		if got := classifyFailure(errors.New(tc.err)); got != tc.want {
			t.Errorf("classifyFailure(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// ─── The Ollama backend ──────────────────────────────────────────────────

// fakeOllama serves /api/tags and a streaming /api/chat.
func fakeOllama(t *testing.T, models []string, chunks []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		type m struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		}
		out := struct {
			Models []m `json:"models"`
		}{}
		for _, name := range models {
			out.Models = append(out.Models, m{Name: name, Model: name})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, c := range chunks {
			fmt.Fprintf(w, `{"message":{"content":%q},"done":false}`+"\n", c)
		}
		fmt.Fprint(w, `{"done":true,"done_reason":"stop","prompt_eval_count":12,"eval_count":34}`+"\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOllamaAvailabilityDistinguishesItsFailures(t *testing.T) {
	srv := fakeOllama(t, []string{"gpt-oss:20b"}, nil)

	if err := NewOllamaFallback(srv.URL, "gpt-oss:20b").Available(context.Background()); err != nil {
		t.Errorf("Available: %v, want nil for a pulled model", err)
	}
	// A bare name should match the pulled tag rather than look missing.
	if err := NewOllamaFallback(srv.URL, "gpt-oss").Available(context.Background()); err != nil {
		t.Errorf("Available(bare name): %v, want it to match gpt-oss:20b", err)
	}
	err := NewOllamaFallback(srv.URL, "llama9:70b").Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ollama pull") {
		t.Errorf("missing model error = %v, want it to name the fix", err)
	}
	err = NewOllamaFallback("http://127.0.0.1:1", "gpt-oss:20b").Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("down-server error = %v, want it to say unreachable", err)
	}
}

func TestOllamaStreamsChunksAsTheyArrive(t *testing.T) {
	srv := fakeOllama(t, []string{"gpt-oss:20b"}, []string{"Three ", "things ", "today."})
	fb := NewOllamaFallback(srv.URL, "gpt-oss:20b")

	events := make(chan Event, 32)
	if err := fb.Generate(context.Background(), RunArgs{Prompt: "hi"}, events); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	close(events)

	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	// Separate events, not one blob: the voice path speaks sentences as they
	// form, and a single final event would make the fallback silent until the
	// whole reply had generated.
	var texts int
	for _, ev := range got {
		if _, ok := ev.(AssistantTextEvent); ok {
			texts++
		}
	}
	if texts != 3 {
		t.Errorf("emitted %d text events, want one per streamed chunk", texts)
	}
	if txt := textOf(got); txt != "Three things today." {
		t.Errorf("text = %q", txt)
	}
}

// Local inference is free, and the tracker prices everything it records
// against a Claude rate card — reporting real counts would invent a cost.
func TestOllamaReportsNoTokenUsage(t *testing.T) {
	srv := fakeOllama(t, []string{"gpt-oss:20b"}, []string{"hi"})
	fb := NewOllamaFallback(srv.URL, "gpt-oss:20b")

	events := make(chan Event, 8)
	fb.Generate(context.Background(), RunArgs{Prompt: "hi"}, events)
	close(events)

	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			if r.InputTokens != 0 || r.OutputTokens != 0 || r.ContextTokens != 0 {
				t.Errorf("result carried token counts %+v, want zeros", r)
			}
		}
	}
}

// Ollama has no sessions, so without a rolling history every fallback turn
// would be amnesiac — and an outage lasting more than one question is normal.
func TestOllamaCarriesConversationAcrossTurns(t *testing.T) {
	var mu sync.Mutex
	var lastReq ollamaChatRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&lastReq)
		mu.Unlock()
		fmt.Fprint(w, `{"message":{"content":"noted"},"done":false}`+"\n")
		fmt.Fprint(w, `{"done":true}`+"\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Snapshot under the lock and release it before the next request — the
	// handler needs the same mutex, so holding it across a call deadlocks.
	sentMessages := func() string {
		mu.Lock()
		defer mu.Unlock()
		var b strings.Builder
		for _, m := range lastReq.Messages {
			b.WriteString(m.Role + ":" + m.Content + "\n")
		}
		return b.String()
	}

	fb := NewOllamaFallback(srv.URL, "gpt-oss:20b")
	ask := func(prompt string) {
		t.Helper()
		events := make(chan Event, 8)
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range events {
			}
		}()
		if err := fb.Generate(context.Background(), RunArgs{Prompt: prompt}, events); err != nil {
			t.Fatalf("Generate(%q): %v", prompt, err)
		}
		close(events)
		<-drained
	}

	ask("my name is alice")
	ask("what's my name")
	if got := sentMessages(); !strings.Contains(got, "my name is alice") {
		t.Errorf("second request messages =\n%s\nwant the first exchange carried forward", got)
	}

	fb.ForgetHistory()
	ask("still there")
	if got := sentMessages(); strings.Contains(got, "my name is alice") {
		t.Errorf("ForgetHistory left the old conversation in the prompt:\n%s", got)
	}
}

// The system prompt has to say the tools are gone, or a model carrying Otto's
// persona will happily claim to have sent the email.
func TestOllamaSystemPromptDisclaimsTools(t *testing.T) {
	var mu sync.Mutex
	var lastReq ollamaChatRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&lastReq)
		mu.Unlock()
		fmt.Fprint(w, `{"message":{"content":"ok"},"done":true}`+"\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fb := NewOllamaFallback(srv.URL, "gpt-oss:20b")
	events := make(chan Event, 8)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range events {
		}
	}()
	if err := fb.Generate(context.Background(), RunArgs{Prompt: "email bob", AppendSystemPrompt: "You are Otto."}, events); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	close(events)
	<-drained

	mu.Lock()
	defer mu.Unlock()

	if len(lastReq.Messages) == 0 || lastReq.Messages[0].Role != "system" {
		t.Fatal("first message should be the system prompt")
	}
	sys := lastReq.Messages[0].Content
	if !strings.Contains(sys, "You are Otto.") {
		t.Error("Otto's own prompt should survive — persona and memory live there")
	}
	if !strings.Contains(sys, "NO\ntools") && !strings.Contains(sys, "NO tools") {
		t.Errorf("system prompt does not disclaim tools:\n%s", sys)
	}
}
