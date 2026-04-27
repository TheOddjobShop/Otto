//go:build unix

package main

import "regexp"

// Markdown stripping is a safety net: the system prompt already tells
// claude not to emit markdown, but it slips occasionally. Telegram doesn't
// render markdown without parse_mode (which we don't set), so leftover
// punctuation appears literally to the user. These regexes kill the
// marker characters while leaving the surrounding prose intact.
var (
	mdBoldStarRe   = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	// __bold__ allows single underscores inside (e.g. __under_bold__) since
	// `__` is the explicit bold delimiter; non-greedy, no newlines.
	mdBoldUnderRe = regexp.MustCompile(`__(.+?)__`)
	mdItalStarRe   = regexp.MustCompile(`\*([A-Za-z0-9][^*\n]*?[A-Za-z0-9])\*`)
	mdItalUnderRe  = regexp.MustCompile(`\b_([A-Za-z0-9][^_\n]*?[A-Za-z0-9])_\b`)
	mdCodeRe       = regexp.MustCompile("`+([^`\n]+?)`+")
	mdHeaderLineRe = regexp.MustCompile(`(?m)^#{1,6}[ \t]+`)
	mdLinkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\n\s]+)\)`)
)

// stripMarkdown removes common Markdown markers so the result reads well
// as plain text on Telegram. Not a full parser — handles the cases claude
// actually emits in chat replies:
//
//   - **bold** and __bold__ → bold
//   - *italic* and _italic_ → italic (underscore form respects word
//     boundaries so identifiers like do_not_strip survive intact)
//   - `code` → code (inline only; multi-line ``` blocks pass through)
//   - # / ## / ### headers → plain text
//   - [label](url) → label (url)
//
// Order: bold before italic so **foo** is consumed first, then code,
// headers, links. Math expressions like "2 * 3" are not stripped because
// the italic regex requires alphanumeric boundaries on both sides.
func stripMarkdown(s string) string {
	s = mdBoldStarRe.ReplaceAllString(s, "$1")
	s = mdBoldUnderRe.ReplaceAllString(s, "$1")
	s = mdItalStarRe.ReplaceAllString(s, "$1")
	s = mdItalUnderRe.ReplaceAllString(s, "$1")
	s = mdCodeRe.ReplaceAllString(s, "$1")
	s = mdHeaderLineRe.ReplaceAllString(s, "")
	s = mdLinkRe.ReplaceAllString(s, "$1 ($2)")
	return s
}
