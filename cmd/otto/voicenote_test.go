//go:build unix

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"otto/internal/telegram"
)

// stubTranscriber stands in for whisper.
type stubTranscriber struct {
	text string
	err  error
}

func (s stubTranscriber) Transcribe(ctx context.Context, wav []byte) (string, error) {
	return s.text, s.err
}

// voiceNoteHandler wires just enough handler to exercise resolveVoiceNote.
func voiceNoteHandler(bot telegram.BotClient, stt stubTranscriber, enabled bool) *handler {
	h := &handler{bot: bot, otto: newOttoState()}
	if enabled {
		h.voiceSTT = stt
	}
	return h
}

// A voice note must never produce silence: every failure path owes the user an
// explanation, because from their side they sent a message and got nothing.
func TestVoiceNoteFailuresAlwaysReply(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		update  telegram.Update
		stt     stubTranscriber
		wantIn  string
	}{
		{
			name:    "stt unavailable",
			enabled: false,
			update:  telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: 3},
			wantIn:  "voice-doctor",
		},
		{
			name:    "too long",
			enabled: true,
			update:  telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: maxVoiceNoteSeconds + 1},
			wantIn:  "shorter",
		},
		{
			name:    "transcription fails",
			enabled: true,
			update:  telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: 3},
			stt:     stubTranscriber{err: errors.New("whisper died")},
			wantIn:  "couldn't make out",
		},
		{
			name:    "silence",
			enabled: true,
			update:  telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: 3},
			stt:     stubTranscriber{text: "   "},
			wantIn:  "didn't catch anything",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bot := &fakeBotWithDownload{
				files: map[string][]byte{"f1": []byte("fake ogg")},
				cts:   map[string]string{"f1": "audio/ogg"},
			}
			h := voiceNoteHandler(bot, tc.stt, tc.enabled)
			// Pass the audio through untouched: these cases are about what
			// happens after decoding, and a real transcoder would reject the
			// placeholder bytes first.
			h.voiceDecode = func(ctx context.Context, data []byte, ext string) ([]byte, error) {
				return data, nil
			}

			text, ok := h.resolveVoiceNote(context.Background(), tc.update)
			if ok {
				t.Fatalf("expected failure, got transcript %q", text)
			}
			bot.mu.Lock()
			sent := append([]sentMsg(nil), bot.sent...)
			bot.mu.Unlock()
			if len(sent) == 0 {
				t.Fatal("no reply sent — a failed voice note must never be silent")
			}
			if !strings.Contains(strings.ToLower(sent[0].text), strings.ToLower(tc.wantIn)) {
				t.Errorf("reply %q should mention %q", sent[0].text, tc.wantIn)
			}
		})
	}
}

func TestVoiceNoteDownloadFailureReplies(t *testing.T) {
	bot := &failingDownloadBot{}
	h := voiceNoteHandler(bot, stubTranscriber{text: "hi"}, true)

	if _, ok := h.resolveVoiceNote(context.Background(),
		telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: 3}); ok {
		t.Fatal("expected failure when the download fails")
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sent) == 0 || !strings.Contains(bot.sent[0].text, "download") {
		t.Errorf("sent %v, want a download-failure explanation", bot.sent)
	}
}

type failingDownloadBot struct{ fakeBot }

func (f *failingDownloadBot) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return nil, "", errors.New("network down")
}

// The whole point of the feature: a spoken message becomes ordinary text and
// takes the identical path a typed one would.
func TestVoiceNoteBecomesText(t *testing.T) {
	bot := &fakeBotWithDownload{
		files: map[string][]byte{"f1": []byte("fake ogg bytes")},
		cts:   map[string]string{"f1": "audio/ogg"},
	}
	h := voiceNoteHandler(bot, stubTranscriber{text: "  what's on my calendar  "}, true)
	h.voiceDecode = func(ctx context.Context, data []byte, ext string) ([]byte, error) {
		if ext != ".oga" {
			t.Errorf("decode called with ext %q, want .oga (Telegram always sends Opus)", ext)
		}
		if string(data) != "fake ogg bytes" {
			t.Errorf("decode got %q, want the downloaded bytes", data)
		}
		return []byte("fake wav"), nil
	}

	text, ok := h.resolveVoiceNote(context.Background(),
		telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: 4})
	if !ok {
		t.Fatal("expected the voice note to resolve")
	}
	if text != "what's on my calendar" {
		t.Errorf("transcript = %q, want it trimmed", text)
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sent) != 0 {
		t.Errorf("sent %v; a successful transcription should stay silent and let the reply speak for itself", bot.sent)
	}
}

func TestVoiceNoteDecodeFailureReplies(t *testing.T) {
	bot := &fakeBotWithDownload{
		files: map[string][]byte{"f1": []byte("x")},
		cts:   map[string]string{"f1": "audio/ogg"},
	}
	h := voiceNoteHandler(bot, stubTranscriber{text: "hi"}, true)
	h.voiceDecode = func(ctx context.Context, data []byte, ext string) ([]byte, error) {
		return nil, errors.New("no decoder")
	}
	if _, ok := h.resolveVoiceNote(context.Background(),
		telegram.Update{ChatID: 7, VoiceFileID: "f1", VoiceSeconds: 3}); ok {
		t.Fatal("expected failure when decoding fails")
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sent) == 0 || !strings.Contains(bot.sent[0].text, "ffmpeg") {
		t.Errorf("sent %v, want a reply naming the fix", bot.sent)
	}
}
