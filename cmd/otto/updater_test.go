//go:build unix

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// newTestUpdater returns an updater whose Toot is wired to a fakeBot.
// Callers read fakeBot.sent to inspect what Toot delivered. (Toot's
// SendMessageHTML appends to the same .sent slice as plain SendMessage,
// so test assertions just look at .sent[i].text.)
func newTestUpdater(t *testing.T, releasesJSON string) (*updater, *fakeBot, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, releasesJSON)
	}))
	bot := &fakeBot{}
	u := &updater{
		httpClient:     server.Client(),
		releasesURL:    server.URL,
		currentVersion: "v1.0.0",
		toot:           newToot(bot),
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

	if len(bot.sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if !strings.Contains(msg, "v1.0.1") {
		t.Errorf("missing tag in message: %q", msg)
	}
	if !strings.Contains(msg, "/update") {
		t.Errorf("missing /update hint: %q", msg)
	}
	if !strings.Contains(msg, "What&#39;s Changed") && !strings.Contains(msg, "Add Toot") {
		// Body is HTML-escaped by Toot.Send. We accept either the escaped
		// apostrophe or the unescaped tail as evidence that body was
		// included.
		t.Errorf("missing patch notes in message: %q", msg)
	}
	if !strings.Contains(msg, "<pre>") {
		t.Errorf("missing owl <pre> wrapper (Toot didn't deliver?): %q", msg)
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
	// Release exists but has no asset for the running platform.
	json := `{
		"tag_name": "v1.0.1",
		"assets": [{"name": "otto-plan9-amd64", "browser_download_url": "https://x/plan9"}]
	}`
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())

	// We still announce so the user knows an update exists, but Pending
	// is nil so /update will explain the platform mismatch.
	if len(bot.sent) != 1 {
		t.Errorf("got %d messages, want 1", len(bot.sent))
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
	u := &updater{
		httpClient:     server.Client(),
		releasesURL:    server.URL,
		currentVersion: "v1.0.0",
		toot:           newToot(bot),
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

func TestBuildAnnounceMessage(t *testing.T) {
	cases := []struct {
		name             string
		current, newTag  string
		body             string
		hasPlatformAsset bool
		wantContains     []string
		wantNotContains  []string
	}{
		{
			name:    "with body and matching platform",
			current: "v1.0.0", newTag: "v1.0.1",
			body:             "What's Changed\n* Add /update",
			hasPlatformAsset: true,
			wantContains:     []string{"v1.0.0 → v1.0.1", "What's Changed", "* Add /update", "Reply /update to install."},
		},
		{
			name:    "empty body collapses to no double-blank",
			current: "v1.0.0", newTag: "v1.0.1",
			body:             "",
			hasPlatformAsset: true,
			wantContains:     []string{"v1.0.0 → v1.0.1\n\nReply /update to install."},
			wantNotContains:  []string{"\n\n\n"},
		},
		{
			name:    "trailing whitespace in body is trimmed",
			current: "v1.0.0", newTag: "v1.0.1",
			body:             "Notes here\n\n   \t\n",
			hasPlatformAsset: true,
			wantContains:     []string{"Notes here\n\nReply /update to install."},
			wantNotContains:  []string{"   \t\n\n\nReply"},
		},
		{
			name:    "missing-platform footer mentions GOOS/GOARCH",
			current: "v1.0.0", newTag: "v1.0.1",
			body:             "",
			hasPlatformAsset: false,
			wantContains:     []string{runtime.GOOS, runtime.GOARCH, "Build manually"},
			wantNotContains:  []string{"Reply /update"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildAnnounceMessage(c.current, c.newTag, c.body, c.hasPlatformAsset)
			for _, want := range c.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, unwant := range c.wantNotContains {
				if strings.Contains(got, unwant) {
					t.Errorf("unexpected %q in:\n%s", unwant, got)
				}
			}
		})
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
	u := &updater{
		httpClient:     server.Client(),
		toot:           newToot(bot),
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

	// Toot delivered one "Installed" confirmation.
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "v1.0.1") {
		t.Errorf("messages=%v", bot.sent)
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

	u := &updater{
		httpClient: server.Client(),
		toot:       newToot(&fakeBot{}),
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
