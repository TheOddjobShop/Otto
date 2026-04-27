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

func hardSplit(text string, limit int) []string {
	var out []string
	var cur strings.Builder
	for _, r := range text {
		rl := utf8.RuneLen(r)
		if rl < 0 {
			rl = 1
		}
		if cur.Len()+rl > limit && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
