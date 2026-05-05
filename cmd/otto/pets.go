//go:build unix

package main

import (
	"context"
	"strings"
)

// Pet is the common interface for addressable companion characters
// like Toto and Toot. The handler's petRegistry routes user messages
// to a pet when the user addresses one by name (see petRegistry.Match
// for the address rules).
//
// Pets receive only the conversational message body — the address
// prefix is stripped before Reply is called, so what arrives is the
// user's actual content (which may be empty for a bare ping like
// "toto").
type Pet interface {
	// Name returns the canonical lowercase identifier (e.g. "toto",
	// "toot"). Used to match against the first word of incoming
	// messages.
	Name() string

	// Reply runs one conversational turn for this pet and sends the
	// result to chatID. Errors are handled internally (logged + a
	// fallback message sent), so this method does not return one.
	Reply(ctx context.Context, chatID int64, userMessage string)
}

// petRegistry is the handler's pet-routing layer. Adding a new pet =
// implement Pet, append the instance in main.go's registry construction.
// First match wins if names collide (they shouldn't — pets are unique).
type petRegistry struct {
	pets []Pet
}

// newPetRegistry constructs a registry from the given pets. Names are
// matched lowercase, so callers don't need to normalize.
func newPetRegistry(pets ...Pet) *petRegistry {
	return &petRegistry{pets: pets}
}

// Match returns the pet addressed by `text` (if any) and the message
// body with the address prefix stripped. Returns (nil, "", false) when
// no pet is addressed.
//
// Recognized address forms (case-insensitive on the pet name):
//
//	<name>            — bare ping
//	<name>, body
//	<name>: body
//	<name> - body
//	<name>! body      (or "?" or ".")
//	<name> body       (whitespace delimiter)
//	@<name> body
//	hey <name> ...    — vocative prefix; everything above also works
//	                     with a leading "hey " (e.g. "hey @toto", "hey toot, hi")
//
// The pet name MUST be the first word (or the second after "hey").
// "I asked toto about it" does NOT route to Toto. "totoman" does NOT
// route. This is intentional: strict matching avoids false positives.
func (r *petRegistry) Match(text string) (Pet, string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", false
	}

	// First attempt: pet name as the first word (with optional @).
	if pet, body, ok := r.matchAddress(text); ok {
		return pet, body, true
	}

	// Second attempt: peel a leading "hey" and retry once. "hey toto"
	// is the most common vocative form that doesn't fit the strict
	// first-word rule, so we accept it here. "heyy" / "heyman" do
	// NOT peel — only the exact word "hey" followed by a non-word
	// character.
	if rest, ok := peelHey(text); ok {
		return r.matchAddress(rest)
	}
	return nil, "", false
}

// matchAddress is the strict first-word matcher (with optional @).
func (r *petRegistry) matchAddress(text string) (Pet, string, bool) {
	head := text
	if strings.HasPrefix(head, "@") {
		head = head[1:]
	}
	end := 0
	for end < len(head) && isWordChar(head[end]) {
		end++
	}
	if end == 0 {
		return nil, "", false
	}
	name := strings.ToLower(head[:end])
	var matched Pet
	for _, p := range r.pets {
		if p.Name() == name {
			matched = p
			break
		}
	}
	if matched == nil {
		return nil, "", false
	}
	rest := strings.TrimLeft(head[end:], " \t,:!?.-")
	return matched, strings.TrimSpace(rest), true
}

// peelHey returns the body of `text` with a leading "hey " (or "hey,"
// etc.) removed. ok=false when text doesn't start with the exact word
// "hey" followed by a non-word character. A bare "hey" with no trailing
// content is also rejected (length < 4) — there's no pet name to address.
func peelHey(text string) (string, bool) {
	if len(text) < 4 {
		return "", false
	}
	if !strings.EqualFold(text[:3], "hey") {
		return "", false
	}
	if isWordChar(text[3]) {
		return "", false // "heyman", "heyy", etc.
	}
	return strings.TrimLeft(text[3:], " \t,:!?.-"), true
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
