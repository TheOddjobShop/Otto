//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// bootConfirmGrace is how long maybeSendBootConfirm waits after boot
// before sending the back-online ping, so the message lands once the
// process has settled rather than racing the first log lines. Package
// variable so tests can shorten it.
var bootConfirmGrace = 5 * time.Second

// installConfirmFile is the name of the marker file the updater drops
// next to state.db after a successful install. Main reads it on the
// next boot and (if the running binary's version matches) sends a
// "back online" ping via Toot so the user knows the restart cycle
// closed cleanly.
const installConfirmFile = "install-confirm.json"

// installConfirm records that an install completed at `TS` for
// `InstalledTag`. The next boot reads this file, compares
// InstalledTag against the running binary's stamped version, and
// either pings the user (match) or warns and discards (mismatch).
type installConfirm struct {
	InstalledTag string `json:"installed_tag"`
	TS           string `json:"ts"`
}

// installConfirmPath returns the absolute path to the install-confirm
// marker. It's written next to state.db so the marker shares lifecycle
// (and directory permissions) with Otto's other persistent state. The
// caller passes cfg.StateDBPath and we derive the directory.
func installConfirmPath(stateDBPath string) string {
	return filepath.Join(filepath.Dir(stateDBPath), installConfirmFile)
}

// writeInstallConfirm drops the marker for `tag` at the standard path.
// Called from the install goroutine after the binary swap succeeds and
// before Exit, so the marker is in place by the time the new process
// boots. Best-effort: a write failure is logged by the caller but not
// fatal — the missed ping is a UX nicety, not a correctness invariant.
func writeInstallConfirm(stateDBPath, tag string) error {
	c := installConfirm{
		InstalledTag: tag,
		TS:           time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("install-confirm marshal: %w", err)
	}
	path := installConfirmPath(stateDBPath)
	// 0600 so only the otto user can read the marker (matches the
	// permission model on state.db and the memory files).
	if err := os.WriteFile(path, body, 0600); err != nil {
		return fmt.Errorf("install-confirm write %s: %w", path, err)
	}
	return nil
}

// readInstallConfirm returns the marker contents and an ok=true, or
// ok=false if the marker is absent. A malformed marker returns an
// error so the caller can decide whether to log-and-discard. We do
// not delete on read — main does the deletion explicitly after acting
// on the contents, so a crash mid-ping doesn't lose the marker.
func readInstallConfirm(stateDBPath string) (installConfirm, bool, error) {
	path := installConfirmPath(stateDBPath)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return installConfirm{}, false, nil
		}
		return installConfirm{}, false, fmt.Errorf("install-confirm read %s: %w", path, err)
	}
	var c installConfirm
	if err := json.Unmarshal(body, &c); err != nil {
		return installConfirm{}, false, fmt.Errorf("install-confirm parse %s: %w", path, err)
	}
	return c, true, nil
}

// maybeSendBootConfirm checks for an install-confirm marker and, if
// present and matching the running binary's version, pings the user via
// Toot. The marker is deleted after processing regardless of whether
// the version matched — a stale marker (binary swap that didn't take)
// gets logged and discarded so it can't loop.
//
// Returns the action taken so tests can assert without touching Toot.
type bootConfirmAction int

const (
	bootConfirmNone     bootConfirmAction = iota // no marker present
	bootConfirmPinged                            // marker matched, ping sent
	bootConfirmStale                             // marker mismatched, discarded
	bootConfirmReadFail                          // marker present but unreadable
)

func maybeSendBootConfirm(ctx context.Context, toot *Toot, chatID int64, stateDBPath, version string, grace time.Duration) bootConfirmAction {
	if stateDBPath == "" {
		return bootConfirmNone
	}
	c, ok, err := readInstallConfirm(stateDBPath)
	if err != nil {
		log.Printf("boot-confirm: %v", err)
		// Drop the unreadable marker so it doesn't loop on every boot.
		_ = removeInstallConfirm(stateDBPath)
		return bootConfirmReadFail
	}
	if !ok {
		return bootConfirmNone
	}
	if c.InstalledTag != version {
		// Mismatch means the swap didn't take (running binary is older
		// than the install marker says) or some other inconsistency.
		// Don't ping — but do remove the marker so we don't keep
		// flagging it every boot.
		log.Printf("boot-confirm: stale marker installed_tag=%s running=%s; discarding", c.InstalledTag, version)
		_ = removeInstallConfirm(stateDBPath)
		return bootConfirmStale
	}
	// Match: hold for grace so the bot has time to settle, then ping.
	select {
	case <-time.After(grace):
	case <-ctx.Done():
		return bootConfirmNone
	}
	body := fmt.Sprintf("Update to %s installed. I'm back online.", c.InstalledTag)
	if err := toot.SystemMessage(ctx, chatID, body); err != nil {
		log.Printf("boot-confirm: send: %v", err)
	}
	if err := removeInstallConfirm(stateDBPath); err != nil {
		log.Printf("boot-confirm: %v", err)
	}
	return bootConfirmPinged
}

// removeInstallConfirm deletes the marker. Called after main has
// processed it (either delivered the back-online ping or logged a
// stale-marker warning). Missing-file is not an error.
func removeInstallConfirm(stateDBPath string) error {
	path := installConfirmPath(stateDBPath)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("install-confirm remove %s: %w", path, err)
	}
	return nil
}
