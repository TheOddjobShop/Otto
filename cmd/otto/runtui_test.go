//go:build unix

package main

import (
	"testing"

	"otto/internal/config"
	"otto/internal/voice"
)

// An absent key must leave the package default alone. Writing a zero through
// would be silent and total: an empty wake word matches nothing, so Otto would
// simply never respond and no error would say why.
func TestVoiceOverridesLeaveUnsetKeysAlone(t *testing.T) {
	base := voice.DefaultConfig(t.TempDir())
	got := applyVoiceOverrides(base, &config.Config{})

	if got.Wake() != base.Wake() {
		t.Errorf("wake word = %q, want the default %q", got.Wake(), base.Wake())
	}
	if got.RequestEndSilence() != base.RequestEndSilence() {
		t.Errorf("end silence = %s, want the default %s", got.RequestEndSilence(), base.RequestEndSilence())
	}
	if got.ConversationTimeout() != base.ConversationTimeout() {
		t.Errorf("timeout = %s, want the default %s", got.ConversationTimeout(), base.ConversationTimeout())
	}
}

func TestVoiceOverridesApply(t *testing.T) {
	got := applyVoiceOverrides(voice.DefaultConfig(t.TempDir()), &config.Config{
		VoiceWakeWord:               "  jarvis  ",
		VoiceEndSilenceMs:           3500,
		VoiceConversationTimeoutSec: 90,
	})

	if got.Wake() != "jarvis" {
		t.Errorf("wake word = %q, want it trimmed to jarvis", got.Wake())
	}
	if got.RequestEndSilence().Milliseconds() != 3500 {
		t.Errorf("end silence = %s, want 3.5s", got.RequestEndSilence())
	}
	if got.ConversationTimeout().Seconds() != 90 {
		t.Errorf("timeout = %s, want 90s", got.ConversationTimeout())
	}
}

// Negative is the documented way to say "never drop out of the conversation",
// so it has to survive the override rather than be treated as unset.
func TestNegativeConversationTimeoutDisablesIt(t *testing.T) {
	got := applyVoiceOverrides(voice.DefaultConfig(t.TempDir()), &config.Config{
		VoiceConversationTimeoutSec: -1,
	})
	if got.ConversationTimeout() != 0 {
		t.Errorf("timeout = %s, want zero meaning never", got.ConversationTimeout())
	}
}
