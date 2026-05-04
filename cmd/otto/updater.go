//go:build unix

package main

// releaseAsset is one entry from a GitHub Release's assets list. The
// updater fetches the latest release JSON, picks the asset matching the
// running binary's GOOS/GOARCH, and downloads it on /update.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
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
