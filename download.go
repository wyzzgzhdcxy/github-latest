package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Proxy resolution: package-level config set by main() from CLI flags
// and consumed by every HTTP client the tool creates.
var (
	noProxyFlag      bool   // true → disable all proxying
	explicitProxyURL string // non-empty → use this URL for every request
)

// SetProxyConfig is called by main() once at startup. After that, every
// newHTTPClient (and every request via it) honors these settings.
func SetProxyConfig(noProxy bool, proxyURL string) {
	noProxyFlag = noProxy
	explicitProxyURL = proxyURL
}

// newHTTPClient returns an http.Client configured per SetProxyConfig:
//   - noProxy=true           → Transport with Proxy: nil (no proxy at all)
//   - explicitProxyURL set   → always use that URL
//   - otherwise              → fall back to HTTP_PROXY / HTTPS_PROXY env
func newHTTPClient(timeout time.Duration) *http.Client {
	if noProxyFlag {
		return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: proxyFunc}}
}

// proxyFunc resolves the proxy URL for a request. Priority:
//  1. --proxy CLI flag (explicitProxyURL)
//  2. HTTP_PROXY/HTTPS_PROXY env vars (read fresh each call, not cached)
//
// Returns nil if no proxy should be used, signaling a direct connection.
//
// Note: we cannot use http.ProxyFromEnvironment here — it caches the
// environment at the first call (sync.OnceValue on defaultProxyConfig),
// so any HTTP_PROXY set after process start (e.g. via t.Setenv in tests,
// or a script that sets the var right before invoking the binary) is
// invisible. Reading os.Getenv here on every request is the fix.
func proxyFunc(req *http.Request) (*url.URL, error) {
	if explicitProxyURL != "" {
		return url.Parse(explicitProxyURL)
	}
	var raw string
	if req.URL.Scheme == "https" {
		raw = firstNonEmpty("HTTPS_PROXY", "https_proxy")
	} else {
		raw = firstNonEmpty("HTTP_PROXY", "http_proxy")
	}
	if raw == "" {
		return nil, nil
	}
	return url.Parse(raw)
}

func firstNonEmpty(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// filenameFromURL extracts the last URL path segment, stripping any
// query string or fragment. Falls back to "download" for empty results.
func filenameFromURL(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		u = u[:i]
	}
	if i := strings.Index(u, "#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndex(u, "/"); i >= 0 {
		fname := u[i+1:]
		if fname != "" {
			return fname
		}
	}
	return "download"
}

// downloadFromResultFile reads URLs (non-comment, non-empty lines) from
// resultPath and downloads each one into downloadDir via cacheDir.
func downloadFromResultFile(resultPath, downloadDir, cacheDir string) error {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", resultPath, err)
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	_ = downloadAll(urls, downloadDir, cacheDir, false /* skipIfExists */)
	return nil
}

// downloadAll downloads each URL to downloadDir. Each file is fetched
// into cacheDir first, then moved (atomic on same volume) so partial
// downloads never appear in the target directory.
//
// skipIfExists=true keeps the legacy "skip when target is already on
// disk" behavior. skipIfExists=false (batch mode, where the URL
// comparison is the gate) always re-downloads and overwrites the target
// — the caller has already decided the GitHub URL changed, so any
// local copy is stale by definition.
//
// The returned batchResult lets callers (e.g. runBatch) persist
// per-call statistics back into the urls table.
func downloadAll(urls []string, downloadDir, cacheDir string, skipIfExists bool) batchResult {
	if len(urls) == 0 {
		return batchResult{}
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "! mkdir %s: %v\n", downloadDir, err)
		return batchResult{}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "! mkdir %s: %v\n", cacheDir, err)
		return batchResult{}
	}

	var res batchResult
	for _, u := range urls {
		fname := filenameFromURL(u)
		target := filepath.Join(downloadDir, fname)
		if skipIfExists {
			if _, err := os.Stat(target); err == nil {
				res.Skipped++
				res.Packages = append(res.Packages, fname)
				fmt.Fprintf(os.Stderr, "= skip   %s (already in target)\n", fname)
				continue
			}
		}
		fmt.Fprintf(os.Stderr, "+ get    %s\n", fname)
		cache, err := downloadOne(u, cacheDir)
		if err != nil {
			res.Failed++
			fmt.Fprintf(os.Stderr, "\n! FAIL   %s: %v\n", fname, err)
			continue
		}
		if err := moveFile(cache, target); err != nil {
			res.Failed++
			fmt.Fprintf(os.Stderr, "! FAIL   move %s -> %s: %v\n", cache, target, err)
			_ = os.Remove(cache)
			continue
		}
		res.Downloaded++
		res.Packages = append(res.Packages, fname)
		fmt.Fprintf(os.Stderr, "v saved  %s\n", target)
	}
	fmt.Fprintf(os.Stderr, "\nsummary: %d downloaded, %d skipped, %d failed -> %s\n",
		res.Downloaded, res.Skipped, res.Failed, downloadDir)
	return res
}

// downloadOne fetches url into cacheDir using Go's net/http, returning
// the local cache path. Prints a single-line byte progress on stderr
// that is overwritten in place until the file finishes.
func downloadOne(url, cacheDir string) (string, error) {
	fname := filenameFromURL(url)
	dest := filepath.Join(cacheDir, fname)

	client := newHTTPClient(10 * time.Minute)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "gh-latest/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()

	total := resp.ContentLength
	var downloaded int64
	bw := bufio.NewWriter(f)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := bw.Write(buf[:n]); werr != nil {
				_ = os.Remove(dest)
				return "", werr
			}
			downloaded += int64(n)
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r  %6s / %6s  %3d%%",
					humanSize(downloaded), humanSize(total),
					int(float64(downloaded)/float64(total)*100))
			} else {
				fmt.Fprintf(os.Stderr, "\r  %6s", humanSize(downloaded))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = os.Remove(dest)
			return "", rerr
		}
	}
	if err := bw.Flush(); err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	fmt.Fprintln(os.Stderr) // newline to terminate the progress line
	return dest, nil
}

// moveFile moves src to dst, transparently handling the case where src
// and dst live on different volumes (os.Rename fails across drives on
// Windows with "The system cannot move the file to a different disk
// drive"). The fast path is rename; the fallback is copy + remove.
//
// On Windows os.Open does not request FILE_SHARE_DELETE, so the source
// handle blocks os.Remove unless it is closed first. copyFile closes the
// source via defer before returning, so by the time we call os.Remove
// here the handle is already released.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}
