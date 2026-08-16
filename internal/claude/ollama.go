package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The local backstop.
//
// Claude Code is a network service behind a subprocess: it fails when the API
// is down or overloaded, when auth expires, when the machine is offline, and
// when the binary itself is missing after a botched npm update. Every one of
// those turns Otto into a bot that answers "⚠️ Claude error" and nothing else —
// on a home server that is often the exact moment you wanted to ask it
// something.
//
// So a second brain runs on the same box: Ollama, serving gpt-oss:20b. It has
// no tools, no MCP, no memory writes and no session — it can only talk. That is
// a large step down and the reply says so, because a degraded answer the user
// mistakes for a full one is worse than an error.

// Fallback is a degraded backend used when the primary Runner cannot run.
type Fallback interface {
	// Available reports why the fallback cannot serve a turn, or nil.
	Available(ctx context.Context) error
	// Generate streams a reply into events as AssistantTextEvent chunks,
	// finishing with a ResultEvent. It must not close events.
	Generate(ctx context.Context, args RunArgs, events chan<- Event) error
	// Name identifies the backend in logs and status output.
	Name() string
}

// OllamaFallback talks to a local Ollama server's /api/chat endpoint.
type OllamaFallback struct {
	baseURL string
	model   string
	client  *http.Client

	// history is the recent exchange log for this backend.
	//
	// Claude Code owns conversation state through --resume; Ollama has none, so
	// without this every fallback turn would be amnesiac. An outage lasting
	// more than one question is the normal case, not the exotic one, and
	// "what did I just say" is the first thing that breaks.
	mu      sync.Mutex
	history []ollamaMessage
}

// NewOllamaFallback returns a fallback backed by the given server and model.
//
// The HTTP timeout is generous because a 20-billion-parameter model on CPU is
// slow and a cold load adds seconds on top; the per-call context is the real
// bound. Nothing here pre-warms the model — doing so would hold gigabytes
// resident for an outage that may never come.
func NewOllamaFallback(baseURL, model string) *OllamaFallback {
	return &OllamaFallback{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

// Name returns "ollama:<model>".
func (o *OllamaFallback) Name() string { return "ollama:" + o.model }

// Model returns the configured model name.
func (o *OllamaFallback) Model() string { return o.model }

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	KeepAlive string          `json:"keep_alive"`
}

type ollamaChatChunk struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	Error           string `json:"error"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

type ollamaTagsResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

// Available checks that the server answers and that the model is actually
// pulled.
//
// Both halves matter and fail differently: an unreachable server means Ollama
// is not running, while a reachable server missing the model means someone has
// to run `ollama pull`. Reporting them as one vague error would send the user
// looking in the wrong place during an outage, which is the worst possible time
// for a misleading diagnostic.
func (o *OllamaFallback) Available(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama unreachable at %s: %w", o.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama at %s: status %d", o.baseURL, resp.StatusCode)
	}
	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return fmt.Errorf("ollama tags: %w", err)
	}
	for _, m := range tags.Models {
		// Ollama reports "gpt-oss:20b"; a config that says "gpt-oss" should
		// still match the pulled tag rather than claim it is missing.
		for _, name := range []string{m.Name, m.Model} {
			if name == o.model || strings.HasPrefix(name, o.model+":") {
				return nil
			}
		}
	}
	return fmt.Errorf("ollama model %q not pulled — run `ollama pull %s`", o.model, o.model)
}

// maxFallbackHistory is how many messages (user and assistant, counted
// together) the backend carries between turns. Six is three exchanges: enough
// for "do the second one" to resolve, short enough that a 20B model on CPU is
// not re-reading an essay before every reply.
const maxFallbackHistory = 6

// maxFallbackReplyBytes bounds what one reply contributes to that history, so a
// single runaway answer cannot crowd out everything before it.
const maxFallbackReplyBytes = 4000

// Generate streams a reply from Ollama, emitting the same events a Claude turn
// would so nothing downstream — the handler, the voice bridge, the TUI, the
// activity log — needs to know which brain answered.
func (o *OllamaFallback) Generate(ctx context.Context, args RunArgs, events chan<- Event) error {
	system := strings.TrimSpace(args.AppendSystemPrompt)
	system = strings.TrimSpace(system + "\n\n" + fallbackSystemClause)

	o.mu.Lock()
	msgs := make([]ollamaMessage, 0, len(o.history)+2)
	msgs = append(msgs, ollamaMessage{Role: "system", Content: system})
	msgs = append(msgs, o.history...)
	o.mu.Unlock()
	msgs = append(msgs, ollamaMessage{Role: "user", Content: args.Prompt})

	body, err := json.Marshal(ollamaChatRequest{
		Model:    o.model,
		Messages: msgs,
		Stream:   true,
		// Held resident for an hour: an outage that produced one question
		// usually produces several, and a cold reload between them is the
		// difference between slow and unusable.
		KeepAlive: "1h",
	})
	if err != nil {
		return fmt.Errorf("fallback: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fallback: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("fallback: %s: %w", o.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fallback: %s: status %d", o.Name(), resp.StatusCode)
	}

	var reply strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	// One NDJSON line per token is short, but the final line carries the whole
	// usage block and a long done_reason; give the scanner room rather than
	// failing the turn on a token boundary.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			// A malformed line mid-stream is not worth discarding a reply
			// that is otherwise arriving fine.
			continue
		}
		if chunk.Error != "" {
			return fmt.Errorf("fallback: %s: %s", o.Name(), chunk.Error)
		}
		if chunk.Message.Content != "" {
			reply.WriteString(chunk.Message.Content)
			emit(ctx, events, AssistantTextEvent{Text: chunk.Message.Content})
		}
		if chunk.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("fallback: %s: read: %w", o.Name(), err)
	}

	out := strings.TrimSpace(reply.String())
	if out == "" {
		return fmt.Errorf("fallback: %s: empty reply", o.Name())
	}
	o.remember(args.Prompt, out)

	// Token counts are deliberately left at zero. They are real, but they are
	// local and free, and the tracker prices everything it records against a
	// Claude rate card — feeding them in would invent a dollar cost for
	// electricity. ContextTokens stays zero too, so the session rotator does
	// not treat a fallback turn as growth in a Claude session that never ran.
	emit(ctx, events, ResultEvent{Subtype: "success"})
	return nil
}

// remember appends one exchange to the rolling history.
func (o *OllamaFallback) remember(prompt, reply string) {
	if len(reply) > maxFallbackReplyBytes {
		reply = reply[:maxFallbackReplyBytes]
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.history = append(o.history,
		ollamaMessage{Role: "user", Content: prompt},
		ollamaMessage{Role: "assistant", Content: reply},
	)
	if len(o.history) > maxFallbackHistory {
		o.history = append([]ollamaMessage(nil), o.history[len(o.history)-maxFallbackHistory:]...)
	}
}

// ForgetHistory drops the rolling context. Called when Otto's session is
// cleared, so /new means the same thing on both brains.
func (o *OllamaFallback) ForgetHistory() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.history = nil
}

// fallbackSystemClause is appended to whatever system prompt Otto composed.
//
// It is short on purpose. The persona, the time block and the memory core are
// already in there; this only has to correct the things that are now false —
// the tools are gone, and the model answering is not the one the prompt was
// written for.
const fallbackSystemClause = `───────────────────────────────────────────────
  OFFLINE FALLBACK — REDUCED CAPABILITY
───────────────────────────────────────────────

Claude Code is unreachable, so you are running on a local model with NO
tools: no file access, no shell, no Notion, Gmail, Drive or Calendar, no
memory writes, no web. Nothing outside this conversation is available.

Answer from what you know and from the conversation above. If the request
genuinely needs a tool, say so plainly in one sentence and offer what you
can — do not pretend to have done it, and do not invent file contents,
message contents, calendar entries or search results. Keep it brief.`

// emit sends an event unless the caller has gone away. The channel is buffered
// and drained by the handler, so this only ever blocks briefly.
func emit(ctx context.Context, events chan<- Event, ev Event) {
	if events == nil {
		return
	}
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}
