//go:build unix

package main

import (
	"context"
	"testing"
)

// stubPet implements Pet for tests. It records the message body it
// received so tests can assert on the routing behavior.
type stubPet struct {
	name     string
	received []string
}

func (s *stubPet) Name() string { return s.name }
func (s *stubPet) Reply(ctx context.Context, chatID int64, userMessage string) {
	s.received = append(s.received, userMessage)
}

func TestPetRegistryMatch(t *testing.T) {
	toto := &stubPet{name: "toto"}
	toot := &stubPet{name: "toot"}
	r := newPetRegistry(toto, toot)

	cases := []struct {
		input    string
		wantPet  string // "" means no match expected
		wantBody string
	}{
		// Bare pings.
		{"toto", "toto", ""},
		{"TOTO", "toto", ""}, // case-insensitive
		{"  toto  ", "toto", ""},
		{"toot", "toot", ""},

		// Punctuation delimiters.
		{"toto, hi", "toto", "hi"},
		{"toto: hello there", "toto", "hello there"},
		{"toto - sup", "toto", "sup"},
		{"toto! how are you", "toto", "how are you"},
		{"toto? you up?", "toto", "you up?"},
		{"toto.", "toto", ""},

		// Space delimiter.
		{"toto how are you", "toto", "how are you"},
		{"toot what's the latest", "toot", "what's the latest"},

		// @ prefix.
		{"@toto hi", "toto", "hi"},
		{"@TOTO HI", "toto", "HI"},
		{"@toto, hi", "toto", "hi"},
		{"@toot tell me about the release", "toot", "tell me about the release"},

		// Non-matches.
		{"", "", ""},
		{"   ", "", ""},
		{"hello", "", ""},
		{"I asked toto about it", "", ""},   // first word is "I"
		{"hey toto", "", ""},                 // first word is "hey"
		{"totoman", "", ""},                  // first word is "totoman"
		{"toto2", "", ""},                    // first word is "toto2", not "toto"
		{"ototo", "", ""},                    // first word is "ototo"
		{"otto, ping toto for me", "", ""},   // addressed to otto, who isn't a pet
		{"123 toto", "", ""},                 // first word is "123"
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			pet, body, ok := r.Match(c.input)
			if c.wantPet == "" {
				if ok {
					t.Errorf("expected no match for %q, got pet=%q body=%q", c.input, pet.Name(), body)
				}
				return
			}
			if !ok {
				t.Fatalf("expected match for %q, got nothing", c.input)
			}
			if pet.Name() != c.wantPet {
				t.Errorf("pet=%q, want %q", pet.Name(), c.wantPet)
			}
			if body != c.wantBody {
				t.Errorf("body=%q, want %q", body, c.wantBody)
			}
		})
	}
}

func TestPetRegistryFirstMatchWins(t *testing.T) {
	// Two pets with the same name should never happen in practice, but
	// the registry's iteration is deterministic.
	first := &stubPet{name: "duplicate"}
	second := &stubPet{name: "duplicate"}
	r := newPetRegistry(first, second)

	pet, _, ok := r.Match("duplicate, hi")
	if !ok {
		t.Fatal("expected match")
	}
	if pet != first {
		t.Errorf("got second pet, want first (registry iterates in insertion order)")
	}
}
