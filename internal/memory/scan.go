// Package memory manages Otto's bounded, always-injected curated-memory core
// (USER.md + MEMORY.md): loading, formatting for prompt injection, and
// guarded add/replace/remove edits.
package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// secretPatterns match credential shapes that must never be persisted into a
// surface that is injected verbatim into every prompt.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{10,}`),         // Anthropic keys
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                   // AWS access key id
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), // PEM private keys
	regexp.MustCompile(`ssh-(rsa|ed25519) AAAA[0-9A-Za-z+/]+`),
	regexp.MustCompile(`gh[posru]_[A-Za-z0-9]{20,}`),           // GitHub PAT/OAuth/server/refresh tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),         // GitHub fine-grained PAT
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),         // Slack tokens
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),               // Google API keys
	regexp.MustCompile(`\b\d{6,}:[A-Za-z0-9_-]{30,}\b`),        // Telegram bot tokens
	regexp.MustCompile(`\b(?:ntn_|secret_)[A-Za-z0-9]{30,}\b`), // Notion integration tokens
}

// injectionPatterns match a small, illustrative sample of prompt-injection
// phrasings. This blocklist is intentionally incomplete — it is trivially
// bypassed by alternative wording (e.g. "forget your earlier instructions") or
// Unicode homoglyphs. For this single-owner bot the practical threat is low
// (only the trusted owner's Claude session writes memory), but callers should
// not treat a clean scan as a security guarantee. A future improvement could
// use an embedding-based classifier or a more comprehensive pattern set.
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

// isInvisibleRune reports whether r is an invisible or format-control
// character that has no place in a plain-text memory entry: the entire
// Unicode Cf (format) category — zero-width chars, bidi controls, BOM, soft
// hyphen, and tag characters (U+E0001..E007F, the classic ASCII-smuggling
// range) — plus the unassigned remainder of the tag block and the variation
// selectors (U+FE00..FE0F, U+E0100..E01EF), which can encode hidden payloads.
// Ordinary whitespace (space, tab, newline) is allowed. Note this also
// rejects ZWJ and VS-16, so composed emoji sequences are refused alongside
// other invisible runes; bare emoji still pass.
func isInvisibleRune(r rune) bool {
	switch {
	case unicode.Is(unicode.Cf, r): // all format chars incl. bidi, ZW*, tags
		return true
	case r >= 0xE0000 && r <= 0xE007F: // full tag block, incl. unassigned gaps
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement
		return true
	}
	return false
}
