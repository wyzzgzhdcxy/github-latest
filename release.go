package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

type release struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Assets      []asset `json:"assets"`
}

func fetchLatestRelease(repo string, timeout time.Duration, token string) (*release, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-latest/1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := newHTTPClient(timeout)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusForbidden && bytesContains(body, "rate limit") {
		return nil, fmt.Errorf("GitHub API rate limit exceeded (set 'token' in config table or $GITHUB_TOKEN to raise the limit)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API %s: %s", resp.Status, truncate(string(body), 200))
	}

	var r release
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("GitHub returned no tag_name (rate limited? raw body: %s)", truncate(string(body), 200))
	}
	return &r, nil
}

// ---------------------------------------------------------------------------
// Platform detection: turn a release asset filename into (os, arch) so
// batch.go can sort the matched assets in a human-friendly order before
// downloading. The configured pkg (exact name or glob) is the only
// selection signal — no platform/arch pre-filter, no pick-best.
//
// Multiple naming conventions are recognized out of the box:
//
//   - sing-box:  "sing-box-1.14.0-linux-amd64-glibc.tar.gz"
//   - cli:       "gh_2.100.0_windows_amd64.zip"
//   - Xray:      "Xray-linux-64.zip", "Xray-macos-arm64-v8a.zip",
//                "Xray-win7-32.zip", "Xray-linux-arm32-v5.zip"
//   - generic:   "tool-x86_64.zip", "tool-aarch64.tar.gz"
// ---------------------------------------------------------------------------

var osKeywords = []string{
	"windows", "win7",
	"linux",
	"darwin", "macos", "osx",
	"freebsd", "openbsd", "netbsd", "dragonfly",
	"android", "ios",
}

var archKeywords = []string{
	"amd64", "x86_64", "x64", "64",
	"arm64", "aarch64",
	"386", "i386", "i686", "32",
	"armv7", "armv6", "armv5", "arm32",
	"mips64le", "mips64", "mipsle", "mips",
	"riscv64",
	"ppc64le", "ppc64",
	"s390x",
	"loong64",
}

// Natural display order for (os, arch); anything not in the map sorts after.
// Use canonical names (after canonicalArch / canonicalOS).
var osOrder = map[string]int{
	"windows": 0, "linux": 1, "darwin": 2,
	"freebsd": 3, "openbsd": 4, "netbsd": 5, "dragonfly": 6,
	"android": 7, "ios": 8,
}

var archOrder = map[string]int{
	"amd64": 0, "arm64": 1, "386": 2,
	"armv7": 3, "armv6": 4, "armv5": 5, "arm32": 6,
	"mips64": 7, "mips64le": 8, "mips": 9, "mipsle": 10,
	"riscv64": 11,
	"ppc64le": 12, "ppc64": 13,
	"s390x": 14,
	"loong64": 15,
}

// canonicalOS maps a recognized OS keyword to its canonical name so that
// variants ("win7" / "macos" / "osx") group with their family.
func canonicalOS(o string) string {
	switch o {
	case "win7", "win":
		return "windows"
	case "macos", "osx":
		return "darwin"
	default:
		return o
	}
}

// canonicalArch maps shorthand arch keywords to their canonical name so
// that Xray's "64" and Go's "amd64" group together.
func canonicalArch(a string) string {
	switch a {
	case "64":
		return "amd64"
	case "32":
		return "386"
	default:
		return a
	}
}

// parseAssetPlatform extracts (os, arch) from a release asset filename.
// Returns ("", "", false) when it can't determine both. The returned values
// are always in canonical form (e.g. "win7" → "windows", "64" → "amd64").
func parseAssetPlatform(name string) (os, arch string, ok bool) {
	lower := strings.ToLower(name)
	parts := strings.FieldsFunc(lower, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for _, p := range parts {
		if os == "" {
			for _, o := range osKeywords {
				if p == o {
					os = canonicalOS(o)
					break
				}
			}
		}
		if arch == "" {
			for _, a := range archKeywords {
				if p == a {
					arch = canonicalArch(a)
					break
				}
			}
		}
		if os != "" && arch != "" {
			break
		}
	}
	if os != "" && arch != "" {
		return os, arch, true
	}
	return "", "", false
}

// sortByPlatform orders assets by (os, arch) using osOrder/archOrder for a
// human-friendly sequence; unknowns fall back to lexical order.
func sortByPlatform(assets []asset) {
	rank := func(name string, m map[string]int) (int, bool) {
		v, ok := m[name]
		return v, ok
	}
	sort.SliceStable(assets, func(i, j int) bool {
		oi, ai, _ := parseAssetPlatform(strings.ToLower(assets[i].Name))
		oj, aj, _ := parseAssetPlatform(strings.ToLower(assets[j].Name))
		if oi != oj {
			ri, oki := rank(oi, osOrder)
			rj, okj := rank(oj, osOrder)
			switch {
			case oki && okj:
				return ri < rj
			case oki:
				return true
			case okj:
				return false
			}
			return oi < oj
		}
		if ai != aj {
			ri, oki := rank(ai, archOrder)
			rj, okj := rank(aj, archOrder)
			switch {
			case oki && okj:
				return ri < rj
			case oki:
				return true
			case okj:
				return false
			}
			return ai < aj
		}
		return assets[i].Name < assets[j].Name
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func bytesContains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}

func humanSize(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(k), 0
	for x := n / k; x >= k; x /= k {
		div *= k
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}
