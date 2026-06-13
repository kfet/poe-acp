// Package selfupdate implements `poe-acp update`: download the latest
// release matching the current GOOS/GOARCH from GitHub Releases, verify
// its sha256 against checksums.txt, and atomically replace the running
// binary.
//
// Design notes:
//
//   - poe-acp release assets are RAW binaries (goreleaser archives
//     formats=[binary]), named "poe-acp-<os>-<arch>" — there is no
//     tarball to extract, so the downloaded asset is the binary itself.
//   - The download is staged in a temp dir next to the running binary
//     (same directory → same filesystem) so the final os.Rename is
//     atomic. This is the ETXTBSY-safe swap: you cannot cp/truncate a
//     running executable in place, but renaming a sibling over it
//     replaces the directory entry while the live process keeps its old
//     inode mapped until it restarts.
//   - Self-update is refused when the binary lives under a path managed
//     by a package manager (Homebrew, linuxbrew, system dirs). The user
//     is told to use the package manager instead.
//   - No third-party dependencies. Pure stdlib.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultRepo is the github "owner/name" we update from.
const DefaultRepo = "kfet/poe-acp"

// Options configures a single update run.
type Options struct {
	Repo       string // owner/repo (default kfet/poe-acp)
	Version    string // explicit tag like "v0.27.0"; "" → latest
	CheckOnly  bool   // resolve + report, do not download or replace
	HTTPClient *http.Client
	Stdout     io.Writer
	Stderr     io.Writer
	// ExecPath overrides os.Executable() — for tests.
	ExecPath func() (string, error)
}

// managedPaths are prefixes we refuse to self-update under. Each entry
// must be matched as a path prefix on the resolved (symlink-followed)
// executable path.
var managedPaths = []string{
	"/opt/homebrew/",
	"/usr/local/Cellar/",
	"/usr/local/Homebrew/",
	"/home/linuxbrew/",
	"/usr/bin/",
	"/usr/sbin/",
}

// Result reports what an update run did, for the caller (main) to decide
// follow-up actions such as restarting the supervisor.
type Result struct {
	Current  string // current version (with leading v)
	Target   string // resolved target version (with leading v)
	Updated  bool   // true iff the binary was actually replaced
	ExecPath string // resolved path of the (replaced) binary
}

// Run performs the update according to opts. Returns a Result describing
// the outcome and a non-nil error if the update failed. On "already up
// to date" or CheckOnly it returns Updated=false, err=nil.
func Run(currentVersion string, opts Options) (Result, error) {
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.ExecPath == nil {
		opts.ExecPath = os.Executable
	}

	var res Result

	exe, err := opts.ExecPath()
	if err != nil {
		return res, fmt.Errorf("locate self: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	res.ExecPath = resolved
	for _, p := range managedPaths {
		if strings.HasPrefix(resolved, p) {
			return res, fmt.Errorf("refusing to self-update: %s is managed by a package manager; use the package manager (e.g. `brew upgrade poe-acp`) instead", resolved)
		}
	}

	// 1. Resolve target version.
	target := opts.Version
	if target == "" {
		target, err = resolveLatest(opts.HTTPClient, opts.Repo)
		if err != nil {
			return res, fmt.Errorf("resolve latest: %w", err)
		}
	}
	target = ensureV(target)
	cur := ensureV(currentVersion)
	res.Current, res.Target = cur, target

	// 2. Compare with current.
	fmt.Fprintf(opts.Stdout, "current: %s\nlatest:  %s\n", cur, target)
	if target == cur {
		fmt.Fprintln(opts.Stdout, "already up to date.")
		return res, nil
	}
	if opts.CheckOnly {
		fmt.Fprintln(opts.Stdout, "update available. run `poe-acp update` to install.")
		return res, nil
	}

	// 3. Download + verify. Assets are raw binaries: poe-acp-<os>-<arch>.
	asset := fmt.Sprintf("poe-acp-%s-%s", runtime.GOOS, assetArch(runtime.GOARCH))
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", opts.Repo, target)
	fmt.Fprintf(opts.Stdout, "downloading %s/%s\n", base, asset)

	tmpDir, err := os.MkdirTemp(filepath.Dir(resolved), ".poe-acp-update-*")
	if err != nil {
		return res, fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// The asset IS the binary; stage it executable so no separate chmod
	// step is needed (rename preserves the mode).
	newBin := filepath.Join(tmpDir, asset)
	actual, err := download(opts.HTTPClient, base+"/"+asset, newBin, 0o755)
	if err != nil {
		return res, fmt.Errorf("download asset: %w", err)
	}

	sumsPath := filepath.Join(tmpDir, "checksums.txt")
	if _, err := download(opts.HTTPClient, base+"/checksums.txt", sumsPath, 0o644); err != nil {
		return res, fmt.Errorf("download checksums: %w", err)
	}

	expected, err := lookupChecksum(sumsPath, asset)
	if err != nil {
		return res, err
	}
	if actual != expected {
		return res, fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	fmt.Fprintln(opts.Stdout, "✓ checksum verified")

	// 4. Atomic rename. tmpDir is alongside resolved → same filesystem.
	if err := os.Rename(newBin, resolved); err != nil {
		return res, fmt.Errorf("replace: %w", err)
	}
	res.Updated = true
	fmt.Fprintf(opts.Stdout, "✓ poe-acp updated: %s → %s at %s\n", cur, target, resolved)
	return res, nil
}

// ensureV prepends "v" to a version string if not already present.
func ensureV(s string) string {
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// resolveLatest returns the tag of the latest GitHub release of repo
// (e.g. "v0.27.0"). Uses the unauthenticated GitHub API.
func resolveLatest(c *http.Client, repo string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", errors.New("github api: empty tag_name")
	}
	return rel.TagName, nil
}

// download GETs src, writes it to dst with the given file mode, and
// returns the hex-encoded sha256 of the bytes written (computed inline so
// there is no read-back of the file we just wrote).
func download(c *http.Client, src, dst string, mode os.FileMode) (string, error) {
	resp, err := c.Get(src)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET %s: %s", src, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", err
	}
	// Best-effort durability hint. Integrity is already guaranteed by the
	// returned sha256 (computed inline over the bytes as they were copied)
	// which the caller verifies against checksums.txt; a kernel-level
	// Sync EIO would also surface as a failed exec on the next start, and
	// the update is freely re-runnable. So we do not gate on it.
	_ = f.Sync()
	return hex.EncodeToString(h.Sum(nil)), nil
}

func lookupChecksum(sumsPath, asset string) (string, error) {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", asset)
}

// assetArch maps runtime.GOARCH to the arch suffix used in release asset
// names. GOARCH=arm (with GOARM=6 in our matrix) is published as "armv6".
func assetArch(goarch string) string {
	if goarch == "arm" {
		return "armv6"
	}
	return goarch
}
