//go:build unix

package main

// Voice mode: what changes when the reply will be spoken rather than read.
//
// The spoken channel cannot carry Otto's text register. His operational footer
// tells him to use blank lines, bullet characters and ALL-CAPS section labels
// for structure — every one of which is either inaudible or actively wrong read
// aloud. And length works completely differently: a six-line answer skims fine
// on a phone and is interminable spoken, because the listener cannot skip ahead
// or re-read.

// voiceSystemPrompt is appended to Otto's per-call system prompt on a spoken
// turn. It is deliberately blunt and short — a long style guide competes with
// the persona for attention, and the failure it prevents is simple.
const voiceSystemPrompt = `───────────────────────────────────────────────
  VOICE MODE — THIS REPLY WILL BE SPOKEN ALOUD
───────────────────────────────────────────────

The user is talking to you out loud and will HEAR this reply through a
speaker. They cannot see it, skim it, or scroll back.

HOW TO ANSWER:
  • Two or three sentences. Lead with the answer, not the preamble.
  • Plain spoken prose. No lists, no bullets, no headers, no markdown,
    no code, no ALL-CAPS labels — none of it survives being spoken.
  • No file paths, URLs, hashes, or long identifiers read aloud. Say
    "the auth handler" rather than the path; say "I put it in your
    notes" rather than the link.
  • Numbers and dates in words, the way you would say them.
  • If the full answer genuinely needs a list or a lot of detail, say
    the headline out loud and offer the rest: "there are six, want me
    to run through them?"

STILL DO THE WORK. Voice mode is about DELIVERY, not scope. If they ask
you to send an email, push to Notion, run the tests, or check the
calendar — do it, using the same tools you always would, and then say in
one sentence what you did. Never skip the work and merely describe it;
never say you will do something instead of doing it. Being brief means
saying less about the work, not doing less of it.`

// composeVoicePrompt layers the voice-mode clause onto a base system prompt.
//
// Appended last, after the persona, the time block and the memory core, so it
// is the final instruction the model reads. Ordering matters here: the
// operational footer earlier in the prompt actively teaches the formatting this
// block has to override.
func composeVoicePrompt(base string) string {
	if base == "" {
		return voiceSystemPrompt
	}
	return base + "\n\n" + voiceSystemPrompt
}
