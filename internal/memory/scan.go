// Package memory manages Otto's bounded, always-injected curated-memory core
// (USER.md + MEMORY.md): loading, formatting for prompt injection, and
// guarded add/replace/remove edits.
package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// secretPatterns match credential shapes that must never be persisted into a
// surface that is injected verbatim into every prompt.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{10,}`),         // Anthropic keys
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                   // AWS access key id
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), // PEM private keys
	regexp.MustCompile(`ssh-(rsa|ed25519) AAAA[0-9A-Za-z+/]+`),
}

// injectionPatterns match common prompt-injection phrasings.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all )?previous instructions`),
	regexp.MustCompile(`(?i)disregard (the )?(system )?prompt`),
}

// scanContent validates a candidate memory entry. It rejects blank content,
// embedded newlines, credential material, prompt-injection phrasings, and
// invisible / bidi Unicode that could hide payloads in the always-injected
// core. Newlines are disallowed because entries are stored one-per-line and
// deduplicated line-by-line (see entryExists); a multi-line entry would
// silently bypass duplicate detection and corrupt the one-fact-per-line model.
func scanContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory: refuse to store blank content")
	}
	if strings.ContainsAny(content, "\n\r") {
		return fmt.Errorf("memory: entry must be a single line (no newlines)")
	}
	for _, r := range content {
		if isInvisibleRune(r) {
			return fmt.Errorf("memory: content contains disallowed invisible/bidi character U+%04X", r)
		}
	}
	for _, p := range secretPatterns {
		if p.MatchString(content) {
			return fmt.Errorf("memory: content looks like a credential; refusing to store")
		}
	}
	for _, p := range injectionPatterns {
		if p.MatchString(content) {
			return fmt.Errorf("memory: content looks like a prompt-injection attempt; refusing to store")
		}
	}
	return nil
}

// isInvisibleRune reports whether r is a zero-width, bidi-control, or BOM
// character that has no place in a plain-text memory entry. Ordinary
// whitespace (space, tab, newline) is allowed.
func isInvisibleRune(r rune) bool {
	switch {
	case r == '\u061c': // Arabic letter mark (RTL injection vector)
		return true
	case r == '\u180e': // Mongolian vowel separator (zero-width)
		return true
	case r == '\u200b', r == '\u200c', r == '\u200d': // zero-width space / ZWNJ / ZWJ
		return true
	case r >= '\u200e' && r <= '\u200f': // LRM / RLM
		return true
	case r >= '\u202a' && r <= '\u202e': // bidi embeddings / overrides
		return true
	case r == '\u2060', r == '\ufeff': // word joiner / BOM
		return true
	case r >= '\u2066' && r <= '\u2069': // bidi isolates: LRI / RLI / FSI / PDI
		return true
	}
	return false
}
