package voice

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// The listening loop: mic → frames → utterances → transcript → Otto → speech.
//
// State machine:
//
//	idle ──wake word──► armed ──utterance──► processing ──reply──► speaking ──► armed
//	  ▲                   │                                            │
//	  └────── closer ─────┴──────────────── closer ────────────────────┘
//
//	muted is an overlay reachable from any state; only a wake command leaves it.

// ─── Events ──────────────────────────────────────────────────────────────

// Event is anything the client emits. Consumers type-switch on it.
type Event interface{ voiceEvent() }

// LevelEvent carries the current mic RMS in [0,1], roughly ten times a second.
type LevelEvent struct{ RMS float64 }

// StateEvent fires on every state transition.
type StateEvent struct{ State string }

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

	// endSilenceMsIdle is the trailing silence that ends an utterance while
	// idle. Generous, so a pause mid-sentence ("hey otto, what's…") does not
	// split one request into two.
	endSilenceMsIdle = 750
	// endSilenceMsActive applies once armed or speaking, where commands are
	// short by design and snappiness matters more than tolerating pauses.
	endSilenceMsActive = 400

	// preRollMs of audio before speech onset is prepended to each utterance, so
	// the first syllable is not clipped.
	preRollMs = 300

	// noiseFloorGain multiplies the adapted noise floor to get the speech
	// threshold; baseFloor stops a silent room from adapting the floor to zero
	// and making every rustle count as speech.
	noiseFloorGain = 2.8
	baseFloor      = 0.02

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

// PlaybackDevice plays rendered audio and can be interrupted mid-utterance.
// An interface so the speaking path is testable without a sound card — the
// state machine's barge-in logic is the most intricate part of this package and
// the least amenable to being checked by hand.
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
	cache     *Cache
	responder Responder
	logger    *log.Logger

	events    chan Event
	closeOnce sync.Once
	closed    atomic.Bool

	mu    sync.Mutex
	state string
}

// ClientOptions configures a Client. stt, tts and responder are required.
type ClientOptions struct {
	Config    Config
	STT       Transcriber
	TTS       Speaker
	Responder Responder
	// Logger receives the diagnostic trail. Voice failures are notoriously
	// hard to reason about after the fact ("it just didn't hear me"), so every
	// utterance, transcript, wake decision and state change is logged.
	Logger *log.Logger
	// Player overrides the audio output device. Nil uses the real one.
	Player PlaybackDevice
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
	return &Client{
		cfg:       opts.Config,
		stt:       opts.STT,
		tts:       opts.TTS,
		player:    player,
		cache:     NewCache(opts.Config.Dir),
		responder: opts.Responder,
		logger:    logger,
		events:    make(chan Event, 64),
		state:     StateIdle,
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

// Mute silences Otto immediately, killing any in-flight playback. Idempotent.
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

// Start captures from the microphone until ctx is cancelled. A returned error
// means the capture pipeline itself failed; recoverable problems arrive as
// ErrorEvent instead.
func (c *Client) Start(ctx context.Context) error {
	defer c.closeEvents()

	if _, err := exec.LookPath("sox"); err != nil {
		c.setState(StateOff)
		err = fmt.Errorf("sox not installed — run ./setup.sh, or `otto voice-doctor` for details")
		c.emit(ErrorEvent{Err: err})
		return err
	}

	// sox reads the default capture device and writes raw signed 16-bit
	// little-endian mono PCM at 16 kHz to stdout.
	cmd := exec.CommandContext(ctx, "sox",
		"-q", "-d",
		"-c", "1",
		"-r", fmt.Sprint(sampleRate),
		"-b", "16",
		"-e", "signed-integer",
		"-L",
		"-t", "raw",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		c.setState(StateOff)
		err = fmt.Errorf("start sox: %w", err)
		c.emit(ErrorEvent{Err: err})
		return err
	}
	c.logger.Printf("capture started (wake=%q)", c.cfg.Wake())

	frames := make(chan []int16, 32)
	utterances := make(chan capturedUtterance, 4)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.readFrames(ctx, stdout, frames) }()
	go func() { defer wg.Done(); c.detectUtterances(ctx, frames, utterances) }()
	go func() { defer wg.Done(); c.handleUtterances(ctx, utterances) }()

	c.setState(StateIdle)
	<-ctx.Done()

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
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

// ─── Stage 1: PCM reader ─────────────────────────────────────────────────

func (c *Client) readFrames(ctx context.Context, r io.Reader, out chan<- []int16) {
	defer close(out)
	buf := make([]byte, frameSamples*2)
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		frame := make([]int16, frameSamples)
		for i := range frame {
			frame[i] = int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
		}
		c.emit(LevelEvent{RMS: rms(frame)})
		select {
		case out <- frame:
		case <-ctx.Done():
			return
		}
	}
}

// ─── Stage 2: VAD / utterance assembly ───────────────────────────────────

// capturedUtterance bundles audio with the state speech *started* in.
//
// Using the start state rather than the current one is what stops Otto's own
// playback from being treated as a follow-up question: an utterance that began
// during playback is loopback no matter how long the silence detector took to
// flush it, and by then the state may well have moved on.
type capturedUtterance struct {
	samples    []int16
	startState string
}

func (c *Client) detectUtterances(ctx context.Context, in <-chan []int16, out chan<- capturedUtterance) {
	defer close(out)

	ring := newFrameRing(preRollMs / 100)
	var speech []int16
	var startState string
	noiseFloor := baseFloor
	speechFrames, silenceFrames := 0, 0
	inSpeech := false

	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return
			}
			state := c.State()

			// While a reply is being computed there is no audio in flight and
			// nothing to barge into, so pause detection and just keep the ring
			// warm for the next utterance.
			if state == StateProcessing {
				ring.push(frame)
				continue
			}

			level := rms(frame)
			threshold := max(baseFloor, noiseFloor*noiseFloorGain)

			if level > threshold {
				if !inSpeech {
					speech = speech[:0]
					startState = state
					if state != StateSpeaking {
						for _, f := range ring.drain() {
							speech = append(speech, f...)
						}
					} else {
						// Mid-playback the ring holds Otto's own voice off the
						// speakers; prepending it would feed loopback into the
						// transcript.
						ring.reset()
					}
					inSpeech = true
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

			endMs := endSilenceMsIdle
			if state == StateArmed || state == StateSpeaking {
				endMs = endSilenceMsActive
			}
			if silenceFrames < endMs/100 {
				continue
			}

			minMs := minSpeechMsIdle
			if startState == StateArmed || startState == StateSpeaking {
				minMs = minSpeechMsArmed
			}
			minFrames := max(1, minMs/100)

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
			speech = speech[:0]
			speechFrames, silenceFrames = 0, 0
			inSpeech = false
		}
	}
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
			// Show "processing" while transcribing, but only from states where
			// that is a truthful thing to display. Muted must stay muted —
			// transcription there exists solely to catch "otto wake up", and
			// flipping to processing would both lie to the UI and, on the way
			// back out, silently un-mute. Speaking must stay speaking so the
			// barge-in branch sees the state it reasons about.
			if prior == StateIdle || prior == StateArmed {
				c.setState(StateProcessing)
			}
			c.processUtterance(ctx, utt.samples, prior)
			// Nothing advanced the state, so return to where we came from. An
			// unrecognized noise mid-conversation must leave the conversation
			// open rather than quietly dropping back to idle.
			if c.State() == StateProcessing {
				if prior == StateArmed {
					c.setState(StateArmed)
				} else {
					c.setState(StateIdle)
				}
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
			c.setState(StateIdle)
			c.speakAck(ctx, PickUnmuteAck(), StateIdle)
			return
		}
		c.logger.Printf("muted: ignoring")
		return

	case StateSpeaking:
		// Otto's own voice reaches the mic, and he says his own name in
		// replies, so treating every wake-word hit as barge-in produces
		// constant self-interruption. Only an explicit stop gets through;
		// anything else waits and is heard as a normal follow-up once he
		// finishes.
		if AnyMatches(variants, IsMuteCommand) {
			c.logger.Printf("mute during playback → muted")
			c.player.Interrupt()
			c.emit(TranscriptEvent{Text: command, Raw: text})
			c.setState(StateMuted)
			return
		}
		if AnyMatches(variants, IsCloserCommand) {
			c.logger.Printf("closer during playback → idle")
			c.player.Interrupt()
			c.emit(TranscriptEvent{Text: command, Raw: text})
			c.setState(StateIdle)
			c.speakAck(ctx, PickCloserAck(), StateIdle)
			return
		}
		c.logger.Printf("playback continues (not a barge-in phrase)")
		return

	case StateArmed:
		// Mid-conversation: closers and mutes need no wake word, since the user
		// is already talking to Otto.
		if AnyMatches(variants, IsCloserCommand) {
			c.logger.Printf("closer → idle")
			c.emit(TranscriptEvent{Text: command, Raw: text})
			c.setState(StateIdle)
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
	if command == "" {
		// Wake word alone: acknowledge and wait for the request.
		c.logger.Printf("bare wake word → armed")
		c.emit(TranscriptEvent{Text: "", Raw: text})
		c.setState(StateArmed)
		c.speakAck(ctx, PickGreeting(), StateArmed)
		return
	}
	// Wake word and request in one breath.
	c.emit(TranscriptEvent{Text: command, Raw: text})
	c.respond(ctx, command)
}

// respond hands the transcript to Otto and speaks each sentence as it arrives.
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

	var spoken []string
	first := true
	for utt := range stream {
		if utt.Text == "" {
			continue
		}
		if first {
			c.logger.Printf("first sentence after %s", time.Since(start))
			first = false
		}
		spoken = append(spoken, utt.Text)
		c.emit(ReplyEvent{UserText: userText, ReplyText: utt.Text, Persona: utt.Persona})
		// A barge-in during playback moves us to muted or idle. Stop consuming
		// so the rest of the reply is not spoken over the user's objection.
		// Processing (the first iteration) and speaking both mean "carry on".
		if s := c.State(); s == StateMuted || s == StateIdle {
			c.logger.Printf("playback abandoned mid-stream (state=%s)", s)
			drain(stream)
			return
		}
		c.setState(StateSpeaking)
		c.speak(ctx, utt.Persona, utt.Text)
	}

	if len(spoken) == 0 {
		c.logger.Printf("responder produced nothing to say")
	}
	// Only advance from speaking: if a barge-in already moved us to muted or
	// idle, honoring that matters more than re-arming.
	if c.State() == StateSpeaking {
		c.setState(StateArmed)
	}
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
// returns to resumeTo. resumeTo is explicit rather than read back from state to
// avoid racing a concurrent transition.
func (c *Client) speakAck(ctx context.Context, text, resumeTo string) {
	model := c.cfg.VoiceFor(PersonaOtto)
	wav := c.cache.Get(model, text)
	if wav == nil {
		sctx, cancel := context.WithTimeout(ctx, speakTimeout)
		var err error
		wav, err = c.tts.Speak(sctx, text, model)
		cancel()
		if err != nil {
			c.logger.Printf("ack tts failed: %v", err)
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
	// A mute landing during the ack must stick.
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

// ─── Plumbing ────────────────────────────────────────────────────────────

func (c *Client) setState(s string) {
	c.mu.Lock()
	if c.state == s {
		c.mu.Unlock()
		return
	}
	prev := c.state
	c.state = s
	c.mu.Unlock()
	c.logger.Printf("state: %s → %s", prev, s)
	c.emit(StateEvent{State: s})
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
