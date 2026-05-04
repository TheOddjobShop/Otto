//go:build unix

package main

import "testing"

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
