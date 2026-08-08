//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"otto/internal/config"
	"otto/internal/store"
)

// `otto say` — how scheduled work reaches Otto.
//
// The inbox has always accepted source="user" rows, but nothing in the tree
// produced one, so the shape was supported and unreachable. This is the
// producer.
//
// It matters because of what the alternative was. SYSTEM.md tells Otto to build
// automations by writing scripts that call the Telegram Bot API directly with
// the token from his config. Those messages arrive on the user's phone but
// never touch Otto: no session, no memory, no turn log. A morning briefing sent
// that way is invisible to `recent_turns` the moment the user replies to it,
// and Otto has no idea he "said" it.
//
// Routing the same message through the inbox makes it an ordinary turn. Otto
// composes it with his full context, it lands in memory like anything else, and
// a reply continues the conversation instead of starting a confusing new one.

const sayUsage = `otto say — hand a message to the running Otto

Usage:
  otto say [flags] <message>
  echo "<message>" | otto say [flags]

Otto receives it as if you had sent it, so he answers with full context and the
exchange lands in memory. Intended for scheduled work — launchd, systemd timers,
cron — where a script wants Otto to actually think about something rather than
just push text at the user.

Flags:
  -to <otto|toto|toot>   who receives it (default otto)
  -config <path>         path to config.toml
  -timeout <duration>    how long to wait for the daemon to pick it up (default 0, don't wait)

Examples:
  otto say "good morning — give me today's brief"
  otto say -to toot "what version are we on?"
`

// sayMaxBytes bounds a single message. Generous for prose, small enough that a
// runaway script cannot push a megabyte into the queue.
const sayMaxBytes = 64 * 1024

// runSay enqueues a message for the running daemon.
func runSay(args []string) int {
	fs := flag.NewFlagSet("say", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, sayUsage) }

	target := fs.String("to", "otto", "recipient: otto, toto or toot")
	configPath := fs.String("config", defaultConfigPath(), "path to config.toml")
	timeout := fs.Duration("timeout", 0, "wait this long for the daemon to pick the message up")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	body, err := sayBody(fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "otto say: %v\n\n%s", err, sayUsage)
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "otto say: %v\n", err)
		return 1
	}

	st, err := store.Open(cfg.StateDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "otto say: open state db: %v\n", err)
		return 1
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// source="user" rather than "agent": this is the user's own message taking
	// a detour through the queue, so the dispatcher hands it straight to
	// handleMessage with no BUS CONTEXT block. Telling Otto he is mid-chain
	// when a cron job pinged him would simply be false.
	id, err := st.Enqueue(ctx, *target, "user", "", body, 0)
	if err != nil {
		if errors.Is(err, store.ErrBusHopExceeded) {
			// Unreachable at hop 0, but reported precisely rather than as a
			// generic failure if the constant ever changes.
			fmt.Fprintf(os.Stderr, "otto say: refused by the hop cap\n")
			return 1
		}
		fmt.Fprintf(os.Stderr, "otto say: %v\n", err)
		return 1
	}

	if *timeout <= 0 {
		// The common case: fire and forget. A scheduled script has nothing to
		// do with the answer, which goes to Telegram.
		fmt.Printf("queued for %s (id %d)\n", *target, id)
		return 0
	}
	return waitForDelivery(st, id, *timeout, *target)
}

// waitForDelivery polls until the daemon marks the row delivered.
//
// Useful in a script that wants to know Otto is actually running before
// reporting success — the enqueue itself succeeds whether or not anything is
// listening, since the queue is just a table.
func waitForDelivery(st *store.Store, id int64, timeout time.Duration, target string) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		queued, _, err := st.InboxDepth(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "otto say: %v\n", err)
			return 1
		}
		if queued == 0 {
			fmt.Printf("delivered to %s (id %d)\n", target, id)
			return 0
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr,
		"otto say: queued (id %d) but not picked up within %s — is the daemon running?\n"+
			"  systemctl --user status otto\n", id, timeout)
	return 1
}

// sayBody resolves the message from arguments or stdin.
//
// Reading stdin when no arguments are given is what makes this composable in a
// shell pipeline, which is exactly where scheduled scripts live.
func sayBody(args []string) (string, error) {
	if len(args) > 0 {
		body := strings.TrimSpace(strings.Join(args, " "))
		if body == "" {
			return "", fmt.Errorf("message is empty")
		}
		if len(body) > sayMaxBytes {
			return "", fmt.Errorf("message is %d bytes, over the %d-byte cap", len(body), sayMaxBytes)
		}
		return body, nil
	}

	info, err := os.Stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("no message given")
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		// A terminal, not a pipe — waiting for input here would look like a hang.
		return "", fmt.Errorf("no message given")
	}
	buf := make([]byte, sayMaxBytes+1)
	n, _ := os.Stdin.Read(buf)
	body := strings.TrimSpace(string(buf[:n]))
	if body == "" {
		return "", fmt.Errorf("message is empty")
	}
	if n > sayMaxBytes {
		return "", fmt.Errorf("message exceeds the %d-byte cap", sayMaxBytes)
	}
	return body, nil
}

// sayStateDir is a small helper mirroring voiceStateDir, used by tests.
func sayStateDir(cfg *config.Config) string { return filepath.Dir(cfg.StateDBPath) }
