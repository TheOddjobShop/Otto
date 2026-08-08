package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"otto/internal/voice"
)

// ─── Stubs ───────────────────────────────────────────────────────────────

type fakeSubmitter struct {
	mu        sync.Mutex
	got       []string
	userIDs   []int64
	rejectAll bool
}

func (f *fakeSubmitter) Submit(userID int64, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectAll {
		return false
	}
	f.got = append(f.got, text)
	f.userIDs = append(f.userIDs, userID)
	return true
}

func (f *fakeSubmitter) submitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

type fakeVoice struct {
	events chan voice.Event
	muted  bool
}

func newFakeVoice() *fakeVoice {
	return &fakeVoice{events: make(chan voice.Event, 8)}
}

func (f *fakeVoice) Events() <-chan voice.Event { return f.events }
func (f *fakeVoice) Mute()                      { f.muted = true }
func (f *fakeVoice) Unmute()                    { f.muted = false }
func (f *fakeVoice) IsMuted() bool              { return f.muted }

func newTestModel(sub Submitter, vc VoiceController) *Model {
	m := New(Options{Submit: sub, UserID: 42, Voice: vc, Version: "test"})
	m.booting = false // skip the animation; it is tested separately
	m.resize(100, 40)
	return m
}

func key(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{}
}

// ─── Input ───────────────────────────────────────────────────────────────

// Typing any printable character should open chat with that character already
// in the box — no "press enter first" step.
func TestTypingOpensChatWithCharacter(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	if m.mode != modeMinimal {
		t.Fatal("should start minimal")
	}
	m.Update(key("h"))
	if m.mode != modeChat {
		t.Fatal("typing should open chat")
	}
	if got := m.textarea.Value(); got != "h" {
		t.Errorf("textarea = %q, want the typed character preserved", got)
	}
}

func TestEscReturnsToMinimalKeepingDraft(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	m.Update(key("h"))
	m.textarea.SetValue("half a thought")
	m.Update(key("esc"))
	if m.mode != modeMinimal {
		t.Error("esc should return to minimal")
	}
	if got := m.textarea.Value(); got != "half a thought" {
		t.Errorf("draft = %q, want it preserved so nothing is lost", got)
	}
}

func TestEnterSubmitsAndEchoes(t *testing.T) {
	sub := &fakeSubmitter{}
	m := newTestModel(sub, nil)
	m.mode = modeChat
	m.textarea.SetValue("what's on my calendar")
	m.Update(key("enter"))

	if got := sub.submitted(); len(got) != 1 || got[0] != "what's on my calendar" {
		t.Fatalf("submitted %v, want the typed message", got)
	}
	if len(sub.userIDs) != 1 || sub.userIDs[0] != 42 {
		t.Errorf("submitted with user %v, want the allowlisted id so it passes the auth gate", sub.userIDs)
	}
	if len(m.messages) != 1 || m.messages[0].role != "user" {
		t.Errorf("scrollback = %v, want the message echoed locally", m.messages)
	}
	if m.textarea.Value() != "" {
		t.Error("the input should be cleared after sending")
	}
}

func TestEnterOnEmptyInputReturnsToMinimal(t *testing.T) {
	sub := &fakeSubmitter{}
	m := newTestModel(sub, nil)
	m.mode = modeChat
	m.Update(key("enter"))
	if m.mode != modeMinimal {
		t.Error("enter on an empty box should collapse back to minimal")
	}
	if len(sub.submitted()) != 0 {
		t.Error("an empty message must not be sent")
	}
}

func TestRejectedSubmitSurfacesToUser(t *testing.T) {
	m := newTestModel(&fakeSubmitter{rejectAll: true}, nil)
	m.mode = modeChat
	m.textarea.SetValue("hello")
	m.Update(key("enter"))
	if m.voiceErr == "" {
		t.Error("a refused submit must be surfaced, not silently dropped")
	}
	if len(m.messages) != 0 {
		t.Error("a message that was not accepted must not appear in the scrollback")
	}
}

// ─── Mute ────────────────────────────────────────────────────────────────

func TestMuteToggleFromMinimal(t *testing.T) {
	vc := newFakeVoice()
	m := newTestModel(&fakeSubmitter{}, vc)
	m.Update(key("m"))
	if !vc.IsMuted() {
		t.Error("m should mute from minimal mode")
	}
	m.Update(key("m"))
	if vc.IsMuted() {
		t.Error("m again should unmute")
	}
}

// 'm' must only be the mute shortcut on an empty input, or the letter becomes
// untypeable.
func TestMInChatTypesWhenInputHasText(t *testing.T) {
	vc := newFakeVoice()
	m := newTestModel(&fakeSubmitter{}, vc)
	m.mode = modeChat
	m.textarea.SetValue("hm")
	m.Update(key("m"))
	if vc.IsMuted() {
		t.Error("m with text present must type, not mute")
	}
}

func TestMuteToggleIsSafeWithoutVoice(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	m.Update(key("m")) // must not panic
}

// ─── Voice events ────────────────────────────────────────────────────────

func TestVoiceStateDrivesStatusLine(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, newFakeVoice())
	tests := []struct {
		state string
		want  string
	}{
		{voice.StateIdle, "listening"},
		{voice.StateArmed, "go ahead"},
		{voice.StateProcessing, "thinking"},
		{voice.StateSpeaking, "speaking"},
		{voice.StateMuted, "muted"},
		{voice.StateInstalling, "setting up"},
	}
	for _, tc := range tests {
		m.applyVoiceEvent(voice.StateEvent{State: tc.state})
		if got := strings.ToLower(m.statusLine()); !strings.Contains(got, tc.want) {
			t.Errorf("state %q → %q, want it to mention %q", tc.state, got, tc.want)
		}
	}
}

// An error must outrank the state, or failures scroll past unnoticed.
func TestErrorOutranksStatus(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, newFakeVoice())
	m.applyVoiceEvent(voice.StateEvent{State: voice.StateIdle})
	m.applyVoiceEvent(voice.ErrorEvent{Err: context.DeadlineExceeded})
	if !strings.Contains(m.statusLine(), "voice error") {
		t.Errorf("status = %q, want the error to take priority", m.statusLine())
	}
	// Only a meaningful forward transition clears it — an idle reset would make
	// the error flash past unread.
	m.applyVoiceEvent(voice.StateEvent{State: voice.StateIdle})
	if !strings.Contains(m.statusLine(), "voice error") {
		t.Error("an idle reset should not clear a pending error")
	}
	m.applyVoiceEvent(voice.StateEvent{State: voice.StateArmed})
	if strings.Contains(m.statusLine(), "voice error") {
		t.Error("arming should clear the error")
	}
}

func TestTranscriptAppearsInScrollback(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, newFakeVoice())
	m.applyVoiceEvent(voice.TranscriptEvent{Text: "what's on my calendar"})
	if len(m.messages) != 1 || m.messages[0].text != "what's on my calendar" {
		t.Errorf("scrollback = %v, want the spoken turn logged alongside typed ones", m.messages)
	}
}

// A bare wake word carries no text; logging an empty line would be noise.
func TestBareWakeWordIsNotLogged(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, newFakeVoice())
	m.applyVoiceEvent(voice.TranscriptEvent{Text: ""})
	if len(m.messages) != 0 {
		t.Errorf("scrollback = %v, want nothing logged for a bare wake word", m.messages)
	}
}

// Otto's reply reaches the screen via Deliver; ReplyEvent only drives status.
// Rendering both would double every line.
func TestReplyEventDoesNotDuplicateScrollback(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, newFakeVoice())
	// The real sequence: the client emits ReplyEvent and then transitions to
	// speaking, which is when the line becomes visible.
	m.applyVoiceEvent(voice.ReplyEvent{UserText: "hi", ReplyText: "All good."})
	m.applyVoiceEvent(voice.StateEvent{State: voice.StateSpeaking})
	if len(m.messages) != 0 {
		t.Errorf("scrollback = %v, want ReplyEvent to drive status only", m.messages)
	}
	if !strings.Contains(m.statusLine(), "All good") {
		t.Errorf("status = %q, want the spoken line surfaced", m.statusLine())
	}
}

// Ambient mic level must not drive the bars while idle — it would look like
// Otto is always hearing you, which is both untrue and unsettling.
func TestAmplitudeIgnoresMicWhenIdle(t *testing.T) {
	if got := amplitudeFor(voice.StateIdle, 0.9); got != idleAmplitude {
		t.Errorf("idle amplitude = %v with a loud room, want the flat %v", got, idleAmplitude)
	}
	if got := amplitudeFor(voice.StateArmed, 0.1); got <= idleAmplitude {
		t.Errorf("armed amplitude = %v, want the bars to respond once listening", got)
	}
	if got := amplitudeFor(voice.StateSpeaking, 1.0); got > 1 {
		t.Errorf("amplitude = %v, want it clamped to 1", got)
	}
}

// ─── Delivery ────────────────────────────────────────────────────────────

func TestDeliverStripsHTMLFromPetReplies(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	m.Deliver(context.Background(), "<blockquote><b>TOTO</b></blockquote>\n<pre>art</pre>\n\nmrrp.", true)

	// Deliver hands off through a channel; drain it as Update would.
	msg := <-m.replies
	m.Update(msg)

	if len(m.messages) != 1 {
		t.Fatalf("scrollback = %v, want the reply rendered", m.messages)
	}
	got := m.messages[0].text
	if strings.Contains(got, "<") {
		t.Errorf("rendered %q; HTML tags must not print literally", got)
	}
	if !strings.Contains(got, "mrrp") {
		t.Errorf("rendered %q, want the cat's actual words", got)
	}
}

// Deliver is called from Otto's reply path; stalling it to wait on a UI would
// be exactly backwards.
func TestDeliverNeverBlocks(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			m.Deliver(context.Background(), "flood", false)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-timeoutAfterSeconds(2):
		t.Fatal("Deliver blocked when its buffer filled")
	}
}

// ─── Rendering ───────────────────────────────────────────────────────────

func TestViewRendersInEveryMode(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, newFakeVoice())

	m.booting = true
	if strings.TrimSpace(viewString(m.View())) == "" {
		t.Error("boot view rendered nothing")
	}
	m.booting = false
	if strings.TrimSpace(viewString(m.View())) == "" {
		t.Error("minimal view rendered nothing")
	}
	m.mode = modeChat
	if strings.TrimSpace(viewString(m.View())) == "" {
		t.Error("chat view rendered nothing")
	}
}

// A terminal too small for the art must degrade rather than panic.
func TestTinyTerminalDoesNotPanic(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	m.resize(10, 5)
	_ = m.View()
	m.resize(1, 1)
	_ = m.View()
}

func TestWordWrap(t *testing.T) {
	got := wordWrap("the quick brown fox jumps", 10)
	for _, line := range strings.Split(got, "\n") {
		if len([]rune(line)) > 10 {
			t.Errorf("line %q exceeds the wrap width", line)
		}
	}
	// Paragraph breaks survive.
	if got := wordWrap("one\n\ntwo", 40); !strings.Contains(got, "\n\n") {
		t.Errorf("wordWrap collapsed a paragraph break: %q", got)
	}
	// An over-long token is hard-broken rather than clipped.
	long := strings.Repeat("x", 25)
	if got := wordWrap(long, 10); len(strings.Split(got, "\n")) != 3 {
		t.Errorf("wordWrap(%d chars, width 10) = %q, want it broken across lines", len(long), got)
	}
}

func TestCleanForDisplay(t *testing.T) {
	if got := cleanForDisplay("plain text", false); got != "plain text" {
		t.Errorf("non-HTML should pass through unchanged, got %q", got)
	}
	got := cleanForDisplay("<b>bold</b> &amp; escaped", true)
	if strings.Contains(got, "<") || strings.Contains(got, "&amp;") {
		t.Errorf("cleanForDisplay = %q, want tags stripped and entities decoded", got)
	}
}

func TestStatusLineWithoutVoice(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	if got := m.statusLine(); !strings.Contains(got, "type") {
		t.Errorf("status = %q, want it to say typing still works when voice is off", got)
	}
}

func TestScrollbackIsBounded(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	for i := 0; i < maxScrollback+50; i++ {
		m.appendMessage("user", "line")
	}
	if len(m.messages) > maxScrollback {
		t.Errorf("scrollback grew to %d, want it capped at %d", len(m.messages), maxScrollback)
	}
}

func TestAppendIgnoresBlankMessages(t *testing.T) {
	m := newTestModel(&fakeSubmitter{}, nil)
	m.appendMessage("otto", "   ")
	if len(m.messages) != 0 {
		t.Error("blank replies should not clutter the scrollback")
	}
}

func TestBootAdvancesToCompletion(t *testing.T) {
	m := New(Options{})
	m.resize(100, 40)
	for i := 0; i < 5000 && m.booting; i++ {
		m.advanceBoot()
	}
	if m.booting {
		t.Fatal("boot animation never completed")
	}
	if m.bootProgress.PostRows < 4 {
		t.Errorf("PostRows = %d, want all status lines revealed", m.bootProgress.PostRows)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────

func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

// viewString extracts the rendered text from a tea.View for assertions.
func viewString(v tea.View) string { return fmt.Sprint(v) }
