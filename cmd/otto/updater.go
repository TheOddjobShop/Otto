//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"
)

const (
	updateCheckInterval = 1 * time.Hour
	updateInitialDelay  = 30 * time.Second
	releasesURLDefault  = "https://api.github.com/repos/TheOddjobShop/Otto/releases/latest"
	downloadTimeout     = 5 * time.Minute
)

// releaseAsset is one entry from a GitHub Release's assets list. The
// updater fetches the latest release JSON, picks the asset matching the
// running binary's GOOS/GOARCH, and downloads it on /update.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// release is the slice of GitHub's release JSON we care about. Body
// is the auto-generated changelog (when our workflow sets
// generate_release_notes: true) and Toot includes it in the announcement
// as patch notes for the user.
type release struct {
	TagName string         `json:"tag_name"`
	Body    string         `json:"body"`
	Assets  []releaseAsset `json:"assets"`
}

// pendingUpdate holds the most recent detected available release that
// matches the current platform. /update reads this; the poller writes it.
type pendingUpdate struct {
	Tag       string
	AssetName string
	AssetURL  string
}

// updater polls GitHub Releases for new versions of Otto, notifies the
// allowlisted user via Toot when one is detected, and applies it on
// /update.
//
// httpClient and releasesURL are settable from the same package so
// tests can substitute httptest servers.
type updater struct {
	httpClient     *http.Client
	releasesURL    string
	currentVersion string
	toot           *Toot
	chatID         int64

	mu            sync.Mutex
	pending       *pendingUpdate
	lastAnnounced string
}

// fetchLatest hits the releases/latest endpoint and parses the response.
// Returns an error on non-200 status or unparseable JSON.
func (u *updater) fetchLatest(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.releasesURL, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("updater: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("updater: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB safety cap
	if err != nil {
		return release{}, fmt.Errorf("updater: read: %w", err)
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return release{}, fmt.Errorf("updater: parse: %w", err)
	}
	return rel, nil
}

// assetForPlatform finds the asset whose name ends in -<goos>-<goarch>
// (e.g. otto-linux-amd64). Returns ok=false if no such asset exists.
// CI publishes one binary per supported platform with names matching
// this convention; mismatch means the platform isn't supported by this
// release.
func assetForPlatform(assets []releaseAsset, goos, goarch string) (releaseAsset, bool) {
	suffix := "-" + goos + "-" + goarch
	for _, a := range assets {
		if len(a.Name) > len(suffix) && a.Name[len(a.Name)-len(suffix):] == suffix {
			return a, true
		}
	}
	return releaseAsset{}, false
}

// checkOnce hits releases/latest and, if the latest tag differs from
// both the current version and the previously-announced tag, sends a
// Toot announcement and records the pending install.
//
// If the release exists but has no asset for the current platform, we
// still announce (so the user knows an update is out) but record no
// pending — /update will explain the mismatch.
func (u *updater) checkOnce(ctx context.Context) {
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		log.Printf("updater: %v", err)
		return
	}
	if rel.TagName == u.currentVersion {
		return
	}
	u.mu.Lock()
	if rel.TagName == u.lastAnnounced {
		u.mu.Unlock()
		return
	}
	asset, ok := assetForPlatform(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if ok {
		u.pending = &pendingUpdate{
			Tag:       rel.TagName,
			AssetName: asset.Name,
			AssetURL:  asset.URL,
		}
	} else {
		u.pending = nil
	}
	// Record lastAnnounced BEFORE Send: a flaky network shouldn't make us
	// re-announce the same version every hour. Cost is one missed
	// announcement (logged) until a newer tag ships.
	u.lastAnnounced = rel.TagName
	u.mu.Unlock()

	msg := buildAnnounceMessage(u.currentVersion, rel.TagName, rel.Body, ok)
	if err := u.toot.Send(ctx, u.chatID, msg); err != nil {
		log.Printf("updater: toot send: %v", err)
	}
}

// Pending returns the current pending install, or nil if none.
// Safe to call from any goroutine.
func (u *updater) Pending() *pendingUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pending
}

// buildAnnounceMessage composes Toot's announcement body. Patch notes
// from the release are included verbatim when present; trailing
// whitespace is trimmed so we don't leave a dangling blank line before
// the "Reply /update" hint.
func buildAnnounceMessage(currentVersion, newTag, body string, hasPlatformAsset bool) string {
	header := fmt.Sprintf("%s → %s", currentVersion, newTag)
	footer := "Reply /update to install."
	if !hasPlatformAsset {
		footer = fmt.Sprintf(
			"No binary for %s/%s in this release. Build manually or wait for the next one.",
			runtime.GOOS, runtime.GOARCH,
		)
	}
	if body = trimRight(body); body == "" {
		return header + "\n\n" + footer
	}
	return header + "\n\n" + body + "\n\n" + footer
}

// trimRight strips trailing whitespace including blank lines.
func trimRight(s string) string {
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
