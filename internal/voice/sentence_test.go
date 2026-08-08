package voice

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitSentencesBasic(t *testing.T) {
	got := SplitSentences("Your build is green. I pushed the fix already. Nothing else pending.")
	want := []string{
		"Your build is green.",
		"I pushed the fix already.",
		"Nothing else pending.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// The failure that makes streaming TTS sound broken: a false boundary inside a
// number or abbreviation, which piper renders with a falling "end of thought"
// intonation mid-sentence.
func TestSplitSentencesDoesNotBreakOnDecimalsOrAbbreviations(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int // expected sentence count
	}{
		{"decimal", "The build took 3.5 seconds and everything passed fine.", 1},
		{"version", "You are running version 1.2.3 of the tool right now.", 1},
		{"honorific", "Dr. Smith replied to your message this afternoon.", 1},
		{"eg", "Some things e.g. the calendar sync are still pending here.", 1},
		{"initial", "J. Smith sent you a long message this afternoon.", 1},
		{"filename", "I edited handler.go and the tests are green now.", 1},
		{"month", "The invoice is dated Jan. 14 and has not been paid.", 1},
		{"two real", "The build is green. Everything else looks fine too.", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitSentences(tc.in)
			if len(got) != tc.want {
				t.Errorf("SplitSentences(%q) produced %d sentences %q, want %d", tc.in, len(got), got, tc.want)
			}
		})
	}
}

func TestSplitSentencesKeepsTerminatorRuns(t *testing.T) {
	got := SplitSentences("Are you serious?! That cannot be right at all. Let me look again.")
	if len(got) != 3 {
		t.Fatalf("got %d sentences %q, want 3", len(got), got)
	}
	if !strings.HasSuffix(got[0], "?!") {
		t.Errorf("first sentence %q should keep its full terminator run", got[0])
	}
}

func TestSplitSentencesEllipsis(t *testing.T) {
	got := SplitSentences("Let me think about that... Actually you are right about it.")
	if len(got) != 2 {
		t.Fatalf("got %d sentences %q, want 2", len(got), got)
	}
	if !strings.HasSuffix(got[0], "...") {
		t.Errorf("ellipsis should stay attached, got %q", got[0])
	}
}

// The streaming contract: a terminator at the very end of the buffer is
// ambiguous, because the next chunk could continue the token ("3.1" + "4").
func TestSplitterWaitsForProofOfBoundary(t *testing.T) {
	var s SentenceSplitter
	if got := s.Push("The value is 3."); len(got) != 0 {
		t.Errorf("got %q; a trailing terminator is not yet a boundary", got)
	}
	if got := s.Push("14 exactly."); len(got) != 0 {
		t.Errorf("got %q; still no following character to confirm the end", got)
	}
	got := s.Push(" And that is final enough.")
	if len(got) != 1 || got[0] != "The value is 3.14 exactly." {
		t.Errorf("got %q, want one sentence with the decimal intact", got)
	}
}

func TestSplitterStreamsIncrementally(t *testing.T) {
	var s SentenceSplitter
	var all []string
	// Deliberately split mid-word, the way a token stream actually arrives.
	for _, chunk := range []string{
		"Your tests are ", "passing now. ", "I fixed the ",
		"import cycle in the ", "store package. ", "Nothing else to report.",
	} {
		all = append(all, s.Push(chunk)...)
	}
	if rest := s.Flush(); rest != "" {
		all = append(all, rest)
	}
	want := []string{
		"Your tests are passing now.",
		"I fixed the import cycle in the store package.",
		"Nothing else to report.",
	}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("got %q\nwant %q", all, want)
	}
}

// A reply that never ends in punctuation must still be spoken — Otto often
// signs off without one, and losing that text entirely would be silent failure.
func TestSplitterFlushReturnsUnterminatedTail(t *testing.T) {
	var s SentenceSplitter
	s.Push("here you go")
	if !s.Pending() {
		t.Error("Pending should be true with buffered text")
	}
	if got := s.Flush(); got != "here you go" {
		t.Errorf("Flush = %q, want the unterminated tail", got)
	}
	if s.Pending() {
		t.Error("Flush should have emptied the buffer")
	}
}

// Short fragments are held back so each one does not become its own piper
// spawn with its own falling intonation.
func TestSplitterHoldsBackShortFragments(t *testing.T) {
	var s SentenceSplitter
	if got := s.Push("Yes. "); len(got) != 0 {
		t.Errorf("got %q; a fragment under the minimum should keep buffering", got)
	}
	got := s.Push("Everything is done and green.")
	if len(got) != 0 {
		// The combined text ends the buffer, so it comes out on Flush.
		t.Logf("mid-stream emission: %q", got)
	}
	if rest := s.Flush(); !strings.HasPrefix(rest, "Yes.") {
		t.Errorf("Flush = %q, want the short fragment merged with what followed", rest)
	}
}

// A blank line is an explicit break and must flush regardless of length, or a
// deliberately terse line would be held hostage by the minimum.
func TestSplitterNewlineIsHardBoundary(t *testing.T) {
	var s SentenceSplitter
	got := s.Push("Done\nNow the next part of the answer follows here.")
	if len(got) != 1 || got[0] != "Done" {
		t.Errorf("got %q, want a hard break at the newline even below the minimum", got)
	}
}

func TestSplitterEmptyInput(t *testing.T) {
	var s SentenceSplitter
	if got := s.Push(""); got != nil {
		t.Errorf("empty push returned %q, want nil", got)
	}
	if got := s.Flush(); got != "" {
		t.Errorf("empty flush returned %q, want empty", got)
	}
	if got := SplitSentences(""); len(got) != 0 {
		t.Errorf("SplitSentences(\"\") = %q, want none", got)
	}
}

func TestSplitterMinCharsOverride(t *testing.T) {
	s := SentenceSplitter{MinChars: 1}
	got := s.Push("Yes. No. Maybe so.")
	if len(got) < 2 {
		t.Errorf("got %q, want short sentences emitted with MinChars=1", got)
	}
}
