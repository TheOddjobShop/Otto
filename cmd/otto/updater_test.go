//go:build unix

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
