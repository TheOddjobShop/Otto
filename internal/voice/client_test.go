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
// being tested by hand. Most of it is decisions about *when not to act* — the
// muted overlay, the armed-follow-up path, what closes a conversation — and
// every one of them was a real bug at some point.
//
// The microphone is a stub like everything else (fakeCapture), which is what
// makes the central guarantee checkable: Otto releases the capture device for
// the whole of thinking and speaking, so he cannot hear himself.

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

func (s *scriptedSTT) pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.next...)
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
	// onPlay runs while audio is notionally coming out of the speakers, which
	// is the only moment at which "is the microphone open?" is worth asking.
	onPlay func()
}

func (p *stubPlayer) Play(ctx context.Context, wav []byte) error {
	p.mu.Lock()
	p.plays++
	block := p.block
	hook := p.onPlay
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
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

// fakeCapture is a microphone under test control.
//
// It reports whether a capture session is live, which is the observable the
// whole gating design rests on: sessions is not "did the state machine intend
// to stop recording" but "is there a process holding the device".
type fakeCapture struct {
	feed  chan []int16
	avail error

	mu     sync.Mutex
	opens  int
	live   bool
	closes int
}

func newFakeCapture() *fakeCapture {
	return &fakeCapture{feed: make(chan []int16, 256)}
}

func (f *fakeCapture) Available() error { return f.avail }

func (f *fakeCapture) Capture(ctx context.Context, out chan<- []int16) error {
	f.mu.Lock()
	f.opens++
	f.live = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.live = false
		f.closes++
		f.mu.Unlock()
		// Killing sox discards whatever the kernel had buffered, so anything
		// queued when the gate shut is gone. Without this the fake would hand
		// the *next* session audio recorded before Otto started speaking — the
		// one thing the gate exists to prevent.
		for {
			select {
			case <-f.feed:
			default:
				return
			}
		}
	}()

	// Real hardware starts producing audio the instant it opens, and the client
	// discards a settling period for exactly that reason. Emitting it here
	// keeps every test from having to know the settle length.
	for i := 0; i < micSettleMs/frameMs; i++ {
		select {
		case out <- make([]int16, frameSamples):
		case <-ctx.Done():
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-f.feed:
			select {
			case out <- frame:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (f *fakeCapture) isLive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

func (f *fakeCapture) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// loudFrame is comfortably above baseFloor; quietFrame is digital silence.
func loudFrame() []int16 {
	f := make([]int16, frameSamples)
	for i := range f {
		f[i] = 8000
	}
	return f
}

func quietFrame() []int16 { return make([]int16, frameSamples) }

// say feeds n frames of speech followed by enough silence to endpoint an
// utterance that began in the given state.
func (f *fakeCapture) say(c *Client, speechFrames int, startState string) {
	for i := 0; i < speechFrames; i++ {
		f.feed <- loudFrame()
	}
	for i := 0; i < c.endSilenceFrames(startState)+1; i++ {
		f.feed <- quietFrame()
	}
}

// ─── Harness ─────────────────────────────────────────────────────────────

type harness struct {
	client  *Client
	stt     *scriptedSTT
	tts     *stubSpeaker
	player  *stubPlayer
	resp    *scriptedResponder
	capture *fakeCapture
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

// newHarnessWith builds a client, letting a test adjust the options first —
// timings, mostly, since the real ones are measured in seconds.
func newHarnessWith(t *testing.T, tweak func(*ClientOptions)) *harness {
	t.Helper()
	h := &harness{
		stt:     &scriptedSTT{},
		tts:     &stubSpeaker{},
		player:  &stubPlayer{},
		resp:    &scriptedResponder{sentences: []Utterance{{Persona: PersonaOtto, Text: "All done."}}},
		capture: newFakeCapture(),
	}
	opts := ClientOptions{
		Config:    DefaultConfig(t.TempDir()),
		STT:       h.stt,
		TTS:       h.tts,
		Responder: h.resp,
		Player:    h.player,
		Capture:   h.capture,
		Logger:    log.New(io.Discard, "", 0),
	}
	if tweak != nil {
		tweak(&opts)
	}
	c, err := NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.client = c
	return h
}

// start runs the full loop for the duration of the test.
func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.client.Start(ctx); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the voice loop did not shut down")
		}
	})
	waitFor(t, "the microphone to open", h.capture.isLive)
}

// waitFor polls until cond holds, which is how anything driven by the capture
// goroutines has to be observed.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// utter drives one utterance through the machine as if speech had begun in the
// given state, mirroring what handleUtterances does.
//
// Only an armed utterance flips to processing: that is the transition that
// closes the microphone, and doing it for an idle wake-word check would deafen
// Otto for the length of every ambient noise's transcription.
func (h *harness) utter(t *testing.T, prior, transcript string) {
	t.Helper()
	h.stt.push(transcript)
	if prior == StateArmed {
		h.client.setState(StateProcessing)
	}
	h.client.processUtterance(context.Background(), make([]int16, frameSamples), prior)
	if h.client.State() == StateProcessing {
		h.client.setState(StateArmed)
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

// ─── The microphone gate ─────────────────────────────────────────────────
//
// Barge-in used to live here: three tests pinning down which phrases were
// allowed to interrupt playback and which had to wait. All of it existed
// because the microphone stayed open while Otto talked, and none of it worked
// reliably — Otto says his own name in replies, so the speakers kept tripping
// his own wake word.
//
// The device is now released for the whole of think-and-speak, so the question
// those tests asked cannot arise. What replaces them is the guarantee that
// makes that true: in every state where Otto is producing sound, the microphone
// is off.

// The table is the specification. If a state is added, this fails until someone
// has decided whether Otto records in it.
func TestMicrophoneIsOffWheneverOttoMakesSound(t *testing.T) {
	for _, tc := range []struct {
		state string
		live  bool
	}{
		{StateIdle, true},        // waiting for the wake word
		{StateArmed, true},       // capturing a request
		{StateMuted, true},       // silent, but still owes us "otto wake up"
		{StateProcessing, false}, // transcribing and thinking
		{StateSpeaking, false},   // the speakers are producing audio
		{StateInstalling, false},
		{StateOff, false},
	} {
		if got := micLive(tc.state); got != tc.live {
			t.Errorf("micLive(%q) = %v, want %v", tc.state, got, tc.live)
		}
	}
}

// Every transition runs through setState, so the gate cannot be left open by a
// path that forgot to close it.
func TestSetStateDrivesTheGate(t *testing.T) {
	h := newHarness(t)
	h.client.syncGate() // what Start does for the constructed-idle state
	if !h.client.MicOpen() {
		t.Fatal("a client that reports idle must have its microphone open")
	}
	for _, state := range []string{StateProcessing, StateSpeaking, StateArmed, StateMuted, StateIdle, StateOff} {
		h.client.setState(state)
		if got := h.client.MicOpen(); got != micLive(state) {
			t.Errorf("state %q: mic open = %v, want %v", state, got, micLive(state))
		}
	}
}

// A full turn, from the wake word to being re-armed, watched through the
// microphone rather than through the state names.
func TestTurnClosesTheMicrophoneUntilTheReplyIsFinished(t *testing.T) {
	h := newHarness(t)
	h.player.onPlay = func() {
		if h.client.MicOpen() {
			t.Error("the microphone was open while a reply was playing")
		}
	}
	h.utter(t, StateIdle, "otto what's on my calendar")

	if got := h.client.State(); got != StateArmed {
		t.Errorf("state = %q, want armed for a follow-up", got)
	}
	if !h.client.MicOpen() {
		t.Error("the microphone must come back on once the reply has finished")
	}
}

// The acknowledgment for a bare wake word is spoken audio like any other, so it
// gets the same treatment — otherwise Otto's "Yes?" is the first thing captured
// as the request.
func TestAcknowledgmentIsSpokenWithTheMicrophoneClosed(t *testing.T) {
	h := newHarness(t)
	h.player.onPlay = func() {
		if h.client.MicOpen() {
			t.Error("the microphone was open while the greeting played")
		}
	}
	h.utter(t, StateIdle, "hey otto")

	if got := h.client.State(); got != StateArmed {
		t.Errorf("state = %q, want armed", got)
	}
	if !h.client.MicOpen() {
		t.Error("the microphone must reopen to hear the request")
	}
}

// A piper failure must not leave the device shut for a phrase that never plays.
func TestFailedAcknowledgmentStillReopensTheMicrophone(t *testing.T) {
	h := newHarness(t)
	h.tts.err = errors.New("piper exploded")
	h.utter(t, StateIdle, "hey otto")

	if got := h.client.State(); got != StateArmed {
		t.Errorf("state = %q, want armed even though the greeting failed", got)
	}
	if !h.client.MicOpen() {
		t.Error("a TTS failure must not strand the microphone closed")
	}
}

// ─── Ending the conversation ─────────────────────────────────────────────

func TestDismissalEndsTheConversation(t *testing.T) {
	for _, phrase := range []string{
		"otto go away", "go away", "leave me alone", "that'll be all",
		"you can go", "stand down", "end conversation",
	} {
		h := newHarness(t)
		h.utter(t, StateArmed, phrase)
		if got := h.client.State(); got != StateIdle {
			t.Errorf("%q: state = %q, want idle", phrase, got)
		}
		if len(h.resp.questions()) != 0 {
			t.Errorf("%q reached Otto; a dismissal must not cost a model call", phrase)
		}
	}
}

// Saying "bye" to a room, with the wake word attached, must not be answered —
// acknowledging it would open the conversation it is trying to close.
func TestDismissalWhileIdleIsNotAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.utter(t, StateIdle, "otto go away")

	if got := h.client.State(); got != StateIdle {
		t.Errorf("state = %q, want to stay idle", got)
	}
	if len(h.tts.said()) != 0 {
		t.Errorf("said %v, want silence — there was nothing to end", h.tts.said())
	}
}

// An answered conversation stays open for follow-ups, but not forever: without
// a bound, every word spoken in the room afterwards is sent to the model.
func TestAbandonedConversationTimesOut(t *testing.T) {
	h := newHarnessWith(t, func(o *ClientOptions) {
		o.Config.ConversationTimeoutSec = 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.client.watchConversation(ctx)

	h.client.setState(StateArmed)
	waitFor(t, "the conversation to close itself", func() bool {
		return h.client.State() == StateIdle
	})
}

func TestConversationTimeoutIsDeferredBySpeech(t *testing.T) {
	h := newHarnessWith(t, func(o *ClientOptions) {
		o.Config.ConversationTimeoutSec = 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.client.watchConversation(ctx)

	h.client.setState(StateArmed)
	// Keep talking for longer than the timeout.
	for i := 0; i < 15; i++ {
		h.client.noteActivity()
		time.Sleep(100 * time.Millisecond)
	}
	if got := h.client.State(); got != StateArmed {
		t.Errorf("state = %q, want armed — somebody was still talking", got)
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
	t.Setenv("PATH", t.TempDir()) // no sox anywhere
	if err := (SoxCapture{}).Available(); err == nil {
		t.Fatal("expected an error when sox is unavailable")
	} else if !strings.Contains(err.Error(), "sox") {
		t.Errorf("error %q should name the missing binary", err)
	}

	h := newHarnessWith(t, func(o *ClientOptions) {
		o.Capture = &fakeCapture{avail: errors.New("sox not installed")}
	})
	err := h.client.Start(context.Background())
	if err == nil {
		t.Fatal("Start must fail when the capture device is unavailable")
	}
	if got := h.client.State(); got != StateOff {
		t.Errorf("state = %q, want off", got)
	}
	if h.client.MicOpen() {
		t.Error("a client that could not start must not report an open microphone")
	}
}

// ─── The loop, end to end ────────────────────────────────────────────────

// Audio in, a turn out, and no capture session alive while Otto works. This is
// the only test that exercises the capture goroutines together, and it is the
// one that would fail if any of them stopped honoring the gate.
func TestFullCycleReleasesAndReopensTheDevice(t *testing.T) {
	h := newHarness(t)
	// Checked at the moment audio plays. The gate is the synchronous truth —
	// the capture process is torn down microseconds behind it — so this is the
	// assertion that cannot race.
	h.player.onPlay = func() {
		if h.client.MicOpen() {
			t.Error("the gate was open while a reply was playing")
		}
	}
	h.stt.push("otto what's on my calendar")
	h.start(t)

	before := h.capture.sessions()
	h.capture.say(h.client, 5, StateIdle)

	waitFor(t, "the conversation to re-arm", func() bool { return h.client.State() == StateArmed })
	// A second session can only exist because the first one was torn down: the
	// device really was released for the turn, not merely ignored.
	waitFor(t, "a fresh capture session", func() bool {
		return h.capture.sessions() > before && h.capture.isLive()
	})

	if asked := h.resp.questions(); len(asked) != 1 || asked[0] != "what's on my calendar" {
		t.Errorf("Otto was asked %v, want one question with the wake word stripped", asked)
	}
}

// Ambient speech with no wake word must not close the device: Otto is only
// checking whether he was addressed, and going deaf for the length of every
// passing conversation would lose the wake word that follows it.
func TestWakeWordCheckKeepsTheDeviceOpen(t *testing.T) {
	h := newHarness(t)
	h.stt.push("so anyway I told him it was fine")
	h.start(t)

	before := h.capture.sessions()
	h.capture.say(h.client, 5, StateIdle)

	waitFor(t, "the utterance to be transcribed", func() bool { return len(h.stt.pending()) == 0 })
	if !h.capture.isLive() {
		t.Error("the microphone was closed to transcribe a wake-word candidate")
	}
	if got := h.capture.sessions(); got != before {
		t.Errorf("capture sessions = %d, want the same session (was %d)", got, before)
	}
	if len(h.resp.questions()) != 0 {
		t.Error("speech without the wake word must not reach Otto")
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
