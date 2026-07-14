// Package telegram implements the Telegram Bot API client used by Otto.
package telegram

import (
	"strings"
	"unicode/utf8"
)

// ChunkMessage splits text into pieces at most `limit` bytes each, preferring
// paragraph (\n\n) boundaries, then newline boundaries, then hard char splits.
// Splits never land mid-rune, so every returned chunk is valid UTF-8.
func ChunkMessage(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if len(text) <= limit {
		return []string{text}
	}
	if chunks := splitOn(text, "\n\n", limit); chunks != nil {
		return chunks
	}
	if chunks := splitOn(text, "\n", limit); chunks != nil {
		return chunks
	}
	return hardSplit(text, limit)
}

func splitOn(text, sep string, limit int) []string {
	parts := strings.Split(text, sep)
	if len(parts) == 1 {
		return nil
	}
	var out []string
	var cur strings.Builder
	for _, p := range parts {
		// If a single part exceeds limit, this strategy can't help.
		if len(p) > limit {
			return nil
		}
		// +len(sep) for the joining sep we'd add if cur is non-empty.
		addLen := len(p)
		if cur.Len() > 0 {
			addLen += len(sep)
		}
		if cur.Len()+addLen > limit {
			out = append(out, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteString(sep)
		}
		cur.WriteString(p)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// hardSplit is the last-resort splitter for a run with no newline
// boundaries. It advances by atomic units so a chunk never ends mid-rune
// nor mid-HTML-entity: when an "&...;" entity begins at the cursor the
// whole entity is kept together, otherwise a single UTF-8 rune advances.
// This protects the escape-then-chunk HTML path (e.g. "&amp;", "&#39;")
// from producing a chunk that Telegram rejects as malformed entities.
func hardSplit(text string, limit int) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(text); {
		unit := entityLen(text[i:])
		if unit == 0 {
			if _, sz := utf8.DecodeRuneInString(text[i:]); sz > 0 {
				unit = sz
			} else {
				unit = 1
			}
		}
		if cur.Len()+unit > limit && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(text[i : i+unit])
		i += unit
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// entityLen returns the byte length of an HTML character entity at the
// start of s ("&" then one-or-more [A-Za-z0-9#] then ";"), or 0 if s does
// not begin with a well-formed entity. A lone "&" in plain text returns 0
// so the caller advances a single rune instead of consuming to a distant
// ";". The scan is bounded to keep a stray "&" cheap.
func entityLen(s string) int {
	if len(s) == 0 || s[0] != '&' {
		return 0
	}
	const maxEntity = 12 // & + up to 10 name bytes + ;
	end := maxEntity
	if end > len(s) {
		end = len(s)
	}
	for j := 1; j < end; j++ {
		c := s[j]
		if c == ';' {
			if j == 1 {
				return 0 // "&;" — no name
			}
			return j + 1
		}
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '#') {
			return 0 // illegal entity char before ';'
		}
	}
	return 0
}
