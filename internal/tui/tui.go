// Package tui is Otto's terminal front end: a boot reveal, a spinning
// lightbulb over an audio-reactive bar cluster, and a chat pane that appears
// when you type.
//
// It is a *surface*, not a second Otto. Everything it does goes through the
// same handler the Telegram path uses — it submits text and renders replies,
// and knows nothing about sessions, memory, the bus or the model router.
package tui

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"otto/internal/artanim"
	"otto/internal/voice"
)

// Submitter accepts a typed message from the front end. Implemented by the
// surface multiplexer in cmd/otto.
type Submitter interface {
	Submit(userID int64, text string) bool
}

// VoiceController is the subset of the voice client the UI drives. An interface
// so the model is testable without a microphone.
type VoiceController interface {
	Events() <-chan voice.Event
	Mute()
	Unmute()
	IsMuted() bool
}

// Options configures the model.
type Options struct {
	// Submit receives typed messages. Nil disables typing.
	Submit Submitter
	// UserID is the allowlisted Telegram id, needed so submitted messages pass
	// the same auth gate as everything else.
	UserID int64
	// Voice drives the wake-word listener. Nil runs the UI keyboard-only.
	Voice VoiceController
	// Version is shown in the status line.
	Version string
}

// ─── Messages ────────────────────────────────────────────────────────────

type animTickMsg time.Time

// replyMsg carries a reply routed to this surface by the mux.
type replyMsg struct {
	text string
	html bool
}

type voiceEventMsg struct {
	evt    voice.Event
	closed bool
}

// ─── Model ───────────────────────────────────────────────────────────────

type uiMode int

const (
	// modeMinimal is the default: art and status only, no chat chrome. The
	// screen is meant to be glanceable from across a room.
	modeMinimal uiMode = iota
	// modeChat appears the moment you type a printable character.
	modeChat
)

const (
	// bootCharsPerTick controls how fast the OTTO reveal scans in.
	bootCharsPerTick = 5
	// bootPostRowInterval is ticks between status lines after the reveal.
	bootPostRowInterval = 18
	// bootHoldTicks is how long the finished boot holds before the UI appears.
	bootHoldTicks = 30
	// maxScrollback bounds the chat history kept in memory.
	maxScrollback = 200
)

// message is one line of chat scrollback.
type message struct {
	role string // "user" | "otto"
	text string
	at   time.Time
}

// Model is the Bubble Tea model.
type Model struct {
	opts Options

	viewport viewport.Model
	textarea textarea.Model
	messages []message

	width, height int
	ready         bool

	// Animation drivers.
	angle, wave   float64
	waveAmp       float64
	waveTargetAmp float64

	// Boot state.
	booting      bool
	bootProgress artanim.BootProgress
	bootHold     int

	mode uiMode

	// Voice state mirrored from the client's event stream.
	voiceState string
	voiceHeard string
	voiceReply string
	voiceErr   string

	// replies is fed by Deliver from the mux's goroutine.
	replies chan replyMsg
}

// New builds the model.
func New(opts Options) *Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message…"
	ta.Prompt = "│ "
	ta.ShowLineNumbers = false
	ta.SetHeight(3)
	ta.CharLimit = 0

	return &Model{
		opts:          opts,
		booting:       true,
		mode:          modeMinimal,
		waveTargetAmp: idleAmplitude,
		voiceState:    voice.StateOff,
		textarea:      ta,
		replies:       make(chan replyMsg, 64),
	}
}

// Deliver implements the mux's local surface. Called from Otto's goroutines, so
// it hands off through a channel rather than touching model state — Bubble Tea
// requires all state changes to happen inside Update.
//
// Drops rather than blocks when the buffer is full: the caller is Otto's reply
// path, and stalling it to wait on a UI would be exactly backwards.
func (m *Model) Deliver(ctx context.Context, text string, isHTML bool) {
	select {
	case m.replies <- replyMsg{text: text, html: isHTML}:
	default:
	}
}

// Init starts the animation loop and the event pumps.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{animTick(), waitReply(m.replies)}
	if m.opts.Voice != nil {
		cmds = append(cmds, waitVoiceEvent(m.opts.Voice.Events()))
	}
	return tea.Batch(cmds...)
}

func animTick() tea.Cmd {
	// ~30fps. The lightbulb is re-rasterized every frame, which at terminal
	// resolution is a few thousand plots — cheap enough that the frame rate is
	// chosen for how the spin looks rather than for cost.
	return tea.Tick(33*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}

func waitReply(ch <-chan replyMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func waitVoiceEvent(ch <-chan voice.Event) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return voiceEventMsg{closed: true}
		}
		return voiceEventMsg{evt: evt}
	}
}

// ─── Update ──────────────────────────────────────────────────────────────

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case animTickMsg:
		m.angle += 0.05
		m.wave += 0.12
		// Ease toward the target so state changes feel sprung rather than
		// stepped.
		m.waveAmp += (m.waveTargetAmp - m.waveAmp) * 0.18
		if m.booting {
			m.advanceBoot()
		}
		return m, animTick()

	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case replyMsg:
		m.appendMessage("otto", cleanForDisplay(msg.text, msg.html))
		return m, waitReply(m.replies)

	case voiceEventMsg:
		if msg.closed {
			m.voiceState = voice.StateOff
			break
		}
		m.applyVoiceEvent(msg.evt)
		cmds = append(cmds, waitVoiceEvent(m.opts.Voice.Events()))
	}

	if m.ready && !m.booting && m.mode == modeChat {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.booting {
		// Any key skips the boot animation. Nobody wants to watch it twice.
		m.booting = false
		return m, m.textarea.Focus()
	}

	if m.mode == modeMinimal {
		switch {
		case key == "m" || key == "M":
			m.toggleMute()
			return m, nil
		case key == "enter":
			m.mode = modeChat
			return m, m.textarea.Focus()
		case len(key) == 1 && key >= " " && key <= "~":
			// Typing anything opens chat with that character already in the
			// box, so there is no "press enter first" step.
			m.mode = modeChat
			m.textarea.SetValue(key)
			m.textarea.CursorEnd()
			return m, m.textarea.Focus()
		}
		return m, nil
	}

	// Chat mode. 'm' on an empty box is the mute toggle; with text present it
	// types normally, so messages can use the letter freely.
	if (key == "m" || key == "M") && strings.TrimSpace(m.textarea.Value()) == "" {
		m.toggleMute()
		return m, nil
	}

	switch key {
	case "esc":
		// Keep whatever is half-typed; only the chrome goes away.
		m.mode = modeMinimal
		m.textarea.Blur()
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.textarea.Value())
		if val == "" {
			m.mode = modeMinimal
			m.textarea.Blur()
			return m, nil
		}
		m.textarea.Reset()
		m.submit(val)
		return m, nil
	}
	return m, nil
}

func (m *Model) submit(text string) {
	if m.opts.Submit == nil {
		m.voiceErr = "typing is not wired to Otto"
		return
	}
	if !m.opts.Submit.Submit(m.opts.UserID, text) {
		m.voiceErr = "Otto's queue is full — try again in a moment"
		return
	}
	m.appendMessage("user", text)
}

func (m *Model) applyVoiceEvent(evt voice.Event) {
	switch e := evt.(type) {
	case voice.LevelEvent:
		m.waveTargetAmp = amplitudeFor(m.voiceState, e.RMS)

	case voice.StateEvent:
		m.voiceState = e.State
		if e.State == voice.StateIdle {
			m.voiceHeard = ""
		}
		// Clear a stale error only on a meaningful forward transition. Clearing
		// on every idle reset would make errors flash past unread.
		if e.State == voice.StateArmed || e.State == voice.StateSpeaking {
			m.voiceErr = ""
		}

	case voice.TranscriptEvent:
		m.voiceHeard = e.Text
		m.voiceErr = ""
		// A bare wake word carries no text; there is nothing to log.
		if e.Text != "" {
			m.appendMessage("user", e.Text)
		}

	case voice.ReplyEvent:
		m.voiceReply = e.ReplyText
		m.voiceErr = ""
		// Spoken replies arrive here sentence by sentence, and the mux
		// separately delivers the full text through Deliver. Rendering both
		// would double every line, so the scrollback is left to Deliver and
		// this only drives the status line.

	case voice.ErrorEvent:
		m.voiceErr = e.Err.Error()
	}
}

func (m *Model) toggleMute() {
	if m.opts.Voice == nil {
		return
	}
	if m.opts.Voice.IsMuted() {
		m.opts.Voice.Unmute()
		return
	}
	m.opts.Voice.Mute()
}

func (m *Model) appendMessage(role, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	m.messages = append(m.messages, message{role: role, text: text, at: time.Now()})
	if len(m.messages) > maxScrollback {
		// Re-slice into a fresh array so the trimmed prefix can be collected.
		m.messages = append([]message(nil), m.messages[len(m.messages)-maxScrollback:]...)
	}
	if m.ready {
		m.viewport.SetContent(m.renderMessages())
		m.viewport.GotoBottom()
	}
}

func (m *Model) resize(w, h int) {
	m.width, m.height = w, h
	vpHeight := h - chatHeaderRows - chatInputRows
	if vpHeight < 1 {
		vpHeight = 1
	}
	if !m.ready {
		m.viewport = viewport.New(viewport.WithWidth(w), viewport.WithHeight(vpHeight))
		m.ready = true
	} else {
		m.viewport.SetWidth(w)
		m.viewport.SetHeight(vpHeight)
	}
	m.textarea.SetWidth(w)
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()
}

func (m *Model) advanceBoot() {
	const artRows, artCols = 10, 53 // must match artanim.ottoFont
	if m.bootProgress.Rows < artRows {
		m.bootProgress.SubRow += bootCharsPerTick
		if m.bootProgress.SubRow >= artCols {
			m.bootProgress.SubRow = 0
			m.bootProgress.Rows++
		}
		return
	}
	m.bootHold++
	m.bootProgress.Blink = (m.bootHold/8)%2 == 0
	if m.bootProgress.PostRows < 4 && m.bootHold%bootPostRowInterval == 0 {
		m.bootProgress.PostRows++
	}
	if m.bootHold >= bootHoldTicks && m.bootProgress.PostRows >= 4 {
		m.booting = false
	}
}

// ─── View ────────────────────────────────────────────────────────────────

const (
	chatHeaderRows = 2
	chatInputRows  = 6
)

func (m *Model) View() tea.View {
	var content string
	switch {
	case m.width == 0 || m.height == 0:
		content = ""
	case m.booting:
		content = artanim.RenderBoot(m.width, m.height, m.bootProgress)
	case m.mode == modeChat:
		content = m.renderChat()
	default:
		content = m.renderMinimal()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// renderMinimal is the default view: lightbulb, bars, one status line.
func (m *Model) renderMinimal() string {
	if m.width < 20 || m.height < 10 {
		return statusStyle.Render(m.statusLine())
	}

	artH := int(float64(m.height) * 0.62)
	artH = clampInt(artH, 12, 34)
	scale := scaleForHeight(artH)

	barsH := 4
	if m.height < 30 {
		barsH = 3
	}

	art := artanim.RenderLightbulbScaled(m.width, artH, m.angle, scale)
	bars := artanim.RenderSiriBars(m.width, barsH, m.waveAmp, m.wave)
	status := m.statusLine()
	hint := "type to chat  ·  m to mute  ·  ctrl+c to quit"

	var b strings.Builder
	b.WriteString(strings.Repeat("\n", maxInt(0, int(float64(m.height)*0.05))))
	b.WriteString(art)
	if !strings.HasSuffix(art, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(bars)
	b.WriteString("\n\n")
	b.WriteString(centerPlain(statusStyle.Render(status), status, m.width))
	b.WriteByte('\n')

	// Pin the hint to the bottom.
	used := strings.Count(b.String(), "\n") + 1
	b.WriteString(strings.Repeat("\n", maxInt(0, m.height-used-1)))
	b.WriteString(centerPlain(helpStyle.Render(hint), hint, m.width))
	return b.String()
}

func (m *Model) renderChat() string {
	var b strings.Builder
	b.WriteString(m.headerLine())
	b.WriteString("\n\n")
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	b.WriteString(m.textarea.View())
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("enter: send  ·  esc: back  ·  m (empty): mute  ·  ctrl+c: quit"))
	return b.String()
}

func (m *Model) headerLine() string {
	left := ottoLabelStyle.Render("OTTO")
	right := statusStyle.Render(m.statusLine())
	gap := m.width - 4 - lenPlain(m.statusLine())
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// statusLine is the single line of truth about what the voice loop is doing.
// An error outranks everything else so failures are actually seen.
func (m *Model) statusLine() string {
	if m.voiceErr != "" {
		return "voice error: " + truncate(m.voiceErr, 70)
	}
	switch m.voiceState {
	case voice.StateInstalling:
		return "setting up voice… (one-time download)"
	case voice.StateIdle:
		return `listening for "otto"…`
	case voice.StateArmed:
		if m.voiceHeard != "" {
			return "heard: " + truncate(m.voiceHeard, 60)
		}
		return "go ahead, I'm listening"
	case voice.StateProcessing:
		return "thinking…"
	case voice.StateSpeaking:
		if m.voiceReply != "" {
			return "speaking: " + truncate(m.voiceReply, 55)
		}
		return "speaking…"
	case voice.StateMuted:
		return `muted — say "otto wake up", or press m`
	default:
		if m.opts.Voice == nil {
			return "voice off — type to talk to Otto"
		}
		return "voice unavailable — run otto voice-doctor"
	}
}

func (m *Model) renderMessages() string {
	var b strings.Builder
	width := maxInt(20, m.width-2)
	for _, msg := range m.messages {
		if msg.role == "user" {
			b.WriteString(userLabelStyle.Render("you"))
			b.WriteString("\n")
			b.WriteString(userStyle.Render(wordWrap(msg.text, width)))
		} else {
			b.WriteString(ottoLabelStyle.Render("otto"))
			b.WriteString("\n")
			b.WriteString(ottoStyle.Render(wordWrap(msg.text, width)))
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// ─── Amplitude ───────────────────────────────────────────────────────────

// idleAmplitude keeps the bars almost flat at rest.
//
// Mic level is deliberately ignored while idle. Driving the bars from ambient
// noise makes Otto look like he is constantly hearing you, which is both untrue
// (nothing happens until the wake word lands) and unsettling.
const idleAmplitude = 0.08

func amplitudeFor(state string, rms float64) float64 {
	var target float64
	switch state {
	case voice.StateArmed, voice.StateProcessing:
		target = 0.4 + 4.0*rms
	case voice.StateSpeaking:
		target = 0.7 + 3.0*rms
	default:
		target = idleAmplitude
	}
	if target > 1 {
		return 1
	}
	return target
}

func scaleForHeight(h int) float64 {
	switch {
	case h >= 30:
		return 1.3
	case h >= 24:
		return 1.05
	case h >= 18:
		return 0.8
	case h >= 14:
		return 0.6
	default:
		return 0.45
	}
}

// ─── Text helpers ────────────────────────────────────────────────────────

var (
	htmlTag = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)
	ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// cleanForDisplay prepares a reply for the terminal. The pets send their ASCII
// art as HTML (<pre> blocks for Telegram's monospace rendering), which would
// otherwise print as literal tags.
func cleanForDisplay(text string, isHTML bool) string {
	if !isHTML {
		return text
	}
	out := htmlTag.ReplaceAllString(text, "")
	return strings.TrimSpace(html.UnescapeString(out))
}

func wordWrap(text string, width int) string {
	if width <= 0 {
		return text
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		var cur string
		for _, word := range strings.Fields(para) {
			for _, chunk := range hardBreak(word, width) {
				switch {
				case cur == "":
					cur = chunk
				case lenPlain(cur)+1+lenPlain(chunk) > width:
					out = append(out, cur)
					cur = chunk
				default:
					cur += " " + chunk
				}
			}
		}
		if cur != "" {
			out = append(out, cur)
		}
	}
	return strings.Join(out, "\n")
}

// hardBreak splits a word longer than width so nothing is clipped.
func hardBreak(w string, width int) []string {
	if lenPlain(w) <= width {
		return []string{w}
	}
	var chunks []string
	runes := []rune(w)
	for len(runes) > width {
		chunks = append(chunks, string(runes[:width]))
		runes = runes[width:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

// centerPlain pads styled text using the *plain* string's width, since ANSI
// escapes have no display width.
func centerPlain(styled, plain string, width int) string {
	pad := (width - lenPlain(plain)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + styled
}

func lenPlain(s string) int {
	n := 0
	for range ansiSeq.ReplaceAllString(s, "") {
		n++
	}
	return n
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf // retained for future status formatting
