package voice

import (
	"math/rand"
	"strings"
)

// Wake-word and conversational phrase matching.
//
// Everything here is pure string work, which is the point: it is the part of
// the voice stack most likely to be wrong in practice and the part that can be
// tested exhaustively without a microphone.
//
// The governing bias, which shows up in nearly every decision below: a false
// positive costs one ignored turn, while a false negative means Otto silently
// did not hear you — and silence is the failure that makes a voice assistant
// feel broken rather than imperfect. So wake matching is generous and unmute
// matching is strict, for opposite reasons.

// wakeAliases are the ways ASR actually renders "otto". Every entry past the
// first was observed in real whisper transcripts rather than guessed:
// homophones ("auto", "otter"), truncations ("oto", "ado"), and outright
// mishearings ("oh no", "arro", "aura", "auta").
//
// "or" is excluded here and handled as a special case below — it is a common
// enough mishearing to matter, but far too common as an English connector to
// accept in the middle of a sentence.
var wakeAliases = []string{
	"auto", "otter", "oto", "oh no", "arro", "aura", "auta", "ado",
}

// connectives are filler words people put before a wake word without thinking:
// "okay otto", "hey otto", "um, otto". Up to two are skipped so "okay so otto"
// still lands.
var connectives = map[string]bool{
	"okay": true, "ok": true, "hey": true, "yo": true,
	"hi": true, "alright": true, "um": true, "uh": true, "so": true,
}

// tokenize splits on whitespace and sentence punctuation, lowercasing as it
// goes. Apostrophes deliberately stay inside tokens so "what's" survives as one
// word. Doing this once means the matchers below never have to enumerate every
// (comma|period|space) variant by hand.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', '.', '!', '?', ':', ';', '-':
			return true
		}
		return false
	})
}

// StripWakeWord removes a leading wake word (with any connectives and
// punctuation around it) and returns the remaining command plus whether the
// wake word matched at all.
//
// Matching rules, in order:
//   - A lone "or" counts as the wake word. Longer utterances starting with
//     "or" do not, so "or we could go home" stays ignored.
//   - Up to two leading connectives are skipped.
//   - Then an alias must match. Single-word aliases also accept Levenshtein
//     distance 1 on tokens of 3–5 characters, which covers the drift smaller
//     whisper models produce without letting long words match loosely.
func StripWakeWord(text, wake string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return t, false
	}

	seen := map[string]bool{}
	var aliases []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		aliases = append(aliases, s)
	}
	add(wake)
	for _, a := range wakeAliases {
		add(a)
	}
	if len(aliases) == 0 {
		return t, false
	}

	tokens := tokenize(t)
	if len(tokens) == 0 {
		return t, false
	}

	if len(tokens) == 1 && tokens[0] == "or" {
		return "", true
	}

	idx := 0
	for idx < len(tokens) && idx < 2 && connectives[tokens[idx]] {
		idx++
	}
	if idx >= len(tokens) {
		return t, false
	}

	matched := false
	for _, alias := range aliases {
		parts := strings.Split(alias, " ")
		if idx+len(parts) > len(tokens) {
			continue
		}
		allMatch := true
		for i, p := range parts {
			tok := tokens[idx+i]
			if tok == p {
				continue
			}
			// Fuzzy matching is restricted to single-word aliases on short
			// tokens. Allowing it inside a multi-word alias would let one
			// sloppy half of "oh no" match far too much.
			if len(parts) == 1 && len(tok) >= 3 && len(tok) <= 5 && levenshtein(tok, p) <= 1 {
				continue
			}
			allMatch = false
			break
		}
		if allMatch {
			matched = true
			idx += len(parts)
			break
		}
	}
	if !matched {
		return t, false
	}
	if idx >= len(tokens) {
		return "", true
	}
	// Rejoin as plain space-separated words. The model does not need the
	// original punctuation, and whisper's punctuation is unreliable anyway.
	return strings.Join(tokens[idx:], " "), true
}

// StripTrailingWake removes a trailing wake-word token so "thanks otto" matches
// the same closer as "thanks". Only single-token aliases are stripped —
// removing a trailing "oh no" would eat the tail of "oh no problem".
func StripTrailingWake(text, wake string) string {
	tokens := tokenize(text)
	if len(tokens) < 2 {
		return text
	}
	last := tokens[len(tokens)-1]
	candidates := append([]string{strings.ToLower(strings.TrimSpace(wake))}, wakeAliases...)
	for _, a := range candidates {
		if a == "" || strings.Contains(a, " ") {
			continue
		}
		if a == last {
			return strings.Join(tokens[:len(tokens)-1], " ")
		}
	}
	return text
}

// muteCommands put Otto into muted state, where everything except a wake
// command is ignored.
var muteCommands = []string{
	"mute", "be quiet", "shut up", "shush", "hush",
	"silent", "silence", "quiet", "be still",
	"go to sleep", "sleep", "go to bed",
	"stop listening",
	"zip it", "nevermind", "never mind",
}

// wakeCommands resume from muted state. Intentionally tiny and strictly
// matched: this is the one list where a false positive is the expensive
// direction, because an accidental unmute means Otto starts talking again in a
// room where he was deliberately silenced. Greetings, "back", "ready" and "you
// there" were all considered and rejected for firing in normal conversation.
var wakeCommands = []string{
	"wake up",
	"come back",
	"unmute",
}

// closerCommands end an active conversation. Matched without requiring the wake
// word, because by definition the user is already talking to Otto — and they
// fast-path out without a model call, so a missed match costs a pointless
// round-trip just to be told nothing.
var closerCommands = []string{
	"thanks", "thank you", "thank u", "thanks a lot", "thanks so much",
	"thanks man", "thanks bro", "thanks dude",
	"appreciate it", "appreciate you", "appreciate that",
	"much appreciated", "i appreciate it", "i appreciate you",
	"that's all", "that is all", "that's it", "that is it",
	"that'll do", "that will do", "thatll do",
	"no worries", "no worry", "all good", "all's good", "alls good",
	"we're good", "we good", "were good",
	"you're good", "you good", "youre good",
	"i'm good", "im good", "i'm set", "im set",
	"okay cool", "ok cool", "cool thanks", "cool thank you",
	"perfect thanks", "perfect thank you", "perfect", "awesome thanks",
	"bye", "goodbye", "see ya", "see you", "later",
	"done", "i'm done", "im done", "done for now", "we're done", "we done",
	"that's enough", "that is enough", "enough for now",
	"good work", "nice work", "good job", "nice job",
	"let's stop", "lets stop", "stop for now",
	"go away",
}

// ackGreetings answer a bare wake word. Varied so the interaction does not feel
// like a doorbell.
var ackGreetings = []string{
	"Yes?", "Yes sir?", "What's up?", "Mhm?", "Yeah?",
	"I'm here.", "I'm listening.", "Go ahead.", "Go on.",
	"At your service.", "Ready when you are.", "You got it, what's up?",
	"All ears.", "What do you need?", "Hit me.", "Yes, boss?",
}

var muteAcks = []string{
	"Alright, going quiet.", "Got it, I'll shut up.", "Shutting up.",
	"On mute.", "Zipped.", "Okay, I'll be quiet.", "Standing by silently.",
	"Muted. Say, otto, wake up, when you want me back.",
}

var unmuteAcks = []string{
	"I'm back.", "Back online.", "I'm listening again.", "All ears again.",
	"Ready.", "Yes?", "Back at it.", "Awake.",
}

var closerAcks = []string{
	"You got it.", "Anytime.", "Glad to help.", "Happy to help.",
	"My pleasure.", "No problem.", "Catch you later.", "Here whenever.",
	"Talk soon.", "Alright, talk soon.", "Sounds good.", "Okay.",
	"You're welcome.", "Of course.", "Call me when you need me.",
	"Standing by.", "Later.", "Take care.", "All yours.", "Hit me up whenever.",
}

// IsMuteCommand reports whether a command asks Otto to go quiet.
func IsMuteCommand(cmd string) bool { return matchesPhrase(cmd, muteCommands) }

// IsCloserCommand reports whether a phrase ends the conversation.
func IsCloserCommand(cmd string) bool { return matchesPhrase(cmd, closerCommands) }

// IsWakeCommand reports whether a command resumes from muted state. Uses exact
// matching rather than the lenient containment IsMuteCommand uses — see
// wakeCommands for why the asymmetry is deliberate.
func IsWakeCommand(cmd string) bool {
	s := normalizePhrase(cmd)
	if s == "" {
		return false
	}
	for _, p := range wakeCommands {
		if s == p {
			return true
		}
	}
	return false
}

// AnyMatches reports whether pred holds for any of the given variants. Callers
// try a transcript in several normalized forms (raw, wake-stripped,
// trailing-wake-stripped) so "shut up otto" matches the same phrase as "shut
// up".
func AnyMatches(variants []string, pred func(string) bool) bool {
	for _, v := range variants {
		if v != "" && pred(v) {
			return true
		}
	}
	return false
}

// matchesPhrase reports whether cmd contains any listed phrase, normalizing
// hard first because whisper introduces filler, punctuation and casing noise.
//
// Multi-word phrases tolerate surrounding filler ("uh otto, shut up dude"),
// while single-word phrases must match a whole token — otherwise "stop" would
// fire on "stopping" and, worse, on "let's stop" being handled elsewhere.
func matchesPhrase(cmd string, list []string) bool {
	s := normalizePhrase(cmd)
	if s == "" {
		return false
	}
	for _, p := range list {
		if s == p {
			return true
		}
		if strings.Contains(p, " ") {
			if strings.Contains(" "+s+" ", " "+p+" ") {
				return true
			}
			continue
		}
		for _, tok := range strings.Fields(s) {
			if tok == p {
				return true
			}
		}
	}
	return false
}

func normalizePhrase(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.Trim(s, " .!?,'\"")
	return strings.Join(strings.Fields(s), " ")
}

// PickGreeting returns a random acknowledgment for a bare wake word.
func PickGreeting() string { return pick(ackGreetings) }

// PickMuteAck returns a random confirmation for mute.
func PickMuteAck() string { return pick(muteAcks) }

// PickUnmuteAck returns a random confirmation for unmute.
func PickUnmuteAck() string { return pick(unmuteAcks) }

// PickCloserAck returns a random sign-off.
func PickCloserAck() string { return pick(closerAcks) }

// CannedPhrases returns every distinct phrase Otto can speak without a model
// call, for cache pre-rendering. Order is stable so the warm pass is
// deterministic.
//
// Deduplicated because the lists deliberately overlap — "Yes?" is both a
// greeting and an unmute acknowledgment — and rendering the same audio twice
// under the same cache key is pure waste.
func CannedPhrases() []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(ackGreetings)+len(muteAcks)+len(unmuteAcks)+len(closerAcks))
	for _, list := range [][]string{ackGreetings, muteAcks, unmuteAcks, closerAcks} {
		for _, p := range list {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func pick(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[rand.Intn(len(list))]
}

// levenshtein returns the edit distance between a and b, using two rolling rows
// rather than a full matrix — the inputs here are single short words.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
