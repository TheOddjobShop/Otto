package voice

import (
	"regexp"
	"strings"
)

// Markdown and formatting patterns piper would otherwise read out loud.
//
// This is a backstop, not the primary mechanism. Otto is told in his system
// prompt not to emit markdown on a voice turn — but a model told not to do
// something still occasionally does, and the failure mode is piper solemnly
// pronouncing "asterisk asterisk important asterisk asterisk", which is both
// absurd and hard to diagnose from audio alone.
var (
	mdCodeFence   = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\\n?(.*?)```")
	mdBoldItalics = regexp.MustCompile(`\*{1,3}([^*\n]+)\*{1,3}`)
	mdUnderscore  = regexp.MustCompile(`(^|\s)_([^_\n]+)_($|\s)`)
	mdInlineCode  = regexp.MustCompile("`([^`\n]+)`")
	mdHeading     = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	mdLink        = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	mdListBullet  = regexp.MustCompile(`(?m)^\s*[-*+•]\s+`)
	mdListNumber  = regexp.MustCompile(`(?m)^\s*\d+[.)]\s+`)
	mdBlockquote  = regexp.MustCompile(`(?m)^\s*>\s?`)
	mdRule        = regexp.MustCompile(`(?m)^\s*([-*_─]{3,})\s*$`)
	mdEmoji       = regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}\x{1F000}-\x{1F2FF}\x{FE0F}]`)
	multiBlank    = regexp.MustCompile(`\n{3,}`)
)

// SanitizeForTTS strips formatting that does not survive being spoken. Safe to
// run on already-clean text, and safe to run twice.
func SanitizeForTTS(s string) string {
	s = mdCodeFence.ReplaceAllString(s, "$1")
	s = mdBoldItalics.ReplaceAllString(s, "$1")
	s = mdUnderscore.ReplaceAllString(s, "$1$2$3")
	s = mdInlineCode.ReplaceAllString(s, "$1")
	s = mdHeading.ReplaceAllString(s, "")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdRule.ReplaceAllString(s, "")
	s = mdBlockquote.ReplaceAllString(s, "")
	s = mdListBullet.ReplaceAllString(s, "")
	s = mdListNumber.ReplaceAllString(s, "")
	s = mdEmoji.ReplaceAllString(s, "")
	s = multiBlank.ReplaceAllString(s, "\n\n")

	// Collapse each line's internal whitespace but keep paragraph breaks: the
	// sentence splitter downstream uses newlines as a boundary hint.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(ln)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
