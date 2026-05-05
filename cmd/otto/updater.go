//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// errInstallInProgress is returned by Install when another install is
// already running on this updater. Callers (e.g. the /update command)
// can use it to surface a "busy" message instead of double-swapping
// the binary.
var errInstallInProgress = errors.New("install: already in progress")

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
	installing    bool

	// Hooks for testing — production callers leave these at zero values
	// (nil), which means use defaults: os.Executable + filepath.EvalSymlinks
	// for exePath, and syscall.Kill(SIGTERM) for exitFunc.
	exePath  func() (string, error)
	exitFunc func()
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
	// Record lastAnnounced BEFORE the announcement is delivered: a flaky
	// network shouldn't make us re-announce the same version every hour.
	// Cost is one missed announcement (logged) until a newer tag ships.
	u.lastAnnounced = rel.TagName
	u.mu.Unlock()

	if !ok {
		// Platform mismatch: skip Toot's LLM call and send a short static
		// note via the regular bot path. (We still set lastAnnounced so
		// we don't repeat it.) Toot only narrates installable releases.
		log.Printf("updater: %s available but no asset for %s/%s; skipping toot announce", rel.TagName, runtime.GOOS, runtime.GOARCH)
		return
	}
	if err := u.toot.Announce(ctx, u.chatID, u.currentVersion, rel.TagName, rel.Body); err != nil {
		log.Printf("updater: toot announce: %v", err)
	}
}

// Pending returns the current pending install, or nil if none.
// Safe to call from any goroutine.
func (u *updater) Pending() *pendingUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pending
}

// newUpdater constructs an updater that polls the default GitHub URL.
// Pass version="dev" for local builds — Run will short-circuit and not
// poll at all.
func newUpdater(toot *Toot, chatID int64, currentVersion string) *updater {
	return &updater{
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		releasesURL:    releasesURLDefault,
		currentVersion: currentVersion,
		toot:           toot,
		chatID:         chatID,
	}
}

// Run polls for updates until ctx is cancelled. No-op when the binary
// was built without a version tag (currentVersion == "dev").
func (u *updater) Run(ctx context.Context) {
	if u.currentVersion == "dev" {
		log.Printf("updater: version=dev, skipping poll loop")
		return
	}
	log.Printf("updater: starting (interval=%s, initial=%s, repo=%s)",
		updateCheckInterval, updateInitialDelay, u.releasesURL)
	select {
	case <-time.After(updateInitialDelay):
	case <-ctx.Done():
		return
	}
	u.checkOnce(ctx)
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			u.checkOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Install downloads the pending update and atomically replaces the
// running binary. The exit hook is NOT called from Install — callers
// (the /update command) invoke it after Install returns successfully
// so the post-install message lands first.
//
// Returns an error if there's no pending update, the download fails,
// or the binary swap fails. On any error, the original binary is left
// intact.
func (u *updater) Install(ctx context.Context) error {
	u.mu.Lock()
	if u.installing {
		u.mu.Unlock()
		return errInstallInProgress
	}
	p := u.pending
	if p == nil {
		u.mu.Unlock()
		return fmt.Errorf("install: no pending update")
	}
	u.installing = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.installing = false
		u.mu.Unlock()
	}()

	body, err := u.download(ctx, p.AssetURL)
	if err != nil {
		return fmt.Errorf("install: download: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("install: empty asset")
	}

	exe, err := u.resolveExePath()
	if err != nil {
		return fmt.Errorf("install: resolve binary path: %w", err)
	}

	// tmp lives in the same directory as exe so os.Rename is atomic
	// (same filesystem). Don't move this to /tmp without revisiting.
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, body, 0755); err != nil {
		return fmt.Errorf("install: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install: rename %s -> %s: %w", tmp, exe, err)
	}

	// Clear pending so a stuck Exit (or a deferred restart) can't
	// re-install the same version. Note: the running process keeps
	// executing the OLD code from memory until Exit triggers shutdown
	// and systemd brings up the new binary.
	u.mu.Lock()
	u.pending = nil
	u.mu.Unlock()

	if sendErr := u.toot.Confirm(ctx, u.chatID, p.Tag); sendErr != nil {
		log.Printf("install: toot confirm: %v", sendErr)
	}
	return nil
}

// download fetches a binary asset into memory. 5-minute timeout. The
// 100MB cap is paranoia — Otto binaries are ~10MB.
func (u *updater) download(ctx context.Context, url string) ([]byte, error) {
	dlCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 100*1024*1024))
}

// resolveExePath returns the absolute, symlink-resolved path of the
// current process's binary, or whatever the test hook returns.
func (u *updater) resolveExePath() (string, error) {
	if u.exePath != nil {
		return u.exePath()
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Exit triggers a clean process shutdown via SIGTERM (or the test hook).
// systemd's Restart=always brings Otto back on the new binary.
func (u *updater) Exit() {
	if u.exitFunc != nil {
		u.exitFunc()
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
