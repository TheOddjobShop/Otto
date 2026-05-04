//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
