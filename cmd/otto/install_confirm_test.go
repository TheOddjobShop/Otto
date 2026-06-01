//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallConfirmRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "state.db")

	// Marker absent → ok=false, no error (this is the cold-start path).
	if _, ok, err := readInstallConfirm(stateDB); err != nil || ok {
		t.Fatalf("read with no marker: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := writeInstallConfirm(stateDB, "v1.2.3"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Confirm file actually landed next to state.db.
	wantPath := filepath.Join(dir, installConfirmFile)
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("marker file missing at %s: %v", wantPath, err)
	}

	c, ok, err := readInstallConfirm(stateDB)
	if err != nil || !ok {
		t.Fatalf("read after write: ok=%v err=%v", ok, err)
	}
	if c.InstalledTag != "v1.2.3" {
		t.Errorf("InstalledTag=%q, want v1.2.3", c.InstalledTag)
	}
	if c.TS == "" {
		t.Error("TS unset")
	}

	if err := removeInstallConfirm(stateDB); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Errorf("marker still present after remove: err=%v", err)
	}
	// Idempotent remove — missing file is not an error.
	if err := removeInstallConfirm(stateDB); err != nil {
		t.Errorf("remove on absent: %v", err)
	}
}

func TestReadInstallConfirmMalformed(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "state.db")
	path := filepath.Join(dir, installConfirmFile)
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readInstallConfirm(stateDB); err == nil {
		t.Error("expected error parsing malformed marker")
	}
}

func TestMaybeSendBootConfirmAbsent(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "state.db")
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "")

	got := maybeSendBootConfirm(context.Background(), toot, 42, stateDB, "v1.0.0", time.Millisecond)
	if got != bootConfirmNone {
		t.Errorf("action=%v, want bootConfirmNone", got)
	}
	if len(bot.sent) != 0 {
		t.Errorf("bot.sent=%d, want 0", len(bot.sent))
	}
}

func TestMaybeSendBootConfirmMatchingVersionPings(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "state.db")
	if err := writeInstallConfirm(stateDB, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "")

	got := maybeSendBootConfirm(context.Background(), toot, 42, stateDB, "v1.2.3", time.Millisecond)
	if got != bootConfirmPinged {
		t.Errorf("action=%v, want bootConfirmPinged", got)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	for _, want := range []string{"v1.2.3", "back online", "TOOT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %q", want, msg)
		}
	}
	// Marker must be deleted after a successful ping so subsequent boots
	// don't re-ping the same install.
	if _, ok, _ := readInstallConfirm(stateDB); ok {
		t.Error("marker still present after successful ping")
	}
}

func TestMaybeSendBootConfirmStaleMismatchDiscards(t *testing.T) {
	dir := t.TempDir()
	stateDB := filepath.Join(dir, "state.db")
	if err := writeInstallConfirm(stateDB, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "")

	// Running version doesn't match the marker — swap didn't take.
	got := maybeSendBootConfirm(context.Background(), toot, 42, stateDB, "v0.9.0", time.Millisecond)
	if got != bootConfirmStale {
		t.Errorf("action=%v, want bootConfirmStale", got)
	}
	if len(bot.sent) != 0 {
		t.Errorf("stale marker should not ping: bot.sent=%d", len(bot.sent))
	}
	if _, ok, _ := readInstallConfirm(stateDB); ok {
		t.Error("stale marker not cleaned up")
	}
}

func TestInstallConfirmPath(t *testing.T) {
	got := installConfirmPath("/var/lib/otto/state.db")
	want := "/var/lib/otto/" + installConfirmFile
	if got != want {
		t.Errorf("installConfirmPath=%q, want %q", got, want)
	}
}
