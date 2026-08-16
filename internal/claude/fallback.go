package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// FallbackRunner wraps a primary Runner and reroutes a failed turn to a local
// backend rather than surfacing an error the user can do nothing about.
//
// It sits at the Runner seam deliberately. Everything downstream — the handler,
// the token tracker, the activity log, the voice bridge, the TUI — consumes the
// same Event stream either way, so nothing else in the tree learns that a
// second brain exists.
type FallbackRunner struct {
	primary  Runner
	fallback Fallback
	logger   *log.Logger

	// used counts turns the fallback actually served, for /status.
	used atomic.Int64
	mu   sync.Mutex
	last string // why the primary failed, most recently
}

// NewFallbackRunner wraps primary so that a failed turn is retried locally.
// A nil fallback returns primary unchanged, so callers need no conditional.
func NewFallbackRunner(primary Runner, fb Fallback, logger *log.Logger) Runner {
	if fb == nil {
		return primary
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &FallbackRunner{primary: primary, fallback: fb, logger: logger}
}

// WithEnv threads through to the primary, keeping the wrapper in place.
func (f *FallbackRunner) WithEnv(extra map[string]string) Runner {
	return &FallbackRunner{
		primary:  f.primary.WithEnv(extra),
		fallback: f.fallback,
		logger:   f.logger,
	}
}

// Stats reports how many turns the fallback has served and why it last fired.
func (f *FallbackRunner) Stats() (used int64, lastReason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used.Load(), f.last
}

// Fallback exposes the backend, for status output and availability probes.
func (f *FallbackRunner) Fallback() Fallback { return f.fallback }

// Run tries the primary, then the fallback.
func (f *FallbackRunner) Run(ctx context.Context, args RunArgs) error {
	out := args.Events

	// Watch the primary's events on the way past. Two things are needed: did
	// any assistant text actually reach the user, and did the turn end in a
	// non-success result.
	inner := make(chan Event, 64)
	var sawText atomic.Bool
	var badResult atomic.Bool
	var resultDetail atomic.Value

	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for ev := range inner {
			switch e := ev.(type) {
			case AssistantTextEvent:
				if strings.TrimSpace(e.Text) != "" {
					sawText.Store(true)
				}
			case ResultEvent:
				if e.Subtype != "" && e.Subtype != "success" {
					badResult.Store(true)
					detail := e.Subtype
					if e.Error != "" {
						detail += ": " + e.Error
					}
					resultDetail.Store(detail)
				}
			}
			emit(ctx, out, ev)
		}
	}()

	primaryArgs := args
	primaryArgs.Events = inner
	err := f.primary.Run(ctx, primaryArgs)
	close(inner)
	<-forwarded

	reason, should := shouldFallback(ctx, err, sawText.Load(), badResult.Load())
	if !should {
		return err
	}
	if d, ok := resultDetail.Load().(string); ok && err == nil {
		reason = "claude returned " + d
	}

	if avErr := f.fallback.Available(ctx); avErr != nil {
		f.logger.Printf("fallback: %s unavailable, surfacing the original failure: %v", f.fallback.Name(), avErr)
		if err != nil {
			return fmt.Errorf("%w — local fallback also unavailable: %v", err, avErr)
		}
		return fmt.Errorf("claude: %s — local fallback also unavailable: %v", reason, avErr)
	}

	f.logger.Printf("fallback: %s → %s (%s)", reason, f.fallback.Name(), truncateReason(args.Prompt))
	f.mu.Lock()
	f.last = reason
	f.mu.Unlock()
	f.used.Add(1)

	// The notice leads the reply so it is the first thing read — and, on the
	// voice path, the first thing heard. A degraded answer mistaken for a full
	// one is the failure mode worth spending a sentence to prevent.
	emit(ctx, out, AssistantTextEvent{Text: noticeFor(f.fallback) + "\n\n"})

	if genErr := f.fallback.Generate(ctx, args, out); genErr != nil {
		f.logger.Printf("fallback: %s failed too: %v", f.fallback.Name(), genErr)
		if err != nil {
			return fmt.Errorf("%w — local fallback also failed: %v", err, genErr)
		}
		return fmt.Errorf("claude: %s — local fallback also failed: %v", reason, genErr)
	}
	return nil
}

// noticeFor is the one-line warning prepended to every fallback reply. Phrased
// to be readable on Telegram and speakable by piper, which rules out the
// bracketed-tag style used elsewhere in Otto's output.
func noticeFor(fb Fallback) string {
	model := fb.Name()
	if m, ok := fb.(interface{ Model() string }); ok {
		model = m.Model()
	}
	return "Heads up: Claude Code is unreachable, so this answer is from the local model (" +
		model + ") with no tools and no memory access."
}

// shouldFallback decides whether a failed turn is worth retrying locally, and
// returns the reason for the log.
//
// The bias is toward falling back, because the alternative the user sees is
// "⚠️ Claude error" and nothing else. Two cases are excluded, both because
// falling back would be actively wrong rather than merely unnecessary:
//
//   - Cancellation. /restart, shutdown and the hang watchdog all cancel the
//     context. Every one of them is somebody deciding this turn should stop, so
//     answering it anyway on a second brain ignores the instruction.
//   - A turn that already produced text. Claude may fail partway through a
//     reply that was largely fine; appending a second, worse answer to it would
//     leave the user with two and no way to tell which to trust.
func shouldFallback(ctx context.Context, err error, sawText, badResult bool) (string, bool) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	if sawText {
		return "", false
	}
	if err != nil {
		return classifyFailure(err), true
	}
	if badResult {
		return "claude returned a non-success result", true
	}
	return "", false
}

// classifyFailure names the failure for the log. Purely descriptive — every
// branch falls back — but an outage is diagnosed from these lines later, and
// "claude binary not found" and "api overloaded" send you to very different
// places.
func classifyFailure(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "executable file not found"), strings.Contains(s, "no such file or directory"):
		return "claude binary missing"
	case strings.Contains(s, "401"), strings.Contains(s, "unauthorized"),
		strings.Contains(s, "authentication"), strings.Contains(s, "invalid api key"),
		strings.Contains(s, "please run /login"):
		return "claude auth failed"
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"),
		strings.Contains(s, "overloaded"), strings.Contains(s, "capacity"):
		return "claude rate-limited or overloaded"
	case strings.Contains(s, "503"), strings.Contains(s, "502"), strings.Contains(s, "500"),
		strings.Contains(s, "internal server error"):
		return "claude api error"
	case strings.Contains(s, "dial tcp"), strings.Contains(s, "no such host"),
		strings.Contains(s, "network is unreachable"), strings.Contains(s, "connection refused"),
		strings.Contains(s, "timeout"), strings.Contains(s, "tls"):
		return "network unreachable"
	default:
		return "claude failed"
	}
}

func truncateReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
