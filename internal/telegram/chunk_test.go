package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkUnderLimit(t *testing.T) {
	got := ChunkMessage("hello", 4096)
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("ChunkMessage = %q", got)
	}
}

func TestChunkEmptyString(t *testing.T) {
	got := ChunkMessage("", 4096)
	if len(got) != 0 {
		t.Errorf("ChunkMessage(\"\") = %v, want []", got)
	}
}

func TestChunkSplitsAtParagraphBoundary(t *testing.T) {
	// Two paragraphs, total > limit.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	got := ChunkMessage(a+"\n\n"+b, 4096)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(got))
	}
	if got[0] != a {
		t.Errorf("chunk 0 = %q (truncated), want all 'a'", got[0][:20])
	}
	if got[1] != b {
		t.Errorf("chunk 1 = %q (truncated), want all 'b'", got[1][:20])
	}
}

func TestChunkSplitsAtNewlineFallback(t *testing.T) {
	// No paragraph break; uses single newlines.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	got := ChunkMessage(a+"\n"+b, 4096)
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want >=2", len(got))
	}
}

func TestChunkVeryLongUnbrokenText(t *testing.T) {
	// No newlines at all — must fall back to hard char split.
	s := strings.Repeat("x", 10000)
	got := ChunkMessage(s, 4096)
	if len(got) < 3 {
		t.Fatalf("got %d chunks, want >=3", len(got))
	}
	for i, c := range got {
		if len(c) > 4096 {
			t.Errorf("chunk %d exceeds limit: %d", i, len(c))
		}
	}
	if strings.Join(got, "") != s {
		t.Error("rejoined chunks don't match original")
	}
}

func TestChunkPreservesUTF8Boundaries(t *testing.T) {
	// "家" is 3 bytes in UTF-8. Repeating past the limit forces hardSplit
	// to engage; a naive byte split would land mid-rune.
	s := strings.Repeat("家", 2000) // 6000 bytes, no newlines
	got := ChunkMessage(s, 4096)
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want >= 2", len(got))
	}
	for i, c := range got {
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
		if len(c) > 4096 {
			t.Errorf("chunk %d byte length %d > limit 4096", i, len(c))
		}
	}
	if strings.Join(got, "") != s {
		t.Error("round-trip lost data")
	}
}
