package sabnzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FallbackSpoofVersion is the SABnzbd version AltMount reports when the latest
// release cannot be determined from GitHub. Bump this occasionally so fresh
// installs (and offline environments) still identify as a recent SABnzbd.
const FallbackSpoofVersion = "5.0.3"

const (
	// spoofRefreshTTL bounds how often the latest version is re-fetched.
	spoofRefreshTTL = 24 * time.Hour
	// spoofReleasesURL returns the latest non-prerelease SABnzbd release.
	spoofReleasesURL = "https://api.github.com/repos/sabnzbd/sabnzbd/releases/latest"
)

// spoofCache holds the SABnzbd version AltMount masquerades as, refreshed from
// GitHub in the background so it tracks newer SABnzbd releases over time.
type spoofCache struct {
	mu         sync.RWMutex
	version    string
	fetchedAt  time.Time
	refreshing bool
	client     *http.Client
}

var defaultSpoofCache = &spoofCache{
	version: FallbackSpoofVersion,
	client:  &http.Client{Timeout: 10 * time.Second},
}

// SpoofVersion returns the SABnzbd version AltMount reports to clients (e.g.
// Sonarr/Radarr). It tracks the latest stable SABnzbd release, refreshed lazily
// in the background at most once per spoofRefreshTTL, and falls back to
// FallbackSpoofVersion when the release lookup fails.
func SpoofVersion() string { return defaultSpoofCache.get() }

// SpoofUserAgent returns a SABnzbd-style User-Agent ("SABnzbd/<version>")
// matching the version reported by SpoofVersion.
func SpoofUserAgent() string { return "SABnzbd/" + SpoofVersion() }

func (c *spoofCache) get() string {
	c.mu.RLock()
	version, fetchedAt, refreshing := c.version, c.fetchedAt, c.refreshing
	c.mu.RUnlock()

	if !refreshing && time.Since(fetchedAt) > spoofRefreshTTL {
		c.mu.Lock()
		// Re-check under the write lock so only one refresher is spawned.
		if !c.refreshing && time.Since(c.fetchedAt) > spoofRefreshTTL {
			c.refreshing = true
			go c.refresh()
		}
		c.mu.Unlock()
	}

	return version
}

func (c *spoofCache) refresh() {
	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	version, err := fetchLatestSABnzbdVersion(ctx, c.client, spoofReleasesURL)
	if err != nil {
		// Keep the current value but record the attempt so we don't retry on
		// every request while the lookup is failing.
		c.mu.Lock()
		c.fetchedAt = time.Now()
		c.mu.Unlock()
		slog.DebugContext(ctx, "Failed to refresh SABnzbd spoof version, keeping current",
			"error", err)
		return
	}

	c.mu.Lock()
	changed := version != c.version
	c.version = version
	c.fetchedAt = time.Now()
	c.mu.Unlock()

	if changed {
		slog.InfoContext(ctx, "Updated SABnzbd spoof version", "version", version)
	}
}

// fetchLatestSABnzbdVersion queries GitHub for the latest stable SABnzbd release.
func fetchLatestSABnzbdVersion(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github API returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}

	version := strings.TrimPrefix(payload.TagName, "v")
	if payload.Prerelease || version == "" {
		return "", fmt.Errorf("no stable release available")
	}

	return version, nil
}
