//go:build unix

package main

import "testing"

func TestStripMarkdownBold(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold**", "bold"},
		{"say **hi** there", "say hi there"},
		{"**multi word bold**", "multi word bold"},
		{"**a** and **b**", "a and b"},
		{"no markdown", "no markdown"},
		{"__under_bold__", "under_bold"},
	}
	for _, tc := range cases {
		got := stripMarkdown(tc.in)
		if got != tc.want {
			t.Errorf("stripMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripMarkdownItalic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"*italic*", "italic"},
		{"say *hi* now", "say hi now"},
		{"_italic_", "italic"},
		{"do_not_strip", "do_not_strip"}, // identifier survives
		{"2 * 3", "2 * 3"},               // math, not emphasis
		{"*", "*"},                        // bare asterisk
	}
	for _, tc := range cases {
		got := stripMarkdown(tc.in)
		if got != tc.want {
			t.Errorf("stripMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripMarkdownCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"`foo`", "foo"},
		{"use `cmd` here", "use cmd here"},
		{"no code at all", "no code at all"},
	}
	for _, tc := range cases {
		got := stripMarkdown(tc.in)
		if got != tc.want {
			t.Errorf("stripMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripMarkdownHeader(t *testing.T) {
	in := "# Big\n## Medium\n### Small\nplain"
	want := "Big\nMedium\nSmall\nplain"
	if got := stripMarkdown(in); got != want {
		t.Errorf("stripMarkdown headers:\ngot  %q\nwant %q", got, want)
	}
}

func TestStripMarkdownLink(t *testing.T) {
	in := "see [docs](https://example.com) for details"
	want := "see docs (https://example.com) for details"
	if got := stripMarkdown(in); got != want {
		t.Errorf("stripMarkdown(%q) = %q, want %q", in, got, want)
	}
}

func TestStripMarkdownCombined(t *testing.T) {
	// Approximates a realistic claude reply that mixes everything.
	in := "## Inbox\n\n**School**\n- *Math 19B*: midterm review\n- `Edfinity` HW reminder"
	want := "Inbox\n\nSchool\n- Math 19B: midterm review\n- Edfinity HW reminder"
	if got := stripMarkdown(in); got != want {
		t.Errorf("stripMarkdown combined:\ngot  %q\nwant %q", got, want)
	}
}
