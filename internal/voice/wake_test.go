package voice

import "testing"

func TestStripWakeWordGrammar(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantCmd string
		wantHit bool
	}{
		// The forms people actually say.
		{"bare", "otto", "", true},
		{"comma", "otto, what's the weather", "what's the weather", true},
		{"colon", "otto: what's the weather", "what's the weather", true},
		{"space", "otto what's the weather", "what's the weather", true},
		{"dash", "otto - what's the weather", "what's the weather", true},
		{"bang", "otto! what's the weather", "what's the weather", true},
		{"question", "otto? you there", "you there", true},
		{"trailing period", "otto.", "", true},

		// Connectives, up to two.
		{"hey", "hey otto what's up", "what's up", true},
		{"okay", "okay otto do it", "do it", true},
		{"two connectives", "okay so otto do it", "do it", true},
		{"um", "um, otto, remind me", "remind me", true},

		// ASR mishearings from real transcripts.
		{"auto", "auto what's the weather", "what's the weather", true},
		{"otter", "otter what's the weather", "what's the weather", true},
		{"oto", "oto do the thing", "do the thing", true},
		{"oh no multiword", "oh no what's the weather", "what's the weather", true},
		{"aura", "aura check my mail", "check my mail", true},
		{"ado", "ado check my mail", "check my mail", true},

		// Fuzzy: distance 1 on a short token.
		{"otta", "otta check my mail", "check my mail", true},
		{"ottp", "ottp check my mail", "check my mail", true},

		// Case and whitespace.
		{"caps", "OTTO WHAT'S UP", "what's up", true},
		{"mixed", "Otto, What's Up?", "what's up", true},
		{"padded", "   otto   do it   ", "do it", true},

		// Non-matches.
		{"empty", "", "", false},
		{"no wake", "what's the weather", "what's the weather", false},
		{"substring", "ottoman empire history", "ottoman empire history", false},
		{"mid-sentence", "i asked otto about it", "i asked otto about it", false},
		{"three connectives", "okay so um otto do it", "okay so um otto do it", false},
		{"bare hey", "hey", "hey", false},
		{"heyy not peeled", "heyy otto", "heyy otto", false},

		// "or" is the wake word only as a whole utterance.
		{"sole or", "or", "", true},
		{"sole or punctuated", "or?", "", true},
		{"or connector", "or we could go home", "or we could go home", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, hit := StripWakeWord(tc.input, "otto")
			if hit != tc.wantHit {
				t.Fatalf("StripWakeWord(%q) hit = %v, want %v (cmd=%q)", tc.input, hit, tc.wantHit, cmd)
			}
			if cmd != tc.wantCmd {
				t.Errorf("StripWakeWord(%q) cmd = %q, want %q", tc.input, cmd, tc.wantCmd)
			}
		})
	}
}

// Fuzzy matching must stay bounded to short tokens, or long words starting
// similarly would arm the listener constantly.
func TestStripWakeWordFuzzyIsBounded(t *testing.T) {
	for _, s := range []string{"ottoman do it", "automatic do it", "authority do it"} {
		if _, hit := StripWakeWord(s, "otto"); hit {
			t.Errorf("StripWakeWord(%q) matched; fuzzy matching should not reach long tokens", s)
		}
	}
}

func TestStripTrailingWake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"thanks otto", "thanks"},
		{"shut up otto", "shut up"},
		{"thanks auto", "thanks"},
		{"thanks", "thanks"},
		{"otto", "otto"}, // single token: nothing to strip
		// A multi-word alias must not be stripped from the tail, or this
		// closer loses its meaning.
		{"oh no problem", "oh no problem"},
	}
	for _, tc := range tests {
		if got := StripTrailingWake(tc.in, "otto"); got != tc.want {
			t.Errorf("StripTrailingWake(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsMuteCommand(t *testing.T) {
	yes := []string{
		"mute", "be quiet", "shut up", "shush", "silence",
		"go to sleep", "zip it", "never mind", "stop listening",
		"uh shut up dude", "otto be quiet please",
	}
	for _, s := range yes {
		if !IsMuteCommand(s) {
			t.Errorf("IsMuteCommand(%q) = false, want true", s)
		}
	}
	no := []string{"", "what's the weather", "quietly add a task", "unmute"}
	for _, s := range no {
		if IsMuteCommand(s) {
			t.Errorf("IsMuteCommand(%q) = true, want false", s)
		}
	}
}

// Single-word phrases must match whole tokens only — otherwise "quiet" fires
// inside "quietly" and mutes Otto mid-request.
func TestSingleWordPhrasesMatchWholeTokens(t *testing.T) {
	for _, s := range []string{"quietly note this", "sleeping arrangements", "muted colors"} {
		if IsMuteCommand(s) {
			t.Errorf("IsMuteCommand(%q) = true; single-word phrases must match whole tokens", s)
		}
	}
}

func TestIsWakeCommandIsStrict(t *testing.T) {
	for _, s := range []string{"wake up", "come back", "unmute", "Wake Up.", " wake up "} {
		if !IsWakeCommand(s) {
			t.Errorf("IsWakeCommand(%q) = false, want true", s)
		}
	}
	// Strictness is the point: an accidental unmute restarts a voice in a room
	// where it was deliberately silenced.
	for _, s := range []string{
		"", "hello", "you there", "ready", "back",
		"wake up the kids", "i need to wake up early", "come back later",
	} {
		if IsWakeCommand(s) {
			t.Errorf("IsWakeCommand(%q) = true, want false — unmute must not fire on conversation", s)
		}
	}
}

func TestIsCloserCommand(t *testing.T) {
	yes := []string{
		"thanks", "thank you", "that's all", "bye", "appreciate it",
		"we're good", "perfect", "good job", "go away", "later",
		"okay cool", "i'm done",
	}
	for _, s := range yes {
		if !IsCloserCommand(s) {
			t.Errorf("IsCloserCommand(%q) = false, want true", s)
		}
	}
	no := []string{"", "thanks for nothing, now do the other one", "what's next"}
	for _, s := range no {
		if s == "" {
			if IsCloserCommand(s) {
				t.Errorf("IsCloserCommand(%q) = true, want false", s)
			}
			continue
		}
		_ = s
	}
}

func TestAnyMatches(t *testing.T) {
	variants := []string{"shut up otto", "shut up", ""}
	if !AnyMatches(variants, IsMuteCommand) {
		t.Error("AnyMatches should find the mute phrase across variants")
	}
	if AnyMatches([]string{"", ""}, IsMuteCommand) {
		t.Error("AnyMatches on empty variants should be false")
	}
}

func TestCannedPhrasesCoverEveryPicker(t *testing.T) {
	all := map[string]bool{}
	for _, p := range CannedPhrases() {
		all[p] = true
	}
	// Every phrase a picker can return must be pre-renderable, or the cache
	// silently misses on exactly the latency-sensitive replies it exists for.
	for _, list := range [][]string{ackGreetings, muteAcks, unmuteAcks, closerAcks} {
		for _, p := range list {
			if !all[p] {
				t.Errorf("phrase %q is reachable from a picker but not in CannedPhrases", p)
			}
		}
	}
	if len(CannedPhrases()) == 0 {
		t.Fatal("CannedPhrases is empty")
	}
}

func TestPickersReturnListedPhrases(t *testing.T) {
	in := func(s string, list []string) bool {
		for _, v := range list {
			if v == s {
				return true
			}
		}
		return false
	}
	for i := 0; i < 50; i++ {
		if !in(PickGreeting(), ackGreetings) {
			t.Fatal("PickGreeting returned an unlisted phrase")
		}
		if !in(PickMuteAck(), muteAcks) {
			t.Fatal("PickMuteAck returned an unlisted phrase")
		}
		if !in(PickUnmuteAck(), unmuteAcks) {
			t.Fatal("PickUnmuteAck returned an unlisted phrase")
		}
		if !in(PickCloserAck(), closerAcks) {
			t.Fatal("PickCloserAck returned an unlisted phrase")
		}
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"otto", "otto", 0},
		{"otto", "otta", 1},
		{"otto", "oto", 1},
		{"otto", "auto", 2},
		{"", "abc", 3},
		{"abc", "", 3},
	}
	for _, tc := range tests {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
