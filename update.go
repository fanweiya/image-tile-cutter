package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/egoist/mygo"
)

var downloadMirrors = []string{
	"",
	"https://ghfast.top/",
	"https://gh-proxy.com/",
	"https://ghproxy.net/",
	"https://ghproxy.cfd/",
	"https://gh-proxy.net/",
}

var apiMirrors = []string{
	"",
	"https://gh-proxy.com/",
	"https://ghproxy.cfd/",
	"https://gh-proxy.net/",
}

const (
	updateRepo   = "fanweiya/image-tile-cutter"
	updateTarget = "windows-amd64"
)

var updateHTTP = &http.Client{Timeout: 30 * time.Minute}

func openThrough(ctx context.Context, prefixes []string, raw string) (io.ReadCloser, error) {
	var errs []error
	for _, prefix := range prefixes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, prefix+raw, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", mirrorName(prefix), err))
			continue
		}
		req.Header.Set("User-Agent", "ImageTileCutter-Updater")
		resp, err := updateHTTP.Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", mirrorName(prefix), err))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			errs = append(errs, fmt.Errorf("%s: %s", mirrorName(prefix), resp.Status))
			continue
		}
		return resp.Body, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("no download mirror available")
	}
	return nil, fmt.Errorf("all mirrors failed: %w", errors.Join(errs...))
}

func mirrorName(prefix string) string {
	if prefix == "" {
		return "direct"
	}
	return strings.TrimSuffix(strings.TrimPrefix(prefix, "https://"), "/")
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type releaseInfo struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

func manifestURL(ctx context.Context) (string, error) {
	api := "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	body, err := openThrough(ctx, apiMirrors, api)
	if err != nil {
		return "", err
	}
	defer body.Close()

	var rel releaseInfo
	if err := json.NewDecoder(body).Decode(&rel); err != nil {
		return "", fmt.Errorf("failed to decode release info: %w", err)
	}
	if len(rel.Assets) == 0 {
		return "", fmt.Errorf("release %s has no downloadable assets", rel.Tag)
	}
	want := "update-" + updateTarget + ".json"
	for _, a := range rel.Assets {
		if a.Name == want {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s is missing %s", rel.Tag, want)
}

type githubSource struct{}

func (githubSource) Manifest(ctx context.Context, _ bool) (io.ReadCloser, error) {
	url, err := manifestURL(ctx)
	if err != nil {
		return nil, err
	}
	return openThrough(ctx, downloadMirrors, url)
}

func (githubSource) Open(ctx context.Context, url string) (io.ReadCloser, error) {
	return openThrough(ctx, downloadMirrors, url)
}

func useMirroredUpdates() {
	mygo.Updater.SetSource(githubSource{})
}
