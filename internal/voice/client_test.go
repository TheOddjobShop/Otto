package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// The conversation state machine, exercised end-to-end with stubs standing in
// for the microphone, whisper, piper and the speakers.
//
// This is the part of the package that most needs testing and least tolerates
// being tested by hand: the barge-in rules, the muted overlay and the
// armed-follow-up path are all decisions about *when not to act*, and every one
// of them was a real bug in the system this was ported from.

// ─── Stubs ───────────────────────────────────────────────────────────────

// scriptedSTT returns queued transcripts in order.
type scriptedSTT struct {
	mu   sync.Mutex
	next []string
	err  error
}

func (s *scriptedSTT) push(texts ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = append(s.next, texts...)
}

func (s *scriptedSTT) Transcribe(ctx context.Context, wav []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	if len(s.next) == 0 {
		return "", nil
	}
	out := s.next[0]
	s.next = s.next[1:]
	return out, nil
}

type stubSpeaker struct {
	mu     sync.Mutex
	spoken []string
	models []string
	err    error
}

func (s *stubSpeaker) Speak(ctx context.Context, text, model string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.spoken = append(s.spoken, text)
	s.models = append(s.models, model)
	return []byte("wav"), nil
}

func (s *stubSpeaker) said() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.spoken...)
}

func (s *stubSpeaker) voices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

type stubPlayer struct {
	mu          sync.Mutex
	plays       int
	interrupted int
	block       chan struct{} // when non-nil, Play waits on it
}

func (p *stubPlayer) Play(ctx context.Context, wav []byte) error {
	p.mu.Lock()
	p.plays++
	block := p.block
	p.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *stubPlayer) Interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interrupted++
}

func (p *stubPlayer) counts() (plays, interrupts int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays, p.interrupted
}

// scriptedResponder yields the configured sentences as a stream.
type scriptedResponder struct {
	mu        sync.Mutex
	sentences []Utterance
	asked     []string
	err       error
}

func (r *scriptedResponder) Respond(ctx context.Context, text string) (<-chan Utterance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, text)
	if r.err != nil {
		return nil, r.err
	}
	ch := make(chan Utterance, len(r.sentences))
	for _, u := range r.sentences {
		ch <- u
	}
	close(ch)
	return ch, nil
}

func (r *scriptedResponder) questions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// ─── Harness ─────────────────────────────────────────────────────────────

type harness struct {
	client *Client
	stt    *scriptedSTT
	tts    *stubSpeaker
	player *stubPlayer
	resp   *scriptedResponder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		stt:    &scriptedSTT{},
		tts:    &stubSpeaker{},
		player: &stubPlayer{},
		resp:   &scriptedResponder{sentences: []Utterance{{Persona: PersonaOtto, Text: "All done."}}},
	}
	c, err := NewClient(ClientOptions{
		Config:    DefaultConfig(t.TempDir()),
		STT:       h.stt,
		TTS:       h.tts,
		Responder: h.resp,
		Player:    h.player,
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.client = c
	return h
}

// utter drives one utterance through the machine as if speech had begun in the
// given state, mirroring what handleUtterances does.
func (h *harness) utter(t *testing.T, prior, transcript string) {
	t.Helper()
	h.stt.push(transcript)
	if prior == StateIdle || prior == StateArmed {
		h.client.setState(StateProcessing)
	}
	h.client.processUtterance(context.Background(), make([]int16, frameSamples), prior)
	if h.client.State() == StateProcessing {
		if prior == StateArmed {
			h.client.setState(StateArmed)
		} else {
			h.client.setState(StateIdle)
		}
	}
}

// ─── Construction ────────────────────────────────────────────────────────

func TestNewClientRequiresDependencies(t *testing.T) {
	base := ClientOptions{
		Config:    DefaultConfig(t.TempDir()),
		STT:       &scriptedSTT{},
		TTS:       &stubSpeaker{},
		Responder: &scriptedResponder{},
	}
	if _, err := NewClient(base); err != nil {
		t.Fatalf("fully wired client should construct: %v", err)
	}
	for name, mutate := range map[string]func(*ClientOptions){
		"no stt":       func(o *ClientOptions) { o.STT = nil },
		"no tts":       func(o *ClientOptions) { o.TTS = nil },
		"no responder": func(o *ClientOptions) { o.Responder = nil },
	} {
		opts := base
		mutate(&opts)
		if _, err := NewClient(opts); err == nil {
			t.Errorf("%s: expected an error at construction rather than a nil panic in the audio loop", name)
		}
	}
}

// ─── Wake behaviour ──────────────────────────────────────────────────────

func TestIdleIgnoresSpeechWithoutWakeWord(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateIdle, "what time is the meeting")

	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle — speech without the wake word must be ignored", got)
	}
	if len(h.resp.questions()) != 0 {
		t.Errorf("responder was asked %q; nothing should reach Otto without the wake word", h.resp.questions())
	}
}

func TestBareWakeWordArmsAndAcknowledges(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateIdle, "otto")

	if got := h.client.State(); got != StateArmed {
		t.Errorf("state = %q, want armed", got)
	}
	if len(h.tts.said()) != 1 {
		t.Fatalf("spoke %q, want exactly one acknowledgment", h.tts.said())
	}
	if len(h.resp.questions()) != 0 {
		t.Error("a bare wake word must not reach Otto")
	}
}

func TestWakeWordWithCommandGoesStraightToOtto(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateIdle, "otto what's on my calendar")

	asked := h.resp.questions()
	if len(asked) != 1 || asked[0] != "what's on my calendar" {
		t.Fatalf("responder asked %q, want the wake word stripped", asked)
	}
	if got := h.client.State(); got != StateArmed {
		t.Errorf("state = %q, want armed so a follow-up needs no wake word", got)
	}
	if !contains(h.tts.said(), "All done.") {
		t.Errorf("spoke %q, want the reply", h.tts.said())
	}
}

func TestArmedFollowUpNeedsNoWakeWord(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateArmed, "and what about tomorrow")

	asked := h.resp.questions()
	if len(asked) != 1 || asked[0] != "and what about tomorrow" {
		t.Fatalf("responder asked %q, want the follow-up verbatim", asked)
	}
}

// ─── Closers and mute ────────────────────────────────────────────────────

func TestCloserEndsConversationWithoutAskingOtto(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateArmed, "thanks")

	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle after a closer", got)
	}
	if len(h.resp.questions()) != 0 {
		t.Error("a closer must not cost a model round-trip")
	}
	if len(h.tts.said()) != 1 {
		t.Errorf("spoke %q, want a single sign-off", h.tts.said())
	}
}

func TestCloserWithTrailingWakeWordStillCloses(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateArmed, "thanks otto")
	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle — a trailing wake word must not hide the closer", got)
	}
}

func TestMuteRequiresWakeWordWhenIdle(t *testing.T) {
	h := newHarness(t)
	// Someone in the room says "shut up" to another person.
	h.utter(t, StateIdle, "shut up")
	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle — ambient speech must not mute Otto", got)
	}

	h.utter(t, StateIdle, "otto shut up")
	if got := h.client.State(); got != StateMuted {
		t.Errorf("state = %q, want muted when addressed", got)
	}
}

func TestMutedIgnoresEverythingButWakeCommand(t *testing.T) {
	h := newHarness(t)
	h.client.setState(StateMuted)

	h.utter(t, StateMuted, "otto what's the weather")
	if got := h.client.State(); got != StateMuted {
		t.Fatalf("state = %q, want still muted — a muted assistant must stay muted", got)
	}
	if len(h.resp.questions()) != 0 {
		t.Error("nothing should reach Otto while muted")
	}

	h.utter(t, StateMuted, "otto wake up")
	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle after an explicit wake command", got)
	}
}

// The bug this guards: transcription still runs while muted (to catch "otto
// wake up"), and an unconditional processing→idle transition on the way out
// would silently un-mute on any stray noise.
func TestMutedSurvivesUnrecognizedNoise(t *testing.T) {
	h := newHarness(t)
	h.client.setState(StateMuted)
	for _, noise := range []string{"mumble mumble", "otto", "hello there"} {
		h.utter(t, StateMuted, noise)
		if got := h.client.State(); got != StateMuted {
			t.Fatalf("after %q state = %q, want still muted", noise, got)
		}
	}
}

func TestMuteWorksMidConversation(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateArmed, "be quiet")
	if got := h.client.State(); got != StateMuted {
		t.Errorf("state = %q, want muted (no wake word needed mid-conversation)", got)
	}
}

// ─── Barge-in ────────────────────────────────────────────────────────────

// Otto says his own name in replies and the mic hears the speakers, so treating
// every wake-word hit during playback as barge-in caused constant
// self-interruption. Only an explicit stop may interrupt.
func TestPlaybackIsNotInterruptedByOrdinarySpeech(t *testing.T) {
	h := newHarness(t)
	h.client.setState(StateSpeaking)
	h.utter(t, StateSpeaking, "otto what about the other thing")

	if got := h.client.State(); got != StateSpeaking {
		t.Errorf("state = %q, want playback to continue", got)
	}
	if _, interrupts := h.player.counts(); interrupts != 0 {
		t.Errorf("interrupted %d times, want 0 for a non-barge-in phrase", interrupts)
	}
	if len(h.resp.questions()) != 0 {
		t.Error("a question heard during playback must wait, not start a second turn")
	}
}

func TestMuteDuringPlaybackInterrupts(t *testing.T) {
	h := newHarness(t)
	h.client.setState(StateSpeaking)
	h.utter(t, StateSpeaking, "otto shut up")

	if got := h.client.State(); got != StateMuted {
		t.Errorf("state = %q, want muted", got)
	}
	if _, interrupts := h.player.counts(); interrupts != 1 {
		t.Errorf("interrupted %d times, want 1", interrupts)
	}
}

func TestCloserDuringPlaybackInterruptsAndAcknowledges(t *testing.T) {
	h := newHarness(t)
	h.client.setState(StateSpeaking)
	h.utter(t, StateSpeaking, "okay thanks")

	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle", got)
	}
	if _, interrupts := h.player.counts(); interrupts != 1 {
		t.Errorf("interrupted %d times, want 1", interrupts)
	}
}

// ─── Streaming and voices ────────────────────────────────────────────────

func TestStreamedSentencesAreSpokenInOrder(t *testing.T) {
	h := newHarness(t)
	h.resp.sentences = []Utterance{
		{Persona: PersonaOtto, Text: "First sentence."},
		{Persona: PersonaOtto, Text: "Second sentence."},
		{Persona: PersonaOtto, Text: "Third sentence."},
	}
	h.utter(t, StateIdle, "otto do the thing")

	want := []string{"First sentence.", "Second sentence.", "Third sentence."}
	got := h.tts.said()
	if len(got) != len(want) {
		t.Fatalf("spoke %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sentence %d = %q, want %q", i, got[i], want[i])
		}
	}
	if plays, _ := h.player.counts(); plays != 3 {
		t.Errorf("played %d clips, want one per sentence", plays)
	}
}

// The busy-handoff is only audible if the pet actually sounds different.
func TestPersonaSelectsItsOwnVoice(t *testing.T) {
	h := newHarness(t)
	h.resp.sentences = []Utterance{{Persona: PersonaToto, Text: "otto's busy. you got me."}}
	h.utter(t, StateIdle, "otto you there")

	voices := h.tts.voices()
	if len(voices) == 0 {
		t.Fatal("nothing was spoken")
	}
	last := voices[len(voices)-1]
	if !strings.Contains(last, "amy") {
		t.Errorf("Toto spoke with %q, want his own voice rather than Otto's", last)
	}
}

func TestReplyIsSanitizedBeforeSpeaking(t *testing.T) {
	h := newHarness(t)
	h.resp.sentences = []Utterance{{Persona: PersonaOtto, Text: "That is **very** important ✅"}}
	h.utter(t, StateIdle, "otto status")

	said := h.tts.said()
	if len(said) == 0 {
		t.Fatal("nothing spoken")
	}
	if strings.Contains(said[0], "*") || strings.Contains(said[0], "✅") {
		t.Errorf("spoke %q; markdown and emoji must not reach piper", said[0])
	}
}

// ─── Failure handling ────────────────────────────────────────────────────

func TestTranscribeFailureIsNonFatal(t *testing.T) {
	h := newHarness(t)
	h.stt.err = errors.New("whisper exploded")
	h.utter(t, StateIdle, "otto do it")

	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle — a transcription failure must not wedge the listener", got)
	}
	if !hasErrorEvent(h.client) {
		t.Error("expected an ErrorEvent so the UI can surface the failure")
	}
}

func TestResponderFailureReturnsToIdle(t *testing.T) {
	h := newHarness(t)
	h.resp.err = errors.New("otto is on fire")
	h.utter(t, StateIdle, "otto do it")

	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle", got)
	}
	if !hasErrorEvent(h.client) {
		t.Error("expected an ErrorEvent")
	}
}

func TestEmptyTranscriptIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateIdle, "")
	if len(h.resp.questions()) != 0 || len(h.tts.said()) != 0 {
		t.Error("an empty transcript must produce no action at all")
	}
}

// whisper renders a silent room as "[BLANK_AUDIO]"; without cleaning, every
// silence would run a wake-word check against literal noise.
func TestNoiseAnnotationIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateIdle, "[BLANK_AUDIO]")
	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want idle", got)
	}
	if len(h.resp.questions()) != 0 {
		t.Error("a noise annotation must not reach Otto")
	}
}

// ─── External controls ───────────────────────────────────────────────────

func TestMuteUnmuteFromKeyboard(t *testing.T) {
	h := newHarness(t)
	h.client.Mute()
	if !h.client.IsMuted() {
		t.Error("Mute should leave the client muted")
	}
	if _, interrupts := h.player.counts(); interrupts != 1 {
		t.Error("Mute should interrupt any in-flight playback")
	}
	h.client.Unmute()
	if h.client.IsMuted() || h.client.State() != StateIdle {
		t.Errorf("state = %q, want idle after Unmute", h.client.State())
	}
}

// A stalled consumer must never stall audio capture, so emit drops rather than
// blocks once the buffer fills.
func TestEmitNeverBlocks(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			h.client.emit(LevelEvent{RMS: 0.5})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked when the event buffer filled")
	}
}

func TestStartWithoutSoxReportsClearly(t *testing.T) {
	h := newHarness(t)
	t.Setenv("PATH", t.TempDir()) // no sox anywhere
	err := h.client.Start(context.Background())
	if err == nil {
		t.Fatal("expected an error when sox is unavailable")
	}
	if !strings.Contains(err.Error(), "sox") {
		t.Errorf("error %q should name the missing binary", err)
	}
	if got := h.client.State(); got != StateOff {
		t.Errorf("state = %q, want off", got)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────

func hasErrorEvent(c *Client) bool {
	for {
		select {
		case ev := <-c.Events():
			if _, ok := ev.(ErrorEvent); ok {
				return true
			}
		default:
			return false
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
