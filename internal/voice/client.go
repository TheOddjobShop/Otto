package voice

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// The listening loop: mic → frames → utterances → transcript → Otto → speech.
//
// One turn of the cycle, with the microphone's state on the right:
//
//	idle        waiting for the wake word                       mic ON
//	  │ "hey otto, what's on my calendar"
//	  ▼
//	armed       capturing the request until endSilence          mic ON
//	  │ 2 s of silence ends the utterance
//	  ▼
//	processing  transcribe, then Otto thinks and acts           mic OFF
//	  ▼
//	speaking    piper renders, the speakers play                mic OFF
//	  ▼
//	armed       follow-ups need no wake word                    mic ON
//	  │ "otto, go away" — or conversationTimeout expires
//	  ▼
//	idle
//
//	muted is an overlay reachable from idle or armed; only a wake command
//	leaves it. The microphone stays on there, because muted means Otto does not
//	speak, not that he stops listening for permission to.
//
// The microphone column is the whole design. Between the moment a request is
// endpointed and the moment the reply finishes playing, no capture process
// exists — so Otto physically cannot hear himself, and none of the heuristics
// that used to guess at loopback are needed. The cost is that speech cannot
// interrupt a reply; a long answer plays to the end.

// ─── Events ──────────────────────────────────────────────────────────────

// Event is anything the client emits. Consumers type-switch on it.
type Event interface{ voiceEvent() }

// LevelEvent carries the current mic RMS in [0,1], roughly ten times a second.
// A zero level is emitted once when the device is released, so a meter driven
// by this does not freeze at whatever the last live frame happened to be.
type LevelEvent struct{ RMS float64 }

// StateEvent fires on every state transition.
type StateEvent struct{ State string }

// MicEvent fires when the capture device is opened or released. Distinct from
// StateEvent because "is the microphone on" is the one thing a user of a
// voice assistant most wants shown truthfully, and inferring it from a state
// name would put that inference in every consumer.
type MicEvent struct{ Open bool }

// TranscriptEvent fires when a user utterance was recognized and accepted.
// Text is the command with the wake word already stripped, and is empty when
// only the wake word was said.
type TranscriptEvent struct {
	Text string
	Raw  string
}

// ReplyEvent fires when Otto's reply is known. Audio may still be playing.
type ReplyEvent struct {
	UserText  string
	ReplyText string
	Persona   string
}

// ErrorEvent is a non-fatal error; the client keeps listening.
type ErrorEvent struct{ Err error }

func (LevelEvent) voiceEvent()      {}
func (StateEvent) voiceEvent()      {}
func (MicEvent) voiceEvent()        {}
func (TranscriptEvent) voiceEvent() {}
func (ReplyEvent) voiceEvent()      {}
func (ErrorEvent) voiceEvent()      {}

// States.
const (
	StateInstalling = "installing"
	StateIdle       = "idle"
	StateArmed      = "armed"
	StateProcessing = "processing"
	StateSpeaking   = "speaking"
	StateMuted      = "muted"
	StateOff        = "off"
)

// micLive reports whether the capture device should be running in a state.
//
// This single function is the contract the rest of the file relies on: every
// transition through setState opens or releases the microphone according to it,
// so there is exactly one place where "is Otto recording right now" is decided
// and no path can forget to close the device.
func micLive(state string) bool {
	switch state {
	case StateIdle, StateArmed, StateMuted:
		return true
	default:
		// processing, speaking, installing, off.
		return false
	}
}

// ─── Tuning ──────────────────────────────────────────────────────────────

const (
	// minSpeechMsIdle filters ambient blips (a chair creak, a keypress) while
	// waiting for the wake word. "Otto" is two syllables and lands well above.
	minSpeechMsIdle = 400
	// minSpeechMsArmed is far looser because mid-conversation closers —
	// "thanks", "yeah", "no", "bye" — are genuinely 150–300 ms. Anything
	// stricter swallows them, and then the conversation cannot be ended
	// without saying the wake word again.
	minSpeechMsArmed = 150

	// endSilenceMsWake ends a wake-word utterance while idle. Short, because
	// nothing is being composed yet — the sooner "hey otto" is transcribed, the
	// sooner Otto answers.
	endSilenceMsWake = 750

	// preRollMs of audio before speech onset is prepended to each utterance, so
	// the first syllable is not clipped.
	preRollMs = 300

	// micSettleMs of audio is discarded after the device opens. It covers two
	// things at once: the click most capture hardware produces on open, and the
	// tail of Otto's last sentence still moving through the speaker and the
	// operating system's output buffer after the player process has exited.
	micSettleMs = 300

	// noiseFloorGain multiplies the adapted noise floor to get the speech
	// threshold; baseFloor stops a silent room from adapting the floor to zero
	// and making every rustle count as speech.
	noiseFloorGain = 2.8
	baseFloor      = 0.02

	// micRetryMaxSec caps the backoff after a capture failure. A microphone
	// that has been unplugged should not spin, but it should also start working
	// again within a few seconds of being plugged back in.
	micRetryMaxSec = 5

	// transcribeTimeout bounds one whisper invocation.
	transcribeTimeout = 60 * time.Second
	// speakTimeout bounds one piper invocation.
	speakTimeout = 60 * time.Second
)

// ─── Client ──────────────────────────────────────────────────────────────

// Utterance is one chunk of speech to say, tagged with who is saying it.
type Utterance struct {
	Persona string
	Text    string
}

// Responder turns a user's transcript into spoken output. It returns a channel
// of utterances, closed when the turn is complete.
//
// A channel rather than a string is what makes streaming speech possible: the
// Otto side pushes each sentence as the model produces it, so playback starts
// while generation is still running. A responder that has nothing to stream can
// simply send one utterance and close.
type Responder interface {
	Respond(ctx context.Context, text string) (<-chan Utterance, error)
}

// ResponderFunc adapts a function to Responder.
type ResponderFunc func(ctx context.Context, text string) (<-chan Utterance, error)

func (f ResponderFunc) Respond(ctx context.Context, text string) (<-chan Utterance, error) {
	return f(ctx, text)
}

// PlaybackDevice plays rendered audio. An interface so the speaking path is
// testable without a sound card.
type PlaybackDevice interface {
	Play(ctx context.Context, wav []byte) error
	Interrupt()
}

// Client owns the microphone and drives the conversation loop.
type Client struct {
	cfg       Config
	stt       Transcriber
	tts       Speaker
	player    PlaybackDevice
	capture   CaptureDevice
	cache     *Cache
	responder Responder
	logger    *log.Logger

	gate *micGate

	events    chan Event
	closeOnce sync.Once
	closed    atomic.Bool

	mu    sync.Mutex
	state string
	// lastActive is when the user was last heard or answered. It exists only to
	// close an abandoned conversation: without it, one stray word after Otto
	// finishes speaking is sent to the model as though it were addressed to
	// him, forever.
	lastActive time.Time
}

// ClientOptions configures a Client. STT, TTS and Responder are required.
type ClientOptions struct {
	Config    Config
	STT       Transcriber
	TTS       Speaker
	Responder Responder
	// Logger receives the diagnostic trail. Voice failures are notoriously
	// hard to reason about after the fact ("it just didn't hear me"), so every
	// utterance, transcript, wake decision, microphone open/close and state
	// change is logged.
	Logger *log.Logger
	// Player overrides the audio output device. Nil uses the real one.
	Player PlaybackDevice
	// Capture overrides the microphone. Nil uses sox.
	Capture CaptureDevice
}

// NewClient builds a Client. Returns an error when a required dependency is
// missing, rather than nil-panicking later inside the audio loop.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.STT == nil {
		return nil, fmt.Errorf("voice: no transcriber configured")
	}
	if opts.TTS == nil {
		return nil, fmt.Errorf("voice: no speaker configured")
	}
	if opts.Responder == nil {
		return nil, fmt.Errorf("voice: no responder configured")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	player := opts.Player
	if player == nil {
		player = &Player{}
	}
	capture := opts.Capture
	if capture == nil {
		capture = SoxCapture{}
	}
	return &Client{
		cfg:        opts.Config,
		stt:        opts.STT,
		tts:        opts.TTS,
		player:     player,
		capture:    capture,
		cache:      NewCache(opts.Config.Dir),
		responder:  opts.Responder,
		logger:     logger,
		gate:       newMicGate(),
		events:     make(chan Event, 64),
		state:      StateIdle,
		lastActive: time.Now(),
	}, nil
}

// Events returns the read-only event stream.
func (c *Client) Events() <-chan Event { return c.events }

// State returns the current state.
func (c *Client) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// IsMuted reports whether the client is muted.
func (c *Client) IsMuted() bool { return c.State() == StateMuted }

// MicOpen reports whether the capture device is currently allowed to run.
func (c *Client) MicOpen() bool { return c.gate.IsOpen() }

// Mute silences Otto, killing any in-flight playback. Idempotent.
//
// This is the keyboard affordance, and it is the one thing that still stops a
// reply mid-sentence — a keypress is unambiguous intent in a way that a phrase
// picked up by a microphone is not.
func (c *Client) Mute() {
	c.logger.Printf("mute requested externally")
	c.player.Interrupt()
	c.setState(StateMuted)
}

// Unmute returns to idle from muted.
func (c *Client) Unmute() {
	c.logger.Printf("unmute requested externally")
	c.setState(StateIdle)
}

// Start runs the loop until ctx is cancelled. A returned error means the
// capture pipeline could not be brought up at all; recoverable problems arrive
// as ErrorEvent and the loop keeps trying.
func (c *Client) Start(ctx context.Context) error {
	defer c.closeEvents()

	if err := c.capture.Available(); err != nil {
		c.setState(StateOff)
		c.emit(ErrorEvent{Err: err})
		return err
	}

	frames := make(chan micFrame, 32)
	utterances := make(chan capturedUtterance, 4)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); c.captureLoop(ctx, frames) }()
	go func() { defer wg.Done(); c.detectUtterances(ctx, frames, utterances) }()
	go func() { defer wg.Done(); c.handleUtterances(ctx, utterances) }()
	go func() { defer wg.Done(); c.watchConversation(ctx) }()

	c.logger.Printf("voice loop started (wake=%q endSilence=%s convTimeout=%s)",
		c.cfg.Wake(), c.cfg.RequestEndSilence(), c.cfg.ConversationTimeout())
	// The client is constructed already idle, so setState has no transition to
	// make and would not touch the gate. Open it explicitly — this is the one
	// place the device starts without a state change to carry it.
	c.setState(StateIdle)
	c.syncGate()

	<-ctx.Done()
	c.gate.Close()
	wg.Wait()
	return nil
}

// WarmCache pre-renders the canned phrases for every persona voice. Intended to
// run in the background at startup; failure is non-fatal (live synthesis still
// works), so callers log and continue.
func (c *Client) WarmCache(ctx context.Context) error {
	for _, persona := range []string{PersonaOtto, PersonaToto, PersonaToot} {
		if err := c.cache.Warm(ctx, c.tts, c.cfg.VoiceFor(persona)); err != nil {
			return err
		}
	}
	return nil
}

// ─── Stage 1: the gated capture loop ─────────────────────────────────────

// micFrame is one 100 ms frame, flagged when it is the first of a new capture
// session. The flag matters because the audio either side of a gate close is
// not contiguous: the device was shut and reopened in between, and a half-
// captured utterance from before must not be glued to whatever comes after.
type micFrame struct {
	samples []int16
	reset   bool
}

// captureLoop opens the device whenever the gate allows and releases it the
// moment the gate closes, forever.
func (c *Client) captureLoop(ctx context.Context, out chan<- micFrame) {
	defer close(out)

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.gate.Opened():
		}

		sess, cancel := context.WithCancel(ctx)
		// Cancel the session as soon as the gate shuts. The goroutine also
		// exits when the session ends on its own, so it cannot outlive the
		// session it was started for.
		go func() {
			select {
			case <-c.gate.Closed():
			case <-sess.Done():
			}
			cancel()
		}()

		c.logger.Printf("mic: opening capture device")
		c.emit(MicEvent{Open: true})
		err := c.captureSession(sess, out)
		cancel()
		c.emit(MicEvent{Open: false})
		c.emit(LevelEvent{RMS: 0})
		c.logger.Printf("mic: capture device released")

		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			continue
		}

		failures++
		c.logger.Printf("mic: capture failed (attempt %d): %v", failures, err)
		c.emit(ErrorEvent{Err: fmt.Errorf("microphone: %w", err)})
		// The gate is still open — without a backoff a permanently broken
		// device would respawn sox as fast as the kernel could fail it.
		delay := time.Duration(min(failures, micRetryMaxSec)) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// captureSession runs one open-to-close lifetime of the device.
func (c *Client) captureSession(ctx context.Context, out chan<- micFrame) error {
	raw := make(chan []int16, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.capture.Capture(ctx, raw)
		close(raw)
	}()

	settle := micSettleMs / frameMs
	first := true
	for frame := range raw {
		if settle > 0 {
			settle--
			continue
		}
		c.emit(LevelEvent{RMS: rms(frame)})
		select {
		case out <- micFrame{samples: frame, reset: first}:
			first = false
		case <-ctx.Done():
			// Keep draining so Capture is never blocked on an unread channel;
			// it is on its way out and closing raw is what ends this loop.
		}
	}
	return <-done
}

// ─── Stage 2: VAD / utterance assembly ───────────────────────────────────

// capturedUtterance bundles audio with the state speech *started* in. The start
// state decides how the utterance is interpreted — a wake-word candidate, a
// request, or a phrase heard while muted — and by the time the silence detector
// flushes it the current state may already have moved on.
type capturedUtterance struct {
	samples    []int16
	startState string
}

func (c *Client) detectUtterances(ctx context.Context, in <-chan micFrame, out chan<- capturedUtterance) {
	defer close(out)

	ring := newFrameRing(preRollMs / frameMs)
	var speech []int16
	var startState string
	// The noise floor is a property of the room, so it deliberately survives
	// the device being closed and reopened — re-adapting from scratch after
	// every reply would make the first utterance of each turn the least
	// reliable one.
	noiseFloor := baseFloor
	speechFrames, silenceFrames := 0, 0
	inSpeech := false

	discard := func() {
		speech = speech[:0]
		speechFrames, silenceFrames = 0, 0
		inSpeech = false
		ring.reset()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case mf, ok := <-in:
			if !ok {
				return
			}
			if mf.reset && inSpeech {
				c.logger.Printf("utterance abandoned: device was closed mid-speech")
				discard()
			} else if mf.reset {
				discard()
			}

			frame := mf.samples
			state := c.State()
			level := rms(frame)
			threshold := max(baseFloor, noiseFloor*noiseFloorGain)

			if level > threshold {
				if !inSpeech {
					speech = speech[:0]
					startState = state
					for _, f := range ring.drain() {
						speech = append(speech, f...)
					}
					inSpeech = true
					// Somebody is talking; the conversation is not abandoned.
					c.noteActivity()
				}
				speech = append(speech, frame...)
				speechFrames++
				silenceFrames = 0
				continue
			}

			// Quiet frame: adapt the noise floor toward the ambient level.
			noiseFloor = 0.96*noiseFloor + 0.04*level

			if !inSpeech {
				ring.push(frame)
				continue
			}

			speech = append(speech, frame...)
			silenceFrames++

			if silenceFrames < c.endSilenceFrames(startState) {
				continue
			}

			minMs := minSpeechMsIdle
			if startState == StateArmed {
				minMs = minSpeechMsArmed
			}
			minFrames := max(1, minMs/frameMs)

			if speechFrames >= minFrames {
				clone := make([]int16, len(speech))
				copy(clone, speech)
				c.logger.Printf("utterance: %d samples (~%dms) startState=%s floor=%.4f",
					len(clone), len(clone)*1000/sampleRate, startState, noiseFloor)
				select {
				case out <- capturedUtterance{samples: clone, startState: startState}:
				default:
					// Dropping is correct under backpressure: the processor is
					// still working on the previous utterance, and queueing
					// would answer a stale question late.
					c.logger.Printf("utterance dropped: processor busy")
				}
			} else {
				c.logger.Printf("utterance too short: %d frames (need %d)", speechFrames, minFrames)
			}
			discard()
		}
	}
}

// endSilenceFrames is how much trailing silence ends an utterance that began in
// the given state.
//
// A request gets the long endpoint — two seconds by default — because the user
// is composing a thought out loud and pausing mid-sentence is normal. Waiting
// for the wake word gets a short one, because there is nothing to compose and
// latency to the first acknowledgment is the whole feel of the thing.
func (c *Client) endSilenceFrames(startState string) int {
	ms := endSilenceMsWake
	if startState == StateArmed {
		ms = int(c.cfg.RequestEndSilence() / time.Millisecond)
	}
	return max(1, ms/frameMs)
}

// ─── Stage 3: transcript → decision → speech ─────────────────────────────

func (c *Client) handleUtterances(ctx context.Context, in <-chan capturedUtterance) {
	for {
		select {
		case <-ctx.Done():
			return
		case utt, ok := <-in:
			if !ok {
				return
			}
			prior := utt.startState
			// An armed utterance is a request, so the microphone goes off here
			// — before transcription, and for the whole of think and speak.
			//
			// An idle one must not close the device: it is only a check for the
			// wake word, and going deaf for the few hundred milliseconds
			// whisper takes would drop the very next thing said, which is quite
			// often the wake word itself.
			if prior == StateArmed {
				c.setState(StateProcessing)
			}
			c.processUtterance(ctx, utt.samples, prior)
			// Nothing advanced the state, so the request produced no turn.
			// Return to the conversation rather than dropping out of it.
			if c.State() == StateProcessing {
				c.setState(StateArmed)
			}
		}
	}
}

func (c *Client) processUtterance(ctx context.Context, samples []int16, prior string) {
	tctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	text, err := c.stt.Transcribe(tctx, pcmToWav(samples, sampleRate))
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.logger.Printf("transcribe failed: %v", err)
		c.emit(ErrorEvent{Err: fmt.Errorf("transcribe: %w", err)})
		return
	}
	if text == "" {
		c.logger.Printf("empty transcript, ignoring")
		return
	}

	wake := c.cfg.Wake()
	command, hit := StripWakeWord(text, wake)
	c.logger.Printf("heard %q (prior=%s hit=%v command=%q)", text, prior, hit, command)

	variants := []string{text, command, StripTrailingWake(text, wake)}

	switch prior {
	case StateMuted:
		// Muted: only an explicit wake command gets through. Everything else is
		// discarded without a transcript event, because a muted assistant that
		// still narrates what it heard is not muted in any useful sense.
		if hit && IsWakeCommand(command) {
			c.logger.Printf("wake command while muted → idle")
			c.emit(TranscriptEvent{Text: command, Raw: text})
			c.speakAck(ctx, PickUnmuteAck(), StateIdle)
			return
		}
		c.logger.Printf("muted: ignoring")
		return

	case StateArmed:
		// Mid-conversation: closers and mutes need no wake word, since the user
		// is already talking to Otto. Both are fast paths with no model call,
		// so ending a conversation is instant.
		if AnyMatches(variants, IsCloserCommand) {
			c.logger.Printf("closer → idle")
			c.emit(TranscriptEvent{Text: command, Raw: text})
			c.speakAck(ctx, PickCloserAck(), StateIdle)
			return
		}
		if AnyMatches(variants, IsMuteCommand) {
			c.logger.Printf("mute → muted")
			c.emit(TranscriptEvent{Text: command, Raw: text})
			c.setState(StateMuted)
			return
		}
		c.emit(TranscriptEvent{Text: text, Raw: text})
		c.respond(ctx, text)
		return
	}

	// Idle. Outside a conversation, muting requires the wake word so an
	// ambient "shut up" aimed at someone else does not silence Otto.
	if hit && IsMuteCommand(command) {
		c.logger.Printf("wake+mute → muted")
		c.emit(TranscriptEvent{Text: command, Raw: text})
		c.setState(StateMuted)
		return
	}
	if !hit {
		c.logger.Printf("no wake word, ignoring")
		return
	}
	// A closer said with the wake word while already idle asks for nothing.
	// Acknowledging it would start the very conversation it is trying to end.
	if command != "" && IsCloserCommand(command) {
		c.logger.Printf("closer while idle, already closed")
		return
	}
	if command == "" {
		// Wake word alone: acknowledge and wait for the request. speakAck takes
		// the state through speaking (device off) and lands on armed, so the
		// greeting is never captured as the request.
		c.logger.Printf("bare wake word → armed")
		c.emit(TranscriptEvent{Text: "", Raw: text})
		c.speakAck(ctx, PickGreeting(), StateArmed)
		return
	}
	// Wake word and request in one breath.
	c.emit(TranscriptEvent{Text: command, Raw: text})
	c.respond(ctx, command)
}

// respond hands the transcript to Otto and speaks each sentence as it arrives.
//
// The microphone is already off when this is called and stays off until the
// last sentence has played, which is what makes streaming safe: earlier the
// first spoken sentence would be captured and evaluated as a barge-in against
// the ones still being generated.
func (c *Client) respond(ctx context.Context, userText string) {
	c.setState(StateProcessing)
	start := time.Now()

	stream, err := c.responder.Respond(ctx, userText)
	if err != nil {
		c.logger.Printf("respond failed after %s: %v", time.Since(start), err)
		c.emit(ErrorEvent{Err: fmt.Errorf("respond: %w", err)})
		c.setState(StateIdle)
		return
	}

	spoken := 0
	for utt := range stream {
		if utt.Text == "" {
			continue
		}
		if spoken == 0 {
			c.logger.Printf("first sentence after %s", time.Since(start))
		}
		spoken++
		c.emit(ReplyEvent{UserText: userText, ReplyText: utt.Text, Persona: utt.Persona})
		// A keyboard mute is the only thing that lands here mid-reply. Honor it
		// and stop speaking the rest.
		if c.State() == StateMuted {
			c.logger.Printf("playback abandoned mid-stream (muted)")
			drain(stream)
			return
		}
		c.setState(StateSpeaking)
		c.speak(ctx, utt.Persona, utt.Text)
	}

	if spoken == 0 {
		c.logger.Printf("responder produced nothing to say")
	}
	if c.State() == StateMuted {
		return
	}
	// Re-arm: the reply is finished, the speakers are quiet, and the microphone
	// comes back on for a follow-up that needs no wake word.
	c.setState(StateArmed)
}

// drain consumes the remainder of an abandoned stream so the producer is never
// left blocked on an unread channel.
func drain(ch <-chan Utterance) {
	go func() {
		for range ch {
		}
	}()
}

// speakAck says a short canned phrase, preferring the pre-rendered cache, then
// lands on resumeTo. resumeTo is explicit rather than read back from state
// because the whole point is to choose where the conversation goes next.
func (c *Client) speakAck(ctx context.Context, text, resumeTo string) {
	model := c.cfg.VoiceFor(PersonaOtto)
	wav := c.cache.Get(model, text)
	if wav == nil {
		// Synthesis happens before the state flips so a piper failure does not
		// leave the microphone shut for a phrase that is never spoken.
		sctx, cancel := context.WithTimeout(ctx, speakTimeout)
		var err error
		wav, err = c.tts.Speak(sctx, text, model)
		cancel()
		if err != nil {
			c.logger.Printf("ack tts failed: %v", err)
			c.setState(resumeTo)
			return
		}
		// Store for next time — acks recur constantly, so one live synthesis
		// per phrase per install is the most this should ever cost.
		if err := c.cache.Put(model, text, wav); err != nil {
			c.logger.Printf("ack cache write failed: %v", err)
		}
	}
	c.setState(StateSpeaking)
	if err := c.player.Play(ctx, wav); err != nil && ctx.Err() == nil {
		c.logger.Printf("ack playback failed: %v", err)
	}
	// A keyboard mute landing during the ack must stick.
	if c.State() == StateSpeaking {
		c.setState(resumeTo)
	}
}

// speak synthesizes and plays one utterance in the given persona's voice.
func (c *Client) speak(ctx context.Context, persona, text string) {
	clean := SanitizeForTTS(text)
	if clean == "" {
		return
	}
	model := c.cfg.VoiceFor(persona)

	sctx, cancel := context.WithTimeout(ctx, speakTimeout)
	wav, err := c.tts.Speak(sctx, clean, model)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.logger.Printf("tts failed: %v", err)
		c.emit(ErrorEvent{Err: fmt.Errorf("speak: %w", err)})
		return
	}
	if err := c.player.Play(ctx, wav); err != nil && ctx.Err() == nil {
		c.logger.Printf("playback failed: %v", err)
		c.emit(ErrorEvent{Err: fmt.Errorf("playback: %w", err)})
	}
}

// ─── The conversation timeout ────────────────────────────────────────────

// watchConversation closes an armed conversation nobody came back to.
//
// Without it, staying armed after a reply means every subsequent word spoken in
// the room — to a colleague, on a call — is transcribed and sent to the model as
// though it were addressed to Otto. The wake word exists precisely to prevent
// that, so the armed window has to be bounded.
func (c *Client) watchConversation(ctx context.Context) {
	timeout := c.cfg.ConversationTimeout()
	if timeout <= 0 {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.mu.Lock()
			expired := c.state == StateArmed && time.Since(c.lastActive) > timeout
			c.mu.Unlock()
			if expired {
				c.logger.Printf("conversation idle for %s → idle", timeout)
				c.setState(StateIdle)
			}
		}
	}
}

// noteActivity marks the conversation as alive, deferring the timeout.
func (c *Client) noteActivity() {
	c.mu.Lock()
	c.lastActive = time.Now()
	c.mu.Unlock()
}

// ─── Plumbing ────────────────────────────────────────────────────────────

// setState moves the machine and, with it, the microphone. Every transition
// goes through here, so micLive is the only rule deciding when Otto records.
func (c *Client) setState(s string) {
	c.mu.Lock()
	if c.state == s {
		c.mu.Unlock()
		return
	}
	prev := c.state
	c.state = s
	if s == StateArmed {
		// Entering armed restarts the abandonment clock, so the timeout is
		// measured from the end of Otto's reply rather than from the start of
		// the user's last sentence.
		c.lastActive = time.Now()
	}
	c.mu.Unlock()

	c.syncGate()
	c.logger.Printf("state: %s → %s (mic=%v)", prev, s, micLive(s))
	c.emit(StateEvent{State: s})
}

// syncGate brings the capture device in line with the current state. Called on
// every transition, and once by Start for the initial state.
func (c *Client) syncGate() {
	if micLive(c.State()) {
		c.gate.Open()
		return
	}
	c.gate.Close()
}

// emit publishes an event, dropping it if the buffer is full rather than
// blocking the audio pipeline — a stalled consumer must never stall capture.
func (c *Client) emit(e Event) {
	if c.closed.Load() {
		return
	}
	defer func() {
		// A sibling goroutine can observe closed==false and still race the
		// close. Recover so a late event cannot take down the process.
		_ = recover()
	}()
	select {
	case c.events <- e:
	default:
	}
}

func (c *Client) closeEvents() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.events)
	})
}
