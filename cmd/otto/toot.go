//go:build unix

package main

import (
	"context"
	_ "embed"
	"html"

	"otto/internal/telegram"
)

// tootArtFile is the bundled owl ASCII-art file. Three blocks separated
// by blank lines; same format the existing parseAsciiArts (in toto.go)
// already handles for Toto's cats.
//
//go:embed toot.txt
var tootArtFile string

// tootCycler hands out the embedded owl arts in shuffled round-robin
// order, so consecutive Toot messages don't repeat the same art.
var tootCycler = newAsciiCycler(parseAsciiArts(tootArtFile))

// pickTootArt returns the next owl art via the shuffled round-robin
// cycler, or "" if no arts were loaded.
func pickTootArt() string { return tootCycler.Next() }

// Toot is the owl character that delivers update notifications. Unlike
// Toto, Toot has no LLM and no session — it's a one-way courier. The
// updater calls Send() with a fully-composed body (release announcement
// or install confirmation); Toot prepends a random owl art via <pre>
// tags in HTML mode and ships it.
//
// Conversational messages (command replies, error messages) stay on the
// regular bot — Toot exists specifically to mark "this is an update
// event" visually so the user knows what kind of message they're
// reading.
type Toot struct {
	bot telegram.BotClient
}

func newToot(bot telegram.BotClient) *Toot {
	return &Toot{bot: bot}
}

// Send delivers body with a random owl art prepended. The body is
// HTML-escaped so any literal <, >, & survive Telegram's HTML parser.
func (t *Toot) Send(ctx context.Context, chatID int64, body string) error {
	art := pickTootArt()
	escapedBody := html.EscapeString(body)
	var msg string
	if art != "" {
		msg = "<pre>" + html.EscapeString(art) + "</pre>\n\n" + escapedBody
	} else {
		msg = escapedBody
	}
	return t.bot.SendMessageHTML(ctx, chatID, msg)
}
