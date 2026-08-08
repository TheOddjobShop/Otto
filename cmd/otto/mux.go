//go:build unix

package main

import (
	"context"
	"log"
	"sync"

	"otto/internal/telegram"
)

// The surface multiplexer.
//
// telegram.BotClient was already the surface abstraction — four methods, and
// cmd/otto/tty.go already implements it for -tty mode. Every reply in the tree
// goes through telegram.SendChunked(ctx, <bot>, chatID, text). So a front end
// needs no new interface; it needs a multiplexer that fans two input sources
// into one update stream and routes each reply back to whichever surface the
// message came from.
//
// The payoff is that handler.go, toto.go, toot.go, commands.go and bus.go need
// no changes at all. Memory, session_search, recent_turns, the model router,
// the agent bus, the watchdog, the activity log, session rotation and /tokens
// all work over voice from the first day, because none of them can tell the
// difference. /status spoken aloud is free.

// tuiChatID is the reserved chat id for locally-originated messages.
//
// Telegram user and group ids are non-zero and never take this value: private
// chat ids are positive and equal to the user id, and group ids are large
// negatives (-100…). A small negative constant sits safely outside both ranges,
// so a reply can be routed by chat id alone with no ambiguity and no extra
// field threaded through every call site.
const tuiChatID int64 = -1

// isTUIChat reports whether a chat id belongs to the local front end.
func isTUIChat(id int64) bool { return id == tuiChatID }

// localSurface receives replies addressed to the front end. The TUI implements
// it; a nil surface makes the mux behave exactly like the bare Telegram client.
type localSurface interface {
	// Deliver is called with each reply destined for the front end. html is
	// true when the body carries HTML markup (the pets send their ASCII art
	// that way), so the surface can strip or render it as it sees fit.
	Deliver(ctx context.Context, text string, html bool)
}

// muxBot fans Telegram and the local front end into one BotClient.
type muxBot struct {
	// tg is the real Telegram client. Never nil in production.
	tg telegram.BotClient

	// surface receives replies for tuiChatID. Nil until the front end
	// attaches, which is why it is guarded.
	mu      sync.RWMutex
	surface localSurface

	// local carries updates injected by the front end.
	local chan telegram.Update

	// nextUpdateID assigns synthetic update ids to local messages. The polling
	// loop tracks the max id it has seen to compute the Telegram offset, so
	// local ids must never collide with or exceed real ones — they count
	// downward from zero into negative space, which Telegram never uses.
	nextUpdateID int
	idMu         sync.Mutex
}

// newMuxBot wraps a Telegram client. Buffer is small: the front end is one
// human typing or speaking, and a deep queue would only let stale utterances
// pile up behind a slow turn.
func newMuxBot(tg telegram.BotClient) *muxBot {
	return &muxBot{
		tg:    tg,
		local: make(chan telegram.Update, 8),
	}
}

// AttachSurface registers the front end. Safe to call once the mux is running.
func (m *muxBot) AttachSurface(s localSurface) {
	m.mu.Lock()
	m.surface = s
	m.mu.Unlock()
}

// Submit injects a message from the front end as if it had arrived from
// Telegram. userID must be the allowlisted id or dispatch will drop it.
//
// Returns false when the queue is full rather than blocking: the caller is the
// UI event loop, and stalling it would freeze the screen.
func (m *muxBot) Submit(userID int64, text string) bool {
	m.idMu.Lock()
	m.nextUpdateID--
	id := m.nextUpdateID
	m.idMu.Unlock()

	select {
	case m.local <- telegram.Update{
		UpdateID: id,
		ChatID:   tuiChatID,
		UserID:   userID,
		Text:     text,
	}:
		return true
	default:
		log.Printf("mux: local queue full, dropped %q", truncate(text, 40))
		return false
	}
}

// GetUpdates returns whichever source has something first.
//
// Local messages must not wait behind a quiet Telegram long-poll (up to its 5s
// timeout), so the Telegram call runs in its own goroutine feeding a channel
// and this selects across both. A spoken message dispatches immediately even
// when Telegram has been silent for hours.
//
// A Telegram outage must also not take voice down, so poll errors are returned
// only when nothing local is pending — the existing backoff in the polling loop
// then applies, while local updates keep flowing.
func (m *muxBot) GetUpdates(ctx context.Context, offset int) ([]telegram.Update, error) {
	// Drain anything already queued without touching the network at all.
	if batch := m.drainLocal(); len(batch) > 0 {
		return batch, nil
	}

	type tgResult struct {
		updates []telegram.Update
		err     error
	}
	done := make(chan tgResult, 1)
	go func() {
		u, err := m.tg.GetUpdates(ctx, offset)
		done <- tgResult{updates: u, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case u := <-m.local:
		// A local message arrived while the long-poll was open. Return it
		// immediately; the in-flight Telegram call is left to complete on its
		// own and its result is discarded, which is safe because the offset is
		// unchanged so those updates are re-delivered on the next call.
		batch := append([]telegram.Update{u}, m.drainLocal()...)
		return batch, nil

	case r := <-done:
		if r.err != nil {
			// Prefer delivering local work over surfacing a network error.
			if batch := m.drainLocal(); len(batch) > 0 {
				return batch, nil
			}
			return nil, r.err
		}
		return append(r.updates, m.drainLocal()...), nil
	}
}

// drainLocal removes every queued local update without blocking.
func (m *muxBot) drainLocal() []telegram.Update {
	var out []telegram.Update
	for {
		select {
		case u := <-m.local:
			out = append(out, u)
		default:
			return out
		}
	}
}

// SendMessage routes by chat id: the front end for local turns, Telegram for
// everything else.
func (m *muxBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	if isTUIChat(chatID) {
		m.deliverLocal(ctx, text, false)
		return nil
	}
	return m.tg.SendMessage(ctx, chatID, text)
}

// SendMessageHTML mirrors SendMessage. The front end is told the body is HTML
// so it can decide what to do with the pets' <pre> ASCII art rather than
// printing tags.
func (m *muxBot) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	if isTUIChat(chatID) {
		m.deliverLocal(ctx, text, true)
		return nil
	}
	return m.tg.SendMessageHTML(ctx, chatID, text)
}

// DownloadFile always goes to Telegram — the front end never produces file ids.
func (m *muxBot) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return m.tg.DownloadFile(ctx, fileID)
}

// deliverLocal hands a reply to the attached surface, dropping it with a log
// line if none is attached.
//
// Dropping is correct rather than an error: an unattached surface means the
// front end has closed, and there is nowhere for the text to go. Returning an
// error would surface a "send failed" to a user who is no longer looking.
func (m *muxBot) deliverLocal(ctx context.Context, text string, html bool) {
	m.mu.RLock()
	s := m.surface
	m.mu.RUnlock()
	if s == nil {
		log.Printf("mux: no surface attached, dropping local reply %q", truncate(text, 40))
		return
	}
	s.Deliver(ctx, text, html)
}
