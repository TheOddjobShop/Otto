package voice

import (
	"strings"
	"unicode"
)

// Streaming sentence segmentation.
//
// This is what lets Otto start speaking before he has finished thinking. Otto's
// stream-json parser already delivers assistant text incrementally, so a
// splitter that emits complete sentences as they form removes the largest term
// in the record → STT → LLM → TTS → play latency chain: the user hears the
// first sentence while the rest is still generating.
//
// Correctness matters more than it looks. Splitting on every "." would break
// "Dr. Smith" into two utterances and read "3.14" as two numbers, and each
// mistake is audible — piper drops its pitch at a sentence end, so a false
// boundary sounds like Otto finishing a thought and starting a new one
// mid-word.

// minSentenceChars is the shortest fragment worth speaking on its own.
//
// Below this we keep buffering. Two reasons: each utterance is a piper
// subprocess, so a stream of two-word fragments multiplies process spawns for
// no gain; and speech synthesized in tiny pieces loses its prosody, because
// each piece gets its own falling intonation. A hard newline still flushes
// regardless, so a genuinely short line ("Done.") is never held hostage.
const minSentenceChars = 16

// abbreviations end in a period without ending a sentence. Stored without the
// trailing dot and matched case-insensitively against the last word.
//
// Membership is judged by which reading is more likely *in a spoken assistant
// reply*, not by which is a legitimate abbreviation somewhere. That excludes
// several tempting entries:
//
//   - "no" — "No. 5" for number five is archaic; "No." as a complete answer is
//     constant. Listing it made "Did it work? No. Try again." run together.
//   - "am"/"pm" — "It's 5 pm." ends sentences routinely, and the dotted form
//     "p.m." is already handled by the single-letter rule below.
//   - "min"/"max" — "that's the max." reads as a sentence far more often than
//     "max. value" reads as an abbreviation.
//
// The cost of a wrong entry here is a merged pair of sentences, which is much
// less jarring than a false split — so when a word is genuinely ambiguous the
// tie goes to leaving it out.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "sr": true, "jr": true,
	"st": true, "mt": true, "vs": true, "etc": true, "eg": true, "ie": true, "al": true,
	"approx": true, "dept": true, "est": true, "fig": true, "inc": true, "ltd": true,
	"vol": true, "cf": true, "ca": true, "circa": true,
	"jan": true, "feb": true, "mar": true, "apr": true, "jun": true, "jul": true,
	"aug": true, "sep": true, "sept": true, "oct": true, "nov": true, "dec": true,
	"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true,
}

// SentenceSplitter accumulates streamed text and hands back complete sentences.
// Not safe for concurrent use; callers drive it from a single stream consumer.
type SentenceSplitter struct {
	buf strings.Builder
	// MinChars overrides minSentenceChars when non-zero. Exposed for tests and
	// for callers that would rather have latency than prosody.
	MinChars int
}

// Push appends streamed text and returns every sentence completed by it.
// Returns nil when the chunk did not finish one.
func (s *SentenceSplitter) Push(chunk string) []string {
	if chunk == "" {
		return nil
	}
	s.buf.WriteString(chunk)
	return s.drain()
}

// Flush returns whatever is buffered, complete sentence or not, and resets.
// Call once when the stream ends — otherwise a reply not ending in punctuation
// (which is common: Otto often signs off without a period) would never be
// spoken at all.
func (s *SentenceSplitter) Flush() string {
	out := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	return out
}

// Pending reports whether anything is buffered.
func (s *SentenceSplitter) Pending() bool {
	return strings.TrimSpace(s.buf.String()) != ""
}

func (s *SentenceSplitter) minChars() int {
	if s.MinChars > 0 {
		return s.MinChars
	}
	return minSentenceChars
}

// drain repeatedly peels complete sentences off the front of the buffer.
func (s *SentenceSplitter) drain() []string {
	var out []string
	for {
		text := s.buf.String()
		idx := findBoundary(text, s.minChars())
		if idx < 0 {
			return out
		}
		sentence := strings.TrimSpace(text[:idx])
		rest := text[idx:]
		s.buf.Reset()
		s.buf.WriteString(strings.TrimLeft(rest, " \t"))
		if sentence != "" {
			out = append(out, sentence)
		}
	}
}

// findBoundary returns the index just past the end of the first complete
// sentence in text, or -1 if there is not one yet.
//
// A boundary requires *proof* that the sentence ended, which during streaming
// means seeing a following character. A terminator at the very end of the
// buffer is ambiguous — the next chunk could be "5" completing "3.1" — so it is
// left for the next Push or for Flush.
func findBoundary(text string, minChars int) int {
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// A blank line is a hard boundary regardless of punctuation or length:
		// it is an explicit paragraph break, and holding one back to satisfy a
		// minimum-length rule would stall short deliberate lines.
		if r == '\n' {
			if strings.TrimSpace(string(runes[:i])) != "" {
				return i + 1
			}
			continue
		}

		if r != '.' && r != '!' && r != '?' {
			continue
		}

		// Consume a run of terminators so "..." and "?!" stay together.
		j := i
		for j+1 < len(runes) && isTerminator(runes[j+1]) {
			j++
		}
		// Need a following character to confirm the sentence really ended.
		if j+1 >= len(runes) {
			return -1
		}
		next := runes[j+1]
		if !unicode.IsSpace(next) {
			// "3.14", "otto.md", "e.g" — not a boundary.
			continue
		}
		if r == '.' && j == i && isAbbreviation(runes[:i]) {
			continue
		}
		if len(strings.TrimSpace(string(runes[:j+1]))) < minChars {
			continue
		}
		return j + 1
	}
	return -1
}

func isTerminator(r rune) bool { return r == '.' || r == '!' || r == '?' }

// isAbbreviation reports whether the word immediately before a period is a
// known abbreviation, or a single letter (which covers initials like "J. Smith"
// and spelled-out acronyms).
func isAbbreviation(before []rune) bool {
	end := len(before)
	start := end
	for start > 0 {
		r := before[start-1]
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			start--
			continue
		}
		break
	}
	word := strings.ToLower(string(before[start:end]))
	if word == "" {
		return false
	}
	if len([]rune(word)) == 1 && unicode.IsLetter([]rune(word)[0]) {
		return true
	}
	return abbreviations[word]
}

// SplitSentences segments a complete string in one pass. Used for text that is
// already whole — a cached phrase, a pet reply — where streaming is irrelevant.
func SplitSentences(text string) []string {
	var s SentenceSplitter
	out := s.Push(text)
	if rest := s.Flush(); rest != "" {
		out = append(out, rest)
	}
	return out
}
