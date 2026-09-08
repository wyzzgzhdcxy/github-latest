package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
		return nil, fmt.Errorf("GitHub API rate limit exceeded (set --token or $GITHUB_TOKEN to raise the limit)")
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

func filterAssets(assets []asset, platforms, archs map[string]bool) []asset {
	out := make([]asset, 0, len(assets))
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		tokens := strings.FieldsFunc(name, func(r rune) bool {
			return r == '-' || r == '_' || r == '.'
		})
		if len(platforms) > 0 && !anyTokenIn(tokens, platforms) {
			continue
		}
		if len(archs) > 0 && !anyTokenIn(tokens, archs) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func anyTokenIn(tokens []string, set map[string]bool) bool {
	for _, t := range tokens {
		if set[t] {
			return true
		}
	}
	return false
}

// platformFilterAliases / archFilterAliases make --platforms and --archs
// forgiving: passing one canonical name expands to every substring variant
// the user might see in real release filenames. e.g.:
//   --platforms=darwin   → {darwin, macos, osx}
//   --platforms=windows  → {windows, win7}
//   --archs=amd64        → {amd64, x86_64, x64, 64}
//   --archs=arm64        → {arm64, aarch64}
var platformFilterAliases = map[string][]string{
	"windows": {"windows", "win7"},
	"linux":   {"linux"},
	"darwin":  {"darwin", "macos", "osx"},
	"freebsd": {"freebsd"},
	"openbsd": {"openbsd"},
	"netbsd":  {"netbsd"},
	"android": {"android"},
	"ios":     {"ios"},
}

var archFilterAliases = map[string][]string{
	"amd64":    {"amd64", "x86_64", "x64", "64"},
	"arm64":    {"arm64", "aarch64"},
	"386":      {"386", "i386", "i686", "32"},
	"armv7":    {"armv7", "arm32"},
	"armv6":    {"armv6"},
	"armv5":    {"armv5"},
	"mips64le": {"mips64le"},
	"mips64":   {"mips64"},
	"mipsle":   {"mipsle"},
	"mips":     {"mips"},
	"riscv64":  {"riscv64"},
	"ppc64le":  {"ppc64le"},
	"ppc64":    {"ppc64"},
	"s390x":    {"s390x"},
	"loong64":  {"loong64"},
}

func splitSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, p := range strings.Split(s, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if family, ok := platformFilterAliases[p]; ok {
			for _, v := range family {
				out[v] = true
			}
			continue
		}
		if family, ok := archFilterAliases[p]; ok {
			for _, v := range family {
				out[v] = true
			}
			continue
		}
		out[p] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Platform detection: turn a release asset filename into (os, arch), and
// pick the most generic asset per pair. Multiple naming conventions are
// supported out of the box:
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

// packageExts marks installer / package-manager formats that are NOT portable
// binaries and should be skipped when picking "the most appropriate" asset.
var packageExts = map[string]bool{
	".deb": true, ".rpm": true, ".apk": true,
	".pkg": true, ".msi": true, ".dmg": true,
}

// specialSuffixes bump an asset's score so the plain / generic variant wins.
var specialSuffixes = []string{
	"glibc", "musl", "legacy", "alpine", "hardened", "debug", "win7",
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

func isPackageFormat(name string) bool {
	lower := strings.ToLower(name)
	for ext := range packageExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func specialSuffixScore(name string) int {
	lower := strings.ToLower(name)
	score := 0
	for _, s := range specialSuffixes {
		if strings.Contains(lower, s) {
			score += 10
		}
	}
	return score
}

// pickBestPerPlatform returns at most one asset per (os, arch) combination.
// Package formats are dropped, and among multiple candidates the one with the
// fewest "special" suffixes (glibc, musl, legacy, ...) wins. Ties go to the
// lexicographically smaller name for determinism.
func pickBestPerPlatform(assets []asset) []asset {
	type entry struct {
		os, arch string
		asset    asset
		score    int
	}
	groups := map[string]*entry{}
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if isPackageFormat(name) {
			continue
		}
		os, arch, ok := parseAssetPlatform(name)
		if !ok {
			continue
		}
		key := os + "/" + arch
		score := specialSuffixScore(name)
		if cur, exists := groups[key]; !exists ||
			score < cur.score ||
			(score == cur.score && a.Name < cur.asset.Name) {
			groups[key] = &entry{os: os, arch: arch, asset: a, score: score}
		}
	}
	out := make([]asset, 0, len(groups))
	for _, e := range groups {
		out = append(out, e.asset)
	}
	sortByPlatform(out)
	return out
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

// printTable prints a human-friendly summary.
func printTable(rel *release, matches []asset) {
	fmt.Printf("Version    : %s\n", rel.TagName)
	if rel.Name != "" && rel.Name != rel.TagName {
		fmt.Printf("Name       : %s\n", rel.Name)
	}
	fmt.Printf("Published  : %s\n", rel.PublishedAt)
	fmt.Printf("Release    : %s\n", rel.HTMLURL)
	fmt.Printf("Assets     : %d matched / %d total\n\n", len(matches), len(rel.Assets))

	if len(matches) == 0 {
		fmt.Println("(no matching assets — try --all or relax --platforms/--archs)")
		return
	}
	sortByPlatform(matches)

	platW := len("platform")
	nameW := 0
	for _, a := range matches {
		if o, ar, ok := parseAssetPlatform(strings.ToLower(a.Name)); ok {
			if w := len(o + "/" + ar); w > platW {
				platW = w
			}
		}
		if len(a.Name) > nameW {
			nameW = len(a.Name)
		}
	}
	fmt.Printf("  %-*s  %-*s  %8s  %s\n", platW, "platform", nameW, "asset", "size", "url")
	for _, a := range matches {
		plat := "-"
		if o, ar, ok := parseAssetPlatform(strings.ToLower(a.Name)); ok {
			plat = o + "/" + ar
		}
		fmt.Printf("  %-*s  %-*s  %8s  %s\n", platW, plat, nameW, a.Name, humanSize(a.Size), a.BrowserDownloadURL)
	}
}

// printURLs prints one URL per line — convenient for piping into curl/wget.
func printURLs(matches []asset) {
	sortByPlatform(matches)
	for _, a := range matches {
		fmt.Println(a.BrowserDownloadURL)
	}
}

func printJSON(rel *release, matches []asset) {
	out := struct {
		Version   string  `json:"version"`
		Name      string  `json:"name"`
		Published string  `json:"published_at"`
		HTMLURL   string  `json:"html_url"`
		Assets    []asset `json:"assets"`
	}{rel.TagName, rel.Name, rel.PublishedAt, rel.HTMLURL, matches}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %v\n", err)
	}
}

func downloadAssets(matches []asset, dir string, timeout time.Duration) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	client := newHTTPClient(timeout)

	for _, a := range matches {
		dest := filepath.Join(dir, a.Name)
		fmt.Fprintf(os.Stderr, "↓ %s -> %s\n", a.Name, dest)
		if err := downloadFile(client, a.BrowserDownloadURL, dest); err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
	}
	return nil
}

func downloadFile(client *http.Client, rawURL, dest string) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gh-latest/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func bytesContains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}

func urlParse(s string) (*url.URL, error) {
	return url.Parse(s)
}
