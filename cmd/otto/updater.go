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
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
)

// errInstallInProgress is returned by Install when another install is
// already running on this updater. Callers (e.g. the /update command)
// can use it to surface a "busy" message instead of double-swapping
// the binary.
var errInstallInProgress = errors.New("install: already in progress")

const (
	// updateCheckInterval: how often to poll GitHub for a new release. 10 min
	// is ~6 requests/hour — far under GitHub's 60/hour unauthenticated limit —
	// so updates land quickly without risking rate limits.
	updateCheckInterval = 10 * time.Minute
	updateInitialDelay  = 30 * time.Second
	releasesURLDefault  = "https://api.github.com/repos/TheOddjobShop/Otto/releases/latest"
	downloadTimeout     = 5 * time.Minute
	fetchTimeout        = 30 * time.Second
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

	// stateDBPath is the path to Otto's state.db. The updater drops an
	// install-confirm marker in the same directory after a successful
	// binary swap so the next boot can ping the user "back online".
	// Empty disables the marker (tests that don't care about the ping).
	stateDBPath string

	mu            sync.Mutex
	pending       *pendingUpdate
	lastAnnounced string
	installing    bool

	// Hooks for testing — production callers leave these at zero values
	// (nil), which means use defaults: os.Executable + filepath.EvalSymlinks
	// for exePath, syscall.Kill(SIGTERM) for exitFunc, and os.Exit for
	// forceExitFunc (the fallback when SIGTERM-driven shutdown stalls).
	exePath       func() (string, error)
	exitFunc      func()
	forceExitFunc func(int)

	// allowAllAssetURLs disables the GitHub CDN origin allowlist. Set only
	// in tests that use local httptest servers; production leaves it false.
	allowAllAssetURLs bool
}

// forceExitGrace bounds how long Exit waits after sending SIGTERM before
// it force-exits the process. SIGTERM normally triggers a clean shutdown
// via main.go's signal handler, but if an in-flight Claude subprocess
// holds dispatchWG past systemd's TimeoutStopSec, the user sees ~2 min
// of downtime while waiting for SIGKILL. Capping this here keeps /update
// restart latency predictable regardless of dispatch state. Package-level
// so tests can shorten it.
var forceExitGrace = 10 * time.Second

// fetchLatest hits the releases/latest endpoint and parses the response.
// Returns an error on non-200 status or unparseable JSON.
func (u *updater) fetchLatest(ctx context.Context) (release, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u.releasesURL, nil)
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
	// Match exactly "otto-<goos>-<goarch>". Releases now also ship
	// "otto-memory-<goos>-<goarch>", which shares the platform suffix; an
	// open HasSuffix match could pick that MCP-server binary instead of the
	// main otto binary depending on asset ordering, so require the exact name.
	want := fmt.Sprintf("otto-%s-%s", goos, goarch)
	for _, a := range assets {
		if a.Name == want {
			return a, true
		}
	}
	return releaseAsset{}, false
}

// refreshPending hits releases/latest and updates u.pending to reflect
// the newest release that's installable on this platform (nil if we're
// already current or no asset matches). It returns the fetched release
// and whether an installable, newer-than-current release is available.
//
// It deliberately does NOT announce and does NOT touch lastAnnounced —
// the announcement dedupe lives in checkOnce (the autonomous path). The
// chat path (CheckNow) calls this directly so a user-initiated poll can
// update pending state without taking Toot's reply lock: Announce
// re-acquires that lock, and CheckNow runs while reply already holds it,
// so announcing here would self-deadlock.
func (u *updater) refreshPending(ctx context.Context) (release, bool) {
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		log.Printf("updater: %v", err)
		return release{}, false
	}
	// Only treat the fetched tag as an update when it is strictly newer
	// than what we're running. Guards against a re-tagged or manually
	// published older release on releases/latest causing a silent
	// downgrade. If either tag isn't valid semver (semver requires a
	// leading 'v'), fall back to exact-string behavior so well-formed
	// vX.Y.Z builds are unaffected.
	cur, got := u.currentVersion, rel.TagName
	bothValid := semver.IsValid(cur) && semver.IsValid(got)
	// Locally-built binaries are stamped by `git describe`, which on any
	// commit past the last tag (or a dirty tree) yields a prerelease-shaped
	// version like v1.2.3-5-gabc1234 or v1.2.3-dirty. Semver orders every
	// prerelease BELOW its release, so comparing directly would treat the
	// older v1.2.3 GitHub release as an "update" and downgrade the newer
	// local build. Such a build is at least as new as the tag it describes,
	// so compare the fetched tag against the prerelease's base version.
	// Trade-off: a genuinely tagged prerelease (e.g. v1.2.4-rc1) would not
	// auto-upgrade to its final release — this repo never tags prereleases,
	// and CI stamps the exact tag, so only git-describe versions hit this.
	cmpCur := cur
	if bothValid {
		if pre := semver.Prerelease(cur); pre != "" {
			cmpCur = strings.TrimSuffix(cur, pre)
		}
	}
	upToDate := (bothValid && semver.Compare(got, cmpCur) <= 0) || (!bothValid && got == cur)
	if upToDate {
		u.mu.Lock()
		u.pending = nil
		u.mu.Unlock()
		return rel, false
	}
	asset, ok := assetForPlatform(rel.Assets, runtime.GOOS, runtime.GOARCH)
	u.mu.Lock()
	if ok {
		u.pending = &pendingUpdate{
			Tag:       rel.TagName,
			AssetName: asset.Name,
			AssetURL:  asset.URL,
		}
	} else {
		u.pending = nil
		log.Printf("updater: %s available but no asset for %s/%s", rel.TagName, runtime.GOOS, runtime.GOARCH)
	}
	u.mu.Unlock()
	return rel, ok
}

// checkOnce is the autonomous periodic poll (every updateCheckInterval):
// it refreshes pending state
// and, if a new installable release just appeared that we haven't already
// announced, sends a Toot announcement. The lastAnnounced guard keeps a
// flaky network or repeated ticks from re-announcing the same version.
//
// Releases with no asset for the current platform update pending (to nil)
// but are never announced — Toot only narrates installable releases.
func (u *updater) checkOnce(ctx context.Context) {
	rel, ok := u.refreshPending(ctx)
	if !ok {
		return
	}
	u.mu.Lock()
	if rel.TagName == u.lastAnnounced {
		u.mu.Unlock()
		return
	}
	// Record lastAnnounced BEFORE delivering: a flaky network shouldn't
	// make us re-announce the same version on every tick. Cost is one missed
	// announcement until a newer tag ships.
	u.lastAnnounced = rel.TagName
	u.mu.Unlock()

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

// CheckNow runs a synchronous release poll and returns the new pending
// state. Used by Toot's [CHECK_FOR_UPDATE] marker so the user can ask
// "check for updates" in chat instead of waiting for the next periodic tick.
// Returns whatever Pending() resolves to after the check completes
// (nil = up to date).
//
// Unlike checkOnce, this does NOT announce: it runs while Toot's reply
// already holds Toot's lock (Announce re-acquires it → deadlock), and the
// chat reply surfaces its own one-line result. It also leaves
// lastAnnounced untouched, so the next autonomous tick still delivers the
// rich changelog announcement once.
func (u *updater) CheckNow(ctx context.Context) *pendingUpdate {
	u.refreshPending(ctx)
	return u.Pending()
}

// newUpdater constructs an updater that polls the default GitHub URL.
// Pass version="dev" for local builds — Run will short-circuit and not
// poll at all.
func newUpdater(toot *Toot, chatID int64, currentVersion, stateDBPath string) *updater {
	return &updater{
		httpClient:     &http.Client{Timeout: 0}, // per-call context deadlines bound each request (download/fetch); a client-wide Timeout would cap the 5-min download budget
		releasesURL:    releasesURLDefault,
		currentVersion: currentVersion,
		toot:           toot,
		chatID:         chatID,
		stateDBPath:    stateDBPath,
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

	// Validate the asset URL before fetching it. GitHub's browser_download_url
	// values always start with one of these two prefixes. Rejecting anything
	// else stops redirect-based attacks and spoofed JSON responses from
	// causing the bot to fetch and execute a binary from an arbitrary server.
	// allowAllAssetURLs is only true in tests that substitute a local
	// httptest server; production always runs with it false.
	if !u.allowAllAssetURLs && !isAllowedAssetURL(p.AssetURL) {
		return fmt.Errorf("install: asset URL %q does not start with an allowed GitHub prefix", p.AssetURL)
	}

	exe, err := u.resolveExePath()
	if err != nil {
		return fmt.Errorf("install: resolve binary path: %w", err)
	}

	// Match the existing binary's permission bits so the swap doesn't
	// silently widen access (e.g. from 0700 to 0755). Fall back to
	// 0700 — user-only execute — if the stat fails, which still
	// lets the binary run while keeping permissions tight.
	mode := os.FileMode(0700)
	if info, err := os.Stat(exe); err == nil {
		mode = (info.Mode().Perm() &^ 0022) | 0100 // ensure user-execute survives; never carry group/other write
	}

	// tmp lives in the same directory as exe so os.Rename is atomic
	// (same filesystem). os.CreateTemp gives an unpredictable name,
	// eliminating the TOCTOU window that a fixed ".new" suffix leaves
	// open on a multi-user system where another process could substitute
	// a different binary in the gap between WriteFile and Rename.
	//
	// The temp file is created before the download so we can stream
	// the response body directly into it rather than buffering the
	// entire binary (~10 MB) as a []byte. This keeps the transient
	// RSS spike near zero regardless of binary size.
	f, err := os.CreateTemp(filepath.Dir(exe), ".otto-update-*.new")
	if err != nil {
		return fmt.Errorf("install: create temp: %w", err)
	}
	tmp := f.Name()
	written, err := u.download(ctx, p.AssetURL, f)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("install: download: %w", err)
	}
	if written == 0 {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("install: empty asset")
	}
	// Flush the new binary to disk before the rename. Without this, a
	// power loss inside the write-back window can leave exe pointing at
	// a zero-length or partial file that systemd then crash-loops on.
	// Same atomic-write idiom as internal/memory/core.go write().
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("install: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install: close %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install: chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install: rename %s -> %s: %w", tmp, exe, err)
	}
	// Persist the rename itself. Best-effort — the swap already
	// succeeded, so a failed directory fsync must not report the
	// completed install as an error.
	if dir, derr := os.Open(filepath.Dir(exe)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}

	// Clear pending so a stuck Exit (or a deferred restart) can't
	// re-install the same version. Note: the running process keeps
	// executing the OLD code from memory until Exit triggers shutdown
	// and systemd brings up the new binary.
	u.mu.Lock()
	u.pending = nil
	u.mu.Unlock()

	// Drop the boot-confirm marker BEFORE Toot.Confirm and Exit. If the
	// process dies between these calls, the worst case is a missed
	// "Installed v…" message — the marker still tells the new process
	// to ping "back online" once it's up. Errors are logged but
	// non-fatal: the ping is UX polish, not correctness.
	if u.stateDBPath != "" {
		if err := writeInstallConfirm(u.stateDBPath, p.Tag); err != nil {
			log.Printf("install: %v", err)
		}
	}

	if sendErr := u.toot.Confirm(ctx, u.chatID, p.Tag); sendErr != nil {
		log.Printf("install: toot confirm: %v", sendErr)
	}
	return nil
}

// allowedAssetPrefixes is the exhaustive list of URL origins we will fetch
// a binary from. GitHub's browser_download_url field always starts with one
// of these; rejecting anything else prevents a compromised or spoofed JSON
// response from redirecting the download to an attacker-controlled server.
var allowedAssetPrefixes = []string{
	"https://github.com/",
	"https://objects.githubusercontent.com/",
}

// isAllowedAssetURL reports whether url begins with a known GitHub CDN
// prefix. Called before every download to enforce the origin allowlist.
func isAllowedAssetURL(url string) bool {
	for _, prefix := range allowedAssetPrefixes {
		if strings.HasPrefix(url, prefix) {
			return true
		}
	}
	return false
}

// maxAssetBytes caps how large a release asset we'll accept. Paranoia —
// Otto binaries are ~10 MB. Exceeding it is an explicit error (asset
// discarded), never a silent truncation.
const maxAssetBytes = 100 << 20

// download streams a binary asset from url into dst. 5-minute timeout.
// Returns the number of bytes written; a non-nil error means the download
// or write failed (including an asset over maxAssetBytes) and dst should
// be discarded.
func (u *updater) download(ctx context.Context, url string, dst io.Writer) (int64, error) {
	dlCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	// Read one byte past the cap so an oversized asset is detectable:
	// a bare LimitReader would stop at the cap with a nil error, and the
	// truncated (corrupt) binary would get installed over the running one.
	written, err := io.Copy(dst, io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return written, err
	}
	if written > maxAssetBytes {
		return written, fmt.Errorf("asset exceeds %d MB cap", maxAssetBytes>>20)
	}
	return written, nil
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
//
// A fallback goroutine force-exits after forceExitGrace if SIGTERM doesn't
// take the process down in time — covers the case where an in-flight
// Claude subprocess pins WaitDispatches past systemd's TimeoutStopSec
// and the user would otherwise see ~2 min of dead-bot downtime before
// SIGKILL lands. The force-exit short-circuits that worst case.
func (u *updater) Exit() {
	if u.exitFunc != nil {
		u.exitFunc()
	} else {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}
	// Schedule the force-exit fallback. If the SIGTERM path completes
	// shutdown first, this goroutine never gets a chance to run (the
	// process is gone). If shutdown stalls, os.Exit caps the wait.
	go func() {
		time.Sleep(forceExitGrace)
		log.Printf("updater: SIGTERM shutdown exceeded %s, force-exiting", forceExitGrace)
		if u.forceExitFunc != nil {
			u.forceExitFunc(0)
			return
		}
		os.Exit(0)
	}()
}
