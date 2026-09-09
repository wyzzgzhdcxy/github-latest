// Command gh-latest runs the batch over the SQLite DB and exits. There
// are no CLI flags: every knob is either a hardcoded default or a row
// in the `config` table inside the same DB. Edit config with your own
// SQL tool (any sqlite3 client works).
//
// Config table keys (all optional; missing key → hardcoded default):
//
//   download_dir   where matched assets land. Default: E:\github_app
//   proxy          HTTP proxy URL. Default: http://127.0.0.1:10808
//   no_proxy       "true" to disable proxy entirely. Default: false
//   timeout        HTTP request timeout (Go duration syntax, e.g. "20s").
//                  Default: 20s
//   cache_dir      in-progress download staging. Default:
//                  <os.TempDir>/gh-latest-cache
//   download       "true" to move files to download_dir. "false" to
//                  only update the DB. Default: true
//
// The GitHub API token is NEVER stored in the DB. It is resolved on
// every run from `gh auth token` (with $GITHUB_TOKEN env as a
// fallback), so PAT rotation and OAuth refresh just work.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultDB          = `C:\Users\wangchaojun\AppData\Local\wtools\data\update_github_app.db`
	defaultDownloadDir = `E:\github_app`
	defaultProxyURL    = "http://127.0.0.1:10808"
	defaultTimeout     = 20 * time.Second
	defaultDownload    = true
	defaultNoProxy     = false
)

func main() {
	// Single-instance lock: a stale crash leaves a lock file behind;
	// acquireLock reports that as a friendly "another run is going"
	// and we exit cleanly. The user's next run after the crash
	// cleanup will then proceed.
	releaseLock, err := acquireLock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(0)
	}
	defer releaseLock()

	db, err := openDB(defaultDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db %s: %v\n", defaultDB, err)
		os.Exit(1)
	}
	defer db.Close()

	// Every setting below is config-table driven. A missing key falls
	// back to the hardcoded default above.
	downloadDir := getConfig(db, "download_dir", defaultDownloadDir)
	proxy := getConfig(db, "proxy", defaultProxyURL)
	noProxy := parseBool(getConfig(db, "no_proxy", ""), defaultNoProxy)
	download := parseBool(getConfig(db, "download", ""), defaultDownload)

	timeout := defaultTimeout
	if raw := getConfig(db, "timeout", ""); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			timeout = d
		} else {
			fmt.Fprintf(os.Stderr, "config: bad timeout %q (want Go duration like 20s); using default %s\n", raw, defaultTimeout)
		}
	}

	cacheDir := getConfig(db, "cache_dir", "")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "gh-latest-cache")
	}

	token := resolveToken()

	// Proxy is a global package var consumed by every HTTP client.
	SetProxyConfig(noProxy, proxy)

	if err := runBatch(defaultDB, batchOpts{
		timeout:     timeout,
		token:       token,
		download:    download,
		downloadDir: downloadDir,
		cacheDir:    cacheDir,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "batch error: %v\n", err)
		os.Exit(1)
	}
}

// parseBool accepts the common textual forms so a user can write
// "true"/"false"/"1"/"0"/"yes"/"no"/"on"/"off" in the config table. An
// empty or unrecognized value returns the hardcoded fallback.
func parseBool(s string, fallback bool) bool {
	switch s {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return fallback
}

// resolveToken picks the freshest available GitHub token. Tokens are
// NEVER persisted (they expire and rotate), so every run asks the
// dynamic source. Order:
//
//  1. `gh auth token` (the GitHub CLI manages OAuth refresh itself —
//     the call returns the current valid token, or fails silently if
//     the user isn't logged in)
//  2. $GITHUB_TOKEN env var (handy for CI / scheduled tasks where gh
//     CLI isn't installed)
//
// 4 DB config table is intentionally not in the list — a fixed-value
// `token` row there would silently rot after expiry.
func resolveToken() string {
	// CREATE_NO_WINDOW (0x08000000) + HideWindow prevents `gh` from
	// briefly allocating its own console — the parent process is a
	// GUI-subsystem binary with no console, and without these flags
	// Windows will flash a transient cmd window when gh starts.
	cmd := exec.Command("gh", "auth", "token")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
	if out, err := cmd.Output(); err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t
		}
	}
	return os.Getenv("GITHUB_TOKEN")
}
