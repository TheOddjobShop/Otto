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
//
// The pet name MUST be the first word. "I asked toto about it" does
// NOT route to Toto. "totoman" does NOT route. This is intentional:
// strict matching avoids false positives on casual references.
func (r *petRegistry) Match(text string) (Pet, string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", false
	}

	// Allow optional @ prefix.
	head := text
	if strings.HasPrefix(head, "@") {
		head = head[1:]
	}

	// Find end of first word (anything not [A-Za-z0-9]).
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

	// Strip the address: skip the name, then any trailing punctuation
	// or whitespace, leaving the user's actual content.
	rest := strings.TrimLeft(head[end:], " \t,:!?.-")
	return matched, strings.TrimSpace(rest), true
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
