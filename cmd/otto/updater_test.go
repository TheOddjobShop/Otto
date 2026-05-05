//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAssetForPlatform(t *testing.T) {
	assets := []releaseAsset{
		{Name: "otto-linux-amd64", URL: "https://example.com/linux-amd64"},
		{Name: "otto-linux-arm64", URL: "https://example.com/linux-arm64"},
		{Name: "otto-darwin-arm64", URL: "https://example.com/darwin-arm64"},
	}
	cases := []struct {
		goos, goarch string
		wantURL      string
		wantOK       bool
	}{
		{"linux", "amd64", "https://example.com/linux-amd64", true},
		{"linux", "arm64", "https://example.com/linux-arm64", true},
		{"darwin", "arm64", "https://example.com/darwin-arm64", true},
		{"freebsd", "amd64", "", false},
		{"linux", "386", "", false},
		{"windows", "amd64", "", false},
	}
	for _, c := range cases {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, ok := assetForPlatform(assets, c.goos, c.goarch)
			if ok != c.wantOK {
				t.Fatalf("ok=%v, want %v", ok, c.wantOK)
			}
			if got.URL != c.wantURL {
				t.Errorf("URL=%q, want %q", got.URL, c.wantURL)
			}
		})
	}
}

func TestFetchLatest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"tag_name": "v1.2.3",
			"body": "What's Changed\n* Add /update (#1)\n* Fix denial UX (#2)",
			"assets": [
				{"name": "otto-linux-amd64", "browser_download_url": "https://x/otto-linux-amd64"},
				{"name": "otto-darwin-arm64", "browser_download_url": "https://x/otto-darwin-arm64"}
			]
		}`)
	}))
	defer server.Close()

	u := &updater{
		httpClient:  server.Client(),
		releasesURL: server.URL,
	}
	rel, err := u.fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName=%q, want v1.2.3", rel.TagName)
	}
	if !strings.Contains(rel.Body, "What's Changed") {
		t.Errorf("Body missing patch notes: %q", rel.Body)
	}
	if len(rel.Assets) != 2 {
		t.Fatalf("got %d assets, want 2", len(rel.Assets))
	}
	if rel.Assets[0].Name != "otto-linux-amd64" {
		t.Errorf("Assets[0].Name=%q", rel.Assets[0].Name)
	}
}

func TestFetchLatestNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	u := &updater{httpClient: server.Client(), releasesURL: server.URL}
	_, err := u.fetchLatest(context.Background())
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
}

// newTestUpdater returns an updater wired to a Toot backed by a
// fakeBot + fakeRunner. The fakeRunner emits the canned response
// "TEST_ANNOUNCEMENT_BODY" so tests can assert on what landed in the
// outbound message.
func newTestUpdater(t *testing.T, releasesJSON string) (*updater, *fakeBot, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, releasesJSON)
	}))
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "TEST_ANNOUNCEMENT_BODY")
	u := &updater{
		httpClient:     server.Client(),
		releasesURL:    server.URL,
		currentVersion: "v1.0.0",
		toot:           toot,
		chatID:         42,
	}
	return u, bot, server.Close
}

func TestCheckOnceAnnouncesNewRelease(t *testing.T) {
	json := fmt.Sprintf(`{
		"tag_name": "v1.0.1",
		"body": "What's Changed\n* Add Toot (#3)",
		"assets": [{"name": "otto-%s-%s", "browser_download_url": "https://x/asset"}]
	}`, runtime.GOOS, runtime.GOARCH)
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())

	// Toot's Announce was invoked: bot received one message containing
	// the LLM canned response and the owl banner.
	if len(bot.sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	for _, want := range []string{"TEST_ANNOUNCEMENT_BODY", "TOOT", "<blockquote>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %q", want, msg)
		}
	}

	p := u.Pending()
	if p == nil {
		t.Fatal("Pending() returned nil")
	}
	if p.Tag != "v1.0.1" {
		t.Errorf("Pending.Tag=%q", p.Tag)
	}
	if p.AssetURL != "https://x/asset" {
		t.Errorf("Pending.AssetURL=%q", p.AssetURL)
	}
}

func TestCheckOnceDoesNotAnnounceCurrentVersion(t *testing.T) {
	json := `{"tag_name": "v1.0.0", "assets": []}`
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())
	if len(bot.sent) != 0 {
		t.Errorf("got %d messages, want 0", len(bot.sent))
	}
	if u.Pending() != nil {
		t.Error("Pending() should be nil when tag matches current version")
	}
}

func TestCheckOnceDedupesAnnouncement(t *testing.T) {
	json := fmt.Sprintf(`{
		"tag_name": "v1.0.1",
		"assets": [{"name": "otto-%s-%s", "browser_download_url": "https://x/asset"}]
	}`, runtime.GOOS, runtime.GOARCH)
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())
	u.checkOnce(context.Background())
	u.checkOnce(context.Background())

	if len(bot.sent) != 1 {
		t.Errorf("got %d messages across 3 ticks, want 1", len(bot.sent))
	}
}

func TestCheckOnceSkipsMissingPlatformAsset(t *testing.T) {
	// Release exists but has no asset for the running platform. Toot
	// only narrates installable releases — when the platform doesn't
	// match, we silently skip the announcement so the user isn't
	// pestered about a release they can't apply.
	json := `{
		"tag_name": "v1.0.1",
		"assets": [{"name": "otto-plan9-amd64", "browser_download_url": "https://x/plan9"}]
	}`
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())

	if len(bot.sent) != 0 {
		t.Errorf("got %d messages, want 0 (Toot stays silent on platform mismatch)", len(bot.sent))
	}
	if u.Pending() != nil {
		t.Error("Pending() should be nil when no asset matches platform")
	}
}

func TestCheckOnceFetchError(t *testing.T) {
	// Server returns 500 → fetchLatest errors → checkOnce logs and returns
	// silently. No message, no Pending, no lastAnnounced state set (so a
	// later successful tick still announces).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "TEST")
	u := &updater{
		httpClient:     server.Client(),
		releasesURL:    server.URL,
		currentVersion: "v1.0.0",
		toot:           toot,
		chatID:         42,
	}

	u.checkOnce(context.Background())

	if len(bot.sent) != 0 {
		t.Errorf("got %d messages on fetch error, want 0", len(bot.sent))
	}
	if u.Pending() != nil {
		t.Error("Pending() should be nil on fetch error")
	}
	if u.lastAnnounced != "" {
		t.Errorf("lastAnnounced=%q on fetch error, want empty (so later success can announce)", u.lastAnnounced)
	}
}

func TestInstallSuccess(t *testing.T) {
	// Asset server: returns a small binary blob.
	binaryContents := []byte("#!/bin/sh\necho hello\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(binaryContents)
	}))
	defer server.Close()

	// Stand-in for os.Executable() — point at a temp file we can inspect.
	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "otto")
	if err := os.WriteFile(exePath, []byte("OLD"), 0755); err != nil {
		t.Fatal(err)
	}

	bot := &fakeBot{}
	toot, tootRunner := newTestToot(t, bot, "Installation complete in voice.")
	u := &updater{
		httpClient:     server.Client(),
		toot:           toot,
		chatID:         42,
		currentVersion: "v1.0.0",
		exePath:        func() (string, error) { return exePath, nil },
		exitFunc:       func() {},
	}
	u.pending = &pendingUpdate{
		Tag:       "v1.0.1",
		AssetName: "otto-test",
		AssetURL:  server.URL,
	}

	if err := u.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binaryContents) {
		t.Errorf("binary not swapped: got %q, want %q", got, binaryContents)
	}

	// Confirm goes through Claude — verify the LLM was invoked with the
	// version in its system prompt, and the bot received the LLM's
	// composed message inside Toot's banner.
	if len(tootRunner.called) != 1 {
		t.Fatalf("toot runner called %d times, want 1 (Confirm should invoke Claude)", len(tootRunner.called))
	}
	if !strings.Contains(tootRunner.called[0].AppendSystemPrompt, "v1.0.1") {
		t.Errorf("Confirm system prompt missing version: %q", tootRunner.called[0].AppendSystemPrompt)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	if !strings.Contains(bot.sent[0].text, "TOOT") || !strings.Contains(bot.sent[0].text, "Installation complete") {
		t.Errorf("install confirmation missing TOOT banner or LLM body: %q", bot.sent[0].text)
	}
}

func TestInstallNoPending(t *testing.T) {
	u := &updater{}
	err := u.Install(context.Background())
	if err == nil {
		t.Fatal("expected error when no pending update")
	}
}

func TestInstallDownloadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "otto")
	os.WriteFile(exePath, []byte("OLD"), 0755)

	toot, _ := newTestToot(t, &fakeBot{}, "TEST")
	u := &updater{
		httpClient: server.Client(),
		toot:       toot,
		exePath:    func() (string, error) { return exePath, nil },
		exitFunc:   func() {},
	}
	u.pending = &pendingUpdate{Tag: "v1.0.1", AssetURL: server.URL}

	err := u.Install(context.Background())
	if err == nil {
		t.Fatal("expected download error")
	}
	// Original binary must be untouched.
	got, _ := os.ReadFile(exePath)
	if string(got) != "OLD" {
		t.Errorf("original clobbered on failure: %q", got)
	}
}

func TestInstallConcurrentReturnsBusy(t *testing.T) {
	// First install holds the installing flag while a second one tries.
	// The second must return errInstallInProgress and leave the binary alone.
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate // block until the test releases
		w.Write([]byte("NEW"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "otto")
	if err := os.WriteFile(exePath, []byte("OLD"), 0755); err != nil {
		t.Fatal(err)
	}

	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "TEST")
	u := &updater{
		httpClient: server.Client(),
		toot:       toot,
		chatID:     42,
		exePath:    func() (string, error) { return exePath, nil },
		exitFunc:   func() {},
	}
	u.pending = &pendingUpdate{Tag: "v1.0.1", AssetURL: server.URL}

	// First install runs in background, blocked on the gate.
	firstDone := make(chan error, 1)
	go func() { firstDone <- u.Install(context.Background()) }()

	// Wait until the first install has the installing flag set.
	deadline := time.Now().Add(time.Second)
	for {
		u.mu.Lock()
		busy := u.installing
		u.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first install did not flip installing flag in time")
		}
		time.Sleep(time.Millisecond)
	}

	// Second install must short-circuit with errInstallInProgress.
	if err := u.Install(context.Background()); !errors.Is(err, errInstallInProgress) {
		t.Errorf("second install: err=%v, want errInstallInProgress", err)
	}

	// Release the gate so the first install can finish.
	close(gate)
	if err := <-firstDone; err != nil {
		t.Errorf("first install: %v", err)
	}

	// Pending should be cleared by the successful first install.
	if u.Pending() != nil {
		t.Error("Pending() should be nil after successful install")
	}
}
