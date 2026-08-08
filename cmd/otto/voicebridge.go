//go:build unix

package main

import (
	"context"
	"html"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"otto/internal/voice"
)

// The bridge between the voice client and Otto's normal turn machinery.
//
// It implements voice.Responder: given a transcript, submit it through the mux
// exactly as a typed message, and return a channel of sentences to speak.
//
// Two things make this more than a function call.
//
// First, streaming. Otto's parser already delivers assistant text
// incrementally, so sentences can be spoken while generation is still running —
// which removes the largest chunk of the delay between finishing a sentence and
// hearing a reply. That needs a tap on the stream, not just the final text.
//
// Second, Otto might not be the one answering. If he is mid-task the message
// falls to Toto, whose reply never touches the streaming path — it arrives
// whole, through the ordinary send. A bridge that only understood streaming
// would wait forever for a turn that was already answered, and the voice loop
// would wedge. So the bridge accepts completion from either direction.

// voiceTurnTimeout bounds an open turn. Nothing should reach it — the watchdog
// kills a wedged Otto at ten minutes and that produces a reply — but without it
// a turn that somehow ends silently would leave the listener stuck in
// "thinking…" forever, unable to hear the user ask what happened.
const voiceTurnTimeout = 11 * time.Minute

// petLabel matches the "<blockquote><b>TOTO</b></blockquote>" header the pets
// prefix onto their replies, so the bridge can speak in the right voice.
var petLabel = regexp.MustCompile(`(?i)<b>\s*(TOTO|TOOT)\s*</b>`)

var htmlTagRE = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// voiceBridge adapts the mux to voice.Responder.
type voiceBridge struct {
	mux    *muxBot
	userID int64

	mu       sync.Mutex
	out      chan voice.Utterance
	splitter voice.SentenceSplitter
	streamed bool
	turn     int // increments per turn, so a late timeout cannot close a newer one
}

func newVoiceBridge(mux *muxBot, userID int64) *voiceBridge {
	return &voiceBridge{mux: mux, userID: userID}
}

// Respond submits the transcript and returns the stream of sentences to speak.
func (b *voiceBridge) Respond(ctx context.Context, text string) (<-chan voice.Utterance, error) {
	ch := make(chan voice.Utterance, 32)

	b.mu.Lock()
	// A previous turn should always have closed by now; if one is somehow
	// still open, close it rather than orphaning its consumer.
	if b.out != nil {
		close(b.out)
	}
	b.out = ch
	b.splitter = voice.SentenceSplitter{}
	b.streamed = false
	b.turn++
	turn := b.turn
	b.mu.Unlock()

	if !b.mux.Submit(b.userID, text) {
		b.finish(turn)
		return nil, errQueueFull
	}

	// Safety net, not the normal path.
	go func() {
		select {
		case <-time.After(voiceTurnTimeout):
			if b.finish(turn) {
				log.Printf("voice: turn timed out after %s with no reply", voiceTurnTimeout)
			}
		case <-ctx.Done():
			b.finish(turn)
		}
	}()

	return ch, nil
}

// Chunk receives streamed assistant text from Otto's turn. Complete sentences
// are emitted immediately so playback can start mid-generation.
func (b *voiceBridge) Chunk(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.out == nil {
		return
	}
	b.streamed = true
	for _, sentence := range b.splitter.Push(text) {
		b.emitLocked(voice.Utterance{Persona: voice.PersonaOtto, Text: sentence})
	}
}

// Finish ends a streamed turn, speaking whatever partial sentence remains.
//
// The tail matters: Otto often signs off without terminal punctuation, and
// without a flush that last clause would simply never be said.
func (b *voiceBridge) Finish() {
	b.mu.Lock()
	if b.out == nil {
		b.mu.Unlock()
		return
	}
	if rest := b.splitter.Flush(); rest != "" {
		b.emitLocked(voice.Utterance{Persona: voice.PersonaOtto, Text: rest})
	}
	turn := b.turn
	b.mu.Unlock()
	b.finish(turn)
}

// Delivered is called for every reply routed to the local surface.
//
// For a streamed Otto turn this is the same text already spoken, so it is
// ignored. For anything that did not stream — a pet covering for a busy Otto, a
// command reply, an error — it is the only copy there is, so it is spoken here.
func (b *voiceBridge) Delivered(text string, isHTML bool) {
	b.mu.Lock()
	if b.out == nil || b.streamed {
		// No open turn, or Otto already streamed this content.
		b.mu.Unlock()
		return
	}
	persona, body := classifyDelivered(text, isHTML)
	for _, sentence := range voice.SplitSentences(body) {
		b.emitLocked(voice.Utterance{Persona: persona, Text: sentence})
	}
	turn := b.turn
	b.mu.Unlock()
	b.finish(turn)
}

// emitLocked queues an utterance, dropping it if the consumer has stalled.
// Caller must hold mu.
func (b *voiceBridge) emitLocked(u voice.Utterance) {
	if strings.TrimSpace(u.Text) == "" {
		return
	}
	select {
	case b.out <- u:
	default:
		log.Printf("voice: speech queue full, dropped %q", truncate(u.Text, 40))
	}
}

// finish closes the turn if it is still the one identified by turn. Returns
// whether it actually closed anything, so a timeout can report honestly.
func (b *voiceBridge) finish(turn int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.out == nil || b.turn != turn {
		return false
	}
	close(b.out)
	b.out = nil
	return true
}

// classifyDelivered strips markup and works out who is speaking, so a pet's
// reply is heard in the pet's own voice.
func classifyDelivered(text string, isHTML bool) (persona, body string) {
	persona = voice.PersonaOtto
	if m := petLabel.FindStringSubmatch(text); m != nil {
		persona = strings.ToLower(m[1])
	} else if strings.HasPrefix(text, "TOTO\n") {
		// Plain-text fallback the pets use when an HTML send fails.
		persona = voice.PersonaToto
	} else if strings.HasPrefix(text, "TOOT\n") {
		persona = voice.PersonaToot
	}

	body = text
	if isHTML {
		body = html.UnescapeString(htmlTagRE.ReplaceAllString(body, "\n"))
	}
	// Drop the leading persona label — it is a visual header, and hearing
	// "toto" announced before every one of Toto's lines is not how a
	// conversation sounds.
	body = strings.TrimSpace(body)
	for _, label := range []string{"TOTO", "TOOT", "OTTO"} {
		if strings.HasPrefix(body, label) {
			body = strings.TrimSpace(strings.TrimPrefix(body, label))
			break
		}
	}
	// The pets prefix ASCII art; it is beautiful and completely unspeakable.
	body = stripArtBlock(body)
	return persona, voice.SanitizeForTTS(body)
}

// stripArtBlock removes leading lines that are mostly non-alphanumeric — the
// pets' ASCII art. Stops at the first line that reads like prose.
func stripArtBlock(s string) string {
	lines := strings.Split(s, "\n")
	i := 0
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if looksLikeProse(line) {
			break
		}
	}
	if i >= len(lines) {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[i:], "\n"))
}

// looksLikeProse reports whether a line is mostly letters and spaces. ASCII
// cats are mostly slashes, underscores and parentheses.
func looksLikeProse(line string) bool {
	var letters, total int
	for _, r := range line {
		if r == ' ' {
			continue
		}
		total++
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	if total < 3 {
		return false
	}
	return float64(letters)/float64(total) > 0.6
}
