//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"otto/internal/store"
	"otto/internal/telegram"
)

// busDrainInterval is how often runBusDrain polls the inbox table. Kept
// short so an Otto→Toto ping feels conversational (sub-second), but long
// enough to coalesce bursts and keep idle CPU at ~zero. Package var so
// tests can crank it down.
var busDrainInterval = 250 * time.Millisecond

// runBusDrain polls the inbox table on busDrainInterval and dispatches
// each row to the addressed agent. Returns when ctx is cancelled.
//
// Hop tracking: each row carries a hop counter (0 for user-originated,
// +1 per agent forward). The dispatcher wraps the per-call ctx via
// store.WithBusHop so MCP tool handlers running inside the recipient
// agent can read it and stop the chain at store.MaxBusHop.
func (h *handler) runBusDrain(ctx context.Context) {
	if h.store == nil {
		return
	}
	t := time.NewTicker(busDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		msgs, err := h.store.DequeueAll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("bus: dequeue error: %v", err)
			continue
		}
		for _, m := range msgs {
			h.dispatchBusMessage(ctx, m)
		}
	}
}

// dispatchBusMessage routes one bus message to the addressed agent. No
// banner is sent — the recipient's own persona reply is the only visible
// artifact, so the chat reads as a real conversation rather than a relay
// log.
//
// For agent-sourced messages, the dispatch context is wrapped via
// store.WithBusHop(m.Hop) so the recipient's tool handlers (forward_to_otto,
// message_<x>) can read the current depth and refuse once the chain hits
// store.MaxBusHop. The bus context (sender, hop) is also injected into the
// recipient's per-call system prompt so the model can wind down naturally.
func (h *handler) dispatchBusMessage(ctx context.Context, m store.InboxMsg) {
	dispatchCtx := ctx
	if m.Source == "agent" {
		dispatchCtx = store.WithBusHop(ctx, m.Hop)
	}

	chatID := h.allow.UserID()
	if chatID == 0 {
		log.Printf("bus: no chat id available, dropping message id=%d", m.ID)
		return
	}

	// Log routing so an operator tailing the journal can see the chain
	// without surfacing it in the user-facing chat.
	log.Printf("bus dispatch: id=%d %s→%s hop=%d source=%s",
		m.ID, fromLabel(m), m.Target, m.Hop, m.Source)

	switch m.Target {
	case "otto":
		h.dispatchBusToOtto(dispatchCtx, chatID, m)
	case "toto":
		if h.toto == nil {
			log.Printf("bus: no toto wired, dropping id=%d", m.ID)
			return
		}
		h.toto.BusReply(dispatchCtx, chatID, m.Body, busContextFromMsg(m))
	case "toot":
		toot := h.findToot()
		if toot == nil {
			log.Printf("bus: no toot wired, dropping id=%d", m.ID)
			return
		}
		toot.BusReply(dispatchCtx, chatID, m.Body, busContextFromMsg(m))
	default:
		log.Printf("bus: unknown target %q (id=%d)", m.Target, m.ID)
	}
}

// dispatchBusToOtto runs Otto on a bus-sourced message. User-sourced rows
// go straight through handleMessage (no BUS CONTEXT — that's just a
// normal Telegram message that took a detour). Agent-sourced rows get a
// BUS CONTEXT block prepended so Otto knows to reply via message_<sender>
// and how many hops remain before the cap.
func (h *handler) dispatchBusToOtto(ctx context.Context, chatID int64, m store.InboxMsg) {
	u := telegram.Update{
		UserID: chatID,
		ChatID: chatID,
		Text:   m.Body,
	}
	acquired, snap := h.otto.tryAcquireOrSnapshot(u.Text)
	if !acquired {
		log.Printf("bus: otto busy on forwarded msg id=%d (silence=%s)", m.ID, snap.Silence.Round(time.Second))
		if h.toto != nil {
			h.toto.BusyReply(ctx, chatID, u.Text, snap.CurrentPrompt, snap.Snippet)
		}
		return
	}
	defer h.otto.release()
	if m.Source == "user" {
		// A user-sourced row reaches Otto through the bus the same way a
		// direct Telegram message would — no BUS CONTEXT or HOPS REMAINING
		// goes on this prompt; that would mislead Otto into thinking he's
		// in a chain.
		h.handleMessage(ctx, u)
		return
	}
	h.handleBusOttoMessage(ctx, u, busContextFromMsg(m))
}

// handleBusOttoMessage is the agent-sourced Otto path: it wraps the
// runner with env vars carrying the hop counter + sender so the MCP
// tools running inside this Claude invocation can stamp follow-ups
// correctly, and it composes a per-call system prompt that includes
// BUS CONTEXT + HOPS REMAINING so Otto knows to call message_<sender>
// to keep the loop alive (or to wind down when hops remaining hits 0).
func (h *handler) handleBusOttoMessage(ctx context.Context, u telegram.Update, bc busContext) {
	prevRunner := h.runner
	// "otto" is the receiver; for outbound enqueues by Otto's tools the
	// sender is Otto.
	scoped := prevRunner.WithEnv(busEnv(bc.Hop, "otto"))
	h.runner = scoped
	defer func() { h.runner = prevRunner }()

	prevPrompt := h.baseSystemPrompt
	h.baseSystemPrompt = prevPrompt + "\n\n" + busPromptBlock(bc, "otto")
	defer func() { h.baseSystemPrompt = prevPrompt }()

	h.handleMessage(ctx, u)
}

// busContext is the immutable view of a bus row that the per-call prompt
// composers and env wirers consume.
type busContext struct {
	Sender string // "otto" | "toto" | "toot" | "user"
	Hop    int    // current row's hop depth (0..MaxBusHop)
}

// busContextFromMsg lifts the relevant subset of InboxMsg into the
// composer's struct.
func busContextFromMsg(m store.InboxMsg) busContext {
	from := m.Sender
	if from == "" {
		from = m.Source
	}
	return busContext{Sender: from, Hop: m.Hop}
}

// busEnv builds the env-var map handed to the recipient's Claude
// subprocess. The MCP server picks these up at startup and uses them as
// the effective hop counter / sender label when its tools enqueue
// follow-ups (the in-process WithBusHop ctx is unreachable cross-process).
func busEnv(hop int, sender string) map[string]string {
	return map[string]string{
		"OTTO_BUS_HOP":    fmt.Sprintf("%d", hop),
		"OTTO_BUS_SENDER": sender,
	}
}

// busPromptBlock builds the BUS CONTEXT + HOPS REMAINING section the
// dispatcher prepends to the recipient's per-call system prompt. selfName
// is the recipient's own name; it appears in the tool hint so the model
// reads "call message_<sender>" naturally rather than having to compute
// who isn't itself.
func busPromptBlock(bc busContext, selfName string) string {
	remaining := store.MaxBusHop - bc.Hop
	if remaining < 0 {
		remaining = 0
	}
	var winddown string
	if remaining == 0 {
		winddown = "HOPS REMAINING: 0. Wrap things up. Reply to the user in plain text only — do NOT call message_" + bc.Sender + " or any other agent-message tool. The chain ends with this turn."
	} else {
		winddown = fmt.Sprintf("HOPS REMAINING: %d (of %d). If you want to continue the conversation with %s, call message_%s(message, reason) — that keeps the loop alive. If you're wrapping up, reply in plain text only and the chain ends here. Don't ask a follow-up question on the last hop; better to land cleanly.", remaining, store.MaxBusHop, bc.Sender, bc.Sender)
	}
	return "───────────────────────────────────────────────\n" +
		"BUS CONTEXT (this message came via the inbox)\n" +
		"───────────────────────────────────────────────\n\n" +
		fmt.Sprintf("From: %s. Hop %d of %d. You are: %s.\n\n", bc.Sender, bc.Hop, store.MaxBusHop, selfName) +
		winddown
}

// fromLabel produces the "<sender>" string used in dispatch logs.
func fromLabel(m store.InboxMsg) string {
	if m.Sender != "" {
		return m.Sender
	}
	return m.Source
}

// findToot pulls the Toot pet out of the registry. Toot isn't held as
// its own field on handler (only Toto is, because the dispatch fast-path
// uses BusyReply); for the bus we look it up by name.
func (h *handler) findToot() *Toot {
	if h.pets == nil {
		return nil
	}
	for _, p := range h.pets.pets {
		if t, ok := p.(*Toot); ok {
			return t
		}
	}
	return nil
}
