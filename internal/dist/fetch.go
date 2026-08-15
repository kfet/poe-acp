package dist

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// DefaultLockURL is where a host pulls the fleet-wide lock from. The raw
// repo URL is deliberate: it needs no git, no credentials and no release
// step — editing dist.lock on main IS the fleet-wide rollout.
const DefaultLockURL = "https://raw.githubusercontent.com/kfet/poe-acp/main/dist.lock"

// cacheFile is the ETag cache: the last lock body we fetched plus the
// ETag to revalidate it with. Written atomically (temp + rename).
type cacheFile struct {
	ETag string          `json:"etag,omitempty"`
	Body json.RawMessage `json:"body"`
}

// maxLockBytes bounds what we will read from the lock URL. The lock is a
// few hundred bytes; anything larger is a wrong URL or a hostile server.
const maxLockBytes = 64 << 10

// fetchLock GETs the lock conditionally: with a cached ETag a 304 costs
// one round trip and no parse of new bytes. Returns the lock, its ETag,
// and whether the server actually sent a new body.
func fetchLock(c *http.Client, url, cachePath string) (Lock, string, error) {
	var lock Lock
	cached, _ := readCache(cachePath)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return lock, "", err
	}
	if cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := c.Do(req)
	if err != nil {
		return lock, "", fmt.Errorf("fetch lock: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if len(cached.Body) == 0 {
			return lock, "", fmt.Errorf("fetch lock: 304 but no cached body")
		}
		err := json.Unmarshal(cached.Body, &lock)
		return lock, cached.ETag, err
	case http.StatusOK:
	default:
		return lock, "", fmt.Errorf("fetch lock: GET %s: %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLockBytes))
	if err != nil {
		return lock, "", fmt.Errorf("fetch lock: %w", err)
	}
	if err := json.Unmarshal(body, &lock); err != nil {
		return lock, "", fmt.Errorf("parse lock: %w", err)
	}
	etag := resp.Header.Get("ETag")
	if err := writeJSON(cachePath, cacheFile{ETag: etag, Body: body}); err != nil {
		return lock, etag, fmt.Errorf("cache lock: %w", err)
	}
	return lock, etag, nil
}

func readCache(path string) (cacheFile, error) {
	var cf cacheFile
	b, err := os.ReadFile(path)
	if err != nil {
		return cf, err
	}
	return cf, json.Unmarshal(b, &cf)
}

// writeJSON marshals v and writes it atomically: sibling temp file then
// rename. Every write this package does goes through here, so a crash
// mid-write can never leave a half file behind.
func writeJSON(path string, v any) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, mustJSON(v), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
