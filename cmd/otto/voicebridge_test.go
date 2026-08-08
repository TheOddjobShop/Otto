//go:build unix

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"otto/internal/voice"
)

func newBridge(t *testing.T) (*voiceBridge, *muxBot) {
	t.Helper()
	m := newMuxBot(newMuxFakeTG())
	return newVoiceBridge(m, 42), m
}

// collect drains a stream with a bound, so a bug that leaves it open fails the
// test instead of hanging the suite.
func collect(t *testing.T, ch <-chan voice.Utterance) []voice.Utterance {
	t.Helper()
	var out []voice.Utterance
	deadline := time.After(3 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, u)
		case <-deadline:
			t.Fatal("stream never closed — the voice loop would wedge here")
			return out
		}
	}
}

// The streaming contract: sentences are emitted as they complete, not held
// until generation finishes.
func TestBridgeStreamsSentencesAsTheyForm(t *testing.T) {
	b, mux := newBridge(t)
	ch, err := b.Respond(context.Background(), "otto status")
	if err != nil {
		t.Fatal(err)
	}
	// The transcript reached the mux as an ordinary update.
	got, _ := mux.GetUpdates(context.Background(), 0)
	if len(got) != 1 || got[0].Text != "otto status" {
		t.Fatalf("mux received %v, want the transcript submitted as a normal message", got)
	}

	b.Chunk("Your build is green. ")
	select {
	case u := <-ch:
		if u.Text != "Your build is green." {
			t.Errorf("first utterance = %q, want the completed sentence", u.Text)
		}
		if u.Persona != voice.PersonaOtto {
			t.Errorf("persona = %q, want otto", u.Persona)
		}
	case <-time.After(time.Second):
		t.Fatal("a completed sentence was not emitted until the turn ended")
	}

	b.Chunk("Nothing else is pending right now.")
	b.Finish()

	rest := collect(t, ch)
	if len(rest) != 1 || rest[0].Text != "Nothing else is pending right now." {
		t.Errorf("remaining utterances %v, want the flushed tail", rest)
	}
}

// Otto often signs off without terminal punctuation; without a flush that last
// clause would simply never be spoken.
func TestBridgeFlushesUnterminatedTail(t *testing.T) {
	b, _ := newBridge(t)
	ch, err := b.Respond(context.Background(), "otto hi")
	if err != nil {
		t.Fatal(err)
	}
	b.Chunk("here you go")
	b.Finish()

	got := collect(t, ch)
	if len(got) != 1 || got[0].Text != "here you go" {
		t.Errorf("got %v, want the unterminated tail spoken", got)
	}
}

// The case a streaming-only bridge would deadlock on: Otto is busy, so Toto
// answers, and that reply never touches the streaming path.
func TestBridgeSpeaksNonStreamedPetReply(t *testing.T) {
	b, _ := newBridge(t)
	ch, err := b.Respond(context.Background(), "otto you there")
	if err != nil {
		t.Fatal(err)
	}

	b.Delivered("<blockquote><b>TOTO</b></blockquote>\n<pre>  /\\_/\\\n ( o.o )</pre>\n\notto's busy. you got me. mrow.", true)

	got := collect(t, ch)
	if len(got) == 0 {
		t.Fatal("a pet reply must still be spoken — otherwise the turn hangs forever")
	}
	if got[0].Persona != voice.PersonaToto {
		t.Errorf("persona = %q, want toto so the handoff is audible", got[0].Persona)
	}
	joined := strings.Join(texts(got), " ")
	if !strings.Contains(joined, "otto's busy") {
		t.Errorf("spoke %q, want the cat's actual words", joined)
	}
	if strings.Contains(joined, "<") || strings.Contains(joined, "/\\") {
		t.Errorf("spoke %q; markup and ASCII art must not be read aloud", joined)
	}
	if strings.HasPrefix(strings.ToLower(joined), "toto") {
		t.Errorf("spoke %q; the visual name label should not be announced", joined)
	}
}

// A streamed turn also arrives whole through Deliver. Speaking both would say
// everything twice.
func TestBridgeDoesNotDoubleSpeakStreamedReply(t *testing.T) {
	b, _ := newBridge(t)
	ch, err := b.Respond(context.Background(), "otto status")
	if err != nil {
		t.Fatal(err)
	}
	b.Chunk("Everything is green right now.")
	b.Finish()
	// The mux delivers the same text for display after the stream ends.
	b.Delivered("Everything is green right now.", false)

	got := collect(t, ch)
	if len(got) != 1 {
		t.Errorf("got %d utterances %v, want exactly one — the streamed copy", len(got), texts(got))
	}
}

func TestBridgeSpeaksPlainCommandReply(t *testing.T) {
	b, _ := newBridge(t)
	ch, err := b.Respond(context.Background(), "otto status")
	if err != nil {
		t.Fatal(err)
	}
	b.Delivered("uptime=3m\nstate=idle", false)

	got := collect(t, ch)
	if len(got) == 0 {
		t.Fatal("a command reply should still be spoken")
	}
	if got[0].Persona != voice.PersonaOtto {
		t.Errorf("persona = %q, want otto", got[0].Persona)
	}
}

// A failed turn must still release the listener, or it sits in "thinking…"
// unable to hear the user ask what went wrong.
func TestBridgeFinishClosesEvenWithNoOutput(t *testing.T) {
	b, _ := newBridge(t)
	ch, err := b.Respond(context.Background(), "otto do it")
	if err != nil {
		t.Fatal(err)
	}
	b.Finish()
	if got := collect(t, ch); len(got) != 0 {
		t.Errorf("got %v, want nothing spoken but the stream closed", got)
	}
}

func TestBridgeContextCancelClosesTurn(t *testing.T) {
	b, _ := newBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Respond(ctx, "otto do it")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	collect(t, ch) // fails the test if it never closes
}

// A new turn must not be closed by the previous turn's late timeout.
func TestBridgeTurnsAreIndependent(t *testing.T) {
	b, _ := newBridge(t)

	first, err := b.Respond(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	b.Finish()
	collect(t, first)

	second, err := b.Respond(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	b.Chunk("This is the second turn speaking.")
	b.Finish()
	if got := collect(t, second); len(got) != 1 {
		t.Errorf("second turn produced %v, want its own utterance", texts(got))
	}
}

// Chunks arriving with no open turn must be dropped, not panic.
func TestBridgeIgnoresChunksOutsideATurn(t *testing.T) {
	b, _ := newBridge(t)
	b.Chunk("stray text")
	b.Finish()
	b.Delivered("stray reply", false)
}

func TestClassifyDeliveredPersonas(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		isHTML   bool
		wantWho  string
		wantBody string
	}{
		{"plain otto", "All done.", false, voice.PersonaOtto, "All done."},
		{"html toto", "<blockquote><b>TOTO</b></blockquote>\n\nmrrp.", true, voice.PersonaToto, "mrrp."},
		{"html toot", "<blockquote><b>TOOT</b></blockquote>\n\nNoted, sir.", true, voice.PersonaToot, "Noted, sir."},
		{"plain fallback toto", "TOTO\n\nmrow.", false, voice.PersonaToto, "mrow."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			who, body := classifyDelivered(tc.text, tc.isHTML)
			if who != tc.wantWho {
				t.Errorf("persona = %q, want %q", who, tc.wantWho)
			}
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBody)
			}
		})
	}
}

func TestStripArtBlock(t *testing.T) {
	in := "  /\\_/\\\n ( o.o )\n  > ^ <\n\notto's busy right now."
	got := stripArtBlock(in)
	if got != "otto's busy right now." {
		t.Errorf("stripArtBlock = %q, want the prose only", got)
	}
	// Prose with no art must survive untouched.
	if got := stripArtBlock("just words here"); got != "just words here" {
		t.Errorf("stripArtBlock mangled plain prose: %q", got)
	}
}

func texts(us []voice.Utterance) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Text
	}
	return out
}
