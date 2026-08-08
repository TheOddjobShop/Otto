//go:build unix

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"otto/internal/telegram"
)

// ─── Stubs ───────────────────────────────────────────────────────────────

// muxFakeTG is a Telegram client whose GetUpdates blocks until released,
// simulating an open long-poll.
type muxFakeTG struct {
	mu       sync.Mutex
	sent     []sentMsg
	sentHTML []sentMsg
	updates  chan []telegram.Update
	err      error
}

func newMuxFakeTG() *muxFakeTG {
	return &muxFakeTG{updates: make(chan []telegram.Update, 4)}
}

func (f *muxFakeTG) GetUpdates(ctx context.Context, offset int) ([]telegram.Update, error) {
	if f.err != nil {
		return nil, f.err
	}
	select {
	case u := <-f.updates:
		return u, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *muxFakeTG) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatID: chatID, text: text})
	return nil
}

func (f *muxFakeTG) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentHTML = append(f.sentHTML, sentMsg{chatID: chatID, text: text})
	return nil
}

func (f *muxFakeTG) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return []byte("file"), "application/octet-stream", nil
}

func (f *muxFakeTG) telegramSent() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

// capturingSurface records what the front end was asked to render.
type capturingSurface struct {
	mu       sync.Mutex
	messages []string
	html     []bool
}

func (s *capturingSurface) Deliver(ctx context.Context, text string, html bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, text)
	s.html = append(s.html, html)
}

func (s *capturingSurface) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

// ─── Routing ─────────────────────────────────────────────────────────────

// The core contract: a reply goes back to whichever surface the message came
// from, decided by chat id alone.
func TestMuxRoutesRepliesBySurface(t *testing.T) {
	tg := newMuxFakeTG()
	m := newMuxBot(tg)
	surface := &capturingSurface{}
	m.AttachSurface(surface)

	if err := m.SendMessage(context.Background(), 12345, "to telegram"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if err := m.SendMessage(context.Background(), tuiChatID, "to the front end"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	tgSent := tg.telegramSent()
	if len(tgSent) != 1 || tgSent[0].text != "to telegram" {
		t.Errorf("telegram received %v, want only the telegram-addressed message", tgSent)
	}
	local := surface.delivered()
	if len(local) != 1 || local[0] != "to the front end" {
		t.Errorf("surface received %v, want only the local message", local)
	}
}

func TestMuxRoutesHTMLBySurfaceAndFlagsIt(t *testing.T) {
	tg := newMuxFakeTG()
	m := newMuxBot(tg)
	surface := &capturingSurface{}
	m.AttachSurface(surface)

	if err := m.SendMessageHTML(context.Background(), tuiChatID, "<pre>art</pre>"); err != nil {
		t.Fatalf("SendMessageHTML: %v", err)
	}
	surface.mu.Lock()
	defer surface.mu.Unlock()
	if len(surface.html) != 1 || !surface.html[0] {
		t.Error("the surface must be told the body is HTML so it can render or strip the pets' art")
	}
}

// The reserved chat id must not collide with anything Telegram can produce:
// private chats are positive, groups are large negatives.
func TestTUIChatIDIsOutsideTelegramRange(t *testing.T) {
	if tuiChatID >= 0 {
		t.Error("tuiChatID must be negative so it cannot collide with a private chat id")
	}
	if tuiChatID < -1000 {
		t.Error("tuiChatID should sit above the group-id range (-100…)")
	}
	for _, real := range []int64{1, 99999999, -1001234567890} {
		if isTUIChat(real) {
			t.Errorf("isTUIChat(%d) = true; real Telegram ids must not route locally", real)
		}
	}
}

// A reply with nowhere to go is dropped rather than erroring — the front end
// having closed is not a send failure to report to an absent user.
func TestMuxDropsLocalReplyWithNoSurface(t *testing.T) {
	m := newMuxBot(newMuxFakeTG())
	if err := m.SendMessage(context.Background(), tuiChatID, "into the void"); err != nil {
		t.Errorf("SendMessage with no surface returned %v, want nil", err)
	}
}

// ─── Update fan-in ───────────────────────────────────────────────────────

// The latency requirement: a spoken message must not wait out a quiet Telegram
// long-poll, which can be several seconds.
func TestMuxLocalUpdateDoesNotWaitForTelegram(t *testing.T) {
	tg := newMuxFakeTG() // GetUpdates blocks until something is pushed
	m := newMuxBot(tg)

	if !m.Submit(42, "otto what's up") {
		t.Fatal("Submit was refused")
	}

	done := make(chan []telegram.Update, 1)
	go func() {
		u, err := m.GetUpdates(context.Background(), 0)
		if err != nil {
			t.Errorf("GetUpdates: %v", err)
		}
		done <- u
	}()

	select {
	case got := <-done:
		if len(got) != 1 || got[0].Text != "otto what's up" {
			t.Fatalf("got %v, want the local message", got)
		}
		if got[0].ChatID != tuiChatID {
			t.Errorf("ChatID = %d, want the reserved local id", got[0].ChatID)
		}
		if got[0].UserID != 42 {
			t.Errorf("UserID = %d, want the allowlisted id to pass the auth gate", got[0].UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a local message waited on the Telegram long-poll")
	}
}

// A message submitted while the long-poll is already open must interrupt it.
func TestMuxLocalUpdateInterruptsOpenLongPoll(t *testing.T) {
	m := newMuxBot(newMuxFakeTG())

	done := make(chan []telegram.Update, 1)
	go func() {
		u, _ := m.GetUpdates(context.Background(), 0)
		done <- u
	}()

	// Let the long-poll get going, then speak.
	time.Sleep(50 * time.Millisecond)
	m.Submit(42, "hello")

	select {
	case got := <-done:
		if len(got) != 1 || got[0].Text != "hello" {
			t.Fatalf("got %v, want the local message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the open long-poll was not interrupted by a local message")
	}
}

func TestMuxPassesThroughTelegramUpdates(t *testing.T) {
	tg := newMuxFakeTG()
	m := newMuxBot(tg)
	tg.updates <- []telegram.Update{{UpdateID: 5, ChatID: 99, UserID: 42, Text: "typed"}}

	got, err := m.GetUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(got) != 1 || got[0].Text != "typed" {
		t.Fatalf("got %v, want the telegram update", got)
	}
}

// A Telegram outage must not take voice down with it.
func TestMuxPrefersLocalWorkOverTelegramError(t *testing.T) {
	tg := newMuxFakeTG()
	tg.err = errors.New("network unreachable")
	m := newMuxBot(tg)
	m.Submit(42, "still works")

	got, err := m.GetUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("a queued local message should be delivered despite the Telegram error, got %v", err)
	}
	if len(got) != 1 || got[0].Text != "still works" {
		t.Fatalf("got %v, want the local message", got)
	}
}

func TestMuxSurfacesTelegramErrorWhenNothingLocal(t *testing.T) {
	tg := newMuxFakeTG()
	tg.err = errors.New("network unreachable")
	m := newMuxBot(tg)

	if _, err := m.GetUpdates(context.Background(), 0); err == nil {
		t.Fatal("with nothing local queued the Telegram error must surface so backoff applies")
	}
}

// Synthetic ids count downward so they can never collide with a real Telegram
// update id or corrupt the polling loop's offset arithmetic.
func TestMuxLocalUpdateIDsAreNegativeAndDescending(t *testing.T) {
	m := newMuxBot(newMuxFakeTG())
	for i := 0; i < 3; i++ {
		m.Submit(42, "msg")
	}
	got, err := m.GetUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d updates, want 3 drained together", len(got))
	}
	for i, u := range got {
		if u.UpdateID >= 0 {
			t.Errorf("update %d has id %d; local ids must stay negative", i, u.UpdateID)
		}
		if i > 0 && u.UpdateID >= got[i-1].UpdateID {
			t.Errorf("ids are not descending: %d then %d", got[i-1].UpdateID, u.UpdateID)
		}
	}
}

// Submit must never block the UI event loop.
func TestMuxSubmitRefusesWhenFull(t *testing.T) {
	m := newMuxBot(newMuxFakeTG())
	accepted := 0
	for i := 0; i < 100; i++ {
		if m.Submit(42, "flood") {
			accepted++
		}
	}
	if accepted == 0 {
		t.Fatal("no messages were accepted at all")
	}
	if accepted == 100 {
		t.Fatal("Submit accepted an unbounded flood; it must refuse rather than grow without bound")
	}
}

func TestMuxDownloadAlwaysGoesToTelegram(t *testing.T) {
	m := newMuxBot(newMuxFakeTG())
	body, _, err := m.DownloadFile(context.Background(), "f1")
	if err != nil || string(body) != "file" {
		t.Errorf("DownloadFile = (%q, %v), want the Telegram passthrough", body, err)
	}
}

// ─── Voice mode ──────────────────────────────────────────────────────────

func TestComposeVoicePrompt(t *testing.T) {
	got := composeVoicePrompt("PERSONA")
	if !strings.HasPrefix(got, "PERSONA") {
		t.Error("the base prompt must come first")
	}
	if !strings.Contains(got, "SPOKEN ALOUD") {
		t.Error("the voice clause is missing")
	}
	// The clause must land last, because the operational footer earlier in the
	// prompt actively teaches the formatting this has to override.
	if strings.Index(got, "SPOKEN ALOUD") < strings.Index(got, "PERSONA") {
		t.Error("the voice clause must come after the persona and footer")
	}
}

// The subtle failure mode: a model told to be brief starts doing less work
// instead of saying less about it.
func TestVoicePromptPreservesScope(t *testing.T) {
	p := composeVoicePrompt("")
	for _, want := range []string{"STILL DO THE WORK", "DELIVERY, not scope"} {
		if !strings.Contains(p, want) {
			t.Errorf("voice prompt missing %q — brevity must not be read as doing less", want)
		}
	}
}

func TestVoicePromptBansUnspeakableFormatting(t *testing.T) {
	p := strings.ToLower(composeVoicePrompt(""))
	for _, want := range []string{"no lists", "markdown", "file paths"} {
		if !strings.Contains(p, want) {
			t.Errorf("voice prompt should rule out %q", want)
		}
	}
}
