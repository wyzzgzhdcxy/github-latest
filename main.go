// Command gh-latest prints the latest GitHub release download URLs that match
// the requested platforms and architectures.
//
// Default target is SagerNet/sing-box. Pass --repo to query any other repo.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultRepo = "SagerNet/sing-box"

func main() {
	var (
		repoFlag  = flag.String("repo", defaultRepo, "GitHub repo as 'owner/name' or full https://github.com/... URL")
		platforms = flag.String("platforms", "windows", "Comma-separated platform substrings to include (case-insensitive; use 'darwin' for macOS). Default is windows only; add linux/darwin/etc. explicitly if you want them")
		archs     = flag.String("archs", "amd64", "Comma-separated architecture substrings to include (case-insensitive). Default is amd64 only; add arm64/etc. explicitly if you want them")
		format    = flag.String("format", "table", "Output format: table | urls | json")
		download  = flag.Bool("download", true, "In batch mode, also download URLs from the result file to --download-dir (default: on). Pass -download=false to only write the result file. In single mode, downloads to --output-dir")
		proxy     = flag.String("proxy", "http://127.0.0.1:10808", "HTTP proxy URL (default: http://127.0.0.1:10808). Overrides HTTP_PROXY/HTTPS_PROXY env vars. Pass -proxy=\"\" to use only env vars. Combine with --no-proxy to bypass any proxy entirely")
		outDir    = flag.String("output-dir", ".", "Directory to save downloads")
		timeout   = flag.Duration("timeout", 20*time.Second, "HTTP request timeout")
		noProxy   = flag.Bool("no-proxy", false, "Ignore HTTP(S) proxy from environment")
		showAll   = flag.Bool("all", false, "Ignore --platforms/--archs and show every asset")
		pickBest  = flag.Bool("best", true, "Pick the most generic asset per (os,arch) (skips .deb/.rpm/.msi, prefers plain over glibc/musl/legacy). Set -best=false to keep every match")
		token     = flag.String("token", "", "GitHub token (defaults to $GITHUB_TOKEN); raises API rate limit")
		fromFile    = flag.String("from-file", "", "Batch mode: read GitHub URLs (one per line) from this file. If unset, auto-detects <exe-dir>/github_list.txt. Lines starting with '#' are comments")
		outFile     = flag.String("output", "", "In batch mode, write all download URLs to this file (overwrite). Default: <exe-dir>/github_result.txt")
		downloadDir = flag.String("download-dir", `E:\github_app`, "In batch mode +download, the final destination for downloaded files. Created if missing")
		cacheDir    = flag.String("cache-dir", "", "Temp directory used while downloads are in progress. Default: <os.TempDir>/gh-latest-cache. Files move to --download-dir when complete")
	)
	flag.Usage = usage
	flag.Parse()

	// Single-instance lock: if a previous run is still going (or crashed
	// without cleanup), exit immediately instead of overlapping.
	releaseLock, err := acquireLock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(0)
	}
	defer releaseLock()

	// Proxy config is global so both GitHub API calls and downloads honor it.
	SetProxyConfig(*noProxy, *proxy)

	// --- Batch mode: if --from-file is given (or github_list.txt exists
	// next to the exe), process every URL and write to --output (or
	// github_result.txt next to the exe). Always overwrites. ---
	if batchInput := *fromFile; batchInput != "" || hasDefaultList() {
		if batchInput == "" {
			batchInput = filepath.Join(exeDir(), "github_list.txt")
		}
		outPath := *outFile
		if outPath == "" {
			if dir := exeDir(); dir != "" {
				outPath = filepath.Join(dir, "github_result.txt")
			} else {
				outPath = "github_result.txt"
			}
		}
		cachePath := *cacheDir
		if cachePath == "" {
			cachePath = filepath.Join(os.TempDir(), "gh-latest-cache")
		}
		if err := runBatch(batchInput, outPath, batchOpts{
			timeout:     *timeout,
			token:       pickToken(*token),
			platforms:   *platforms,
			archs:       *archs,
			pickBest:    *pickBest,
			showAll:     *showAll,
			download:    *download,
			downloadDir: *downloadDir,
			cacheDir:    cachePath,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "batch error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	repo, err := parseRepo(*repoFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	rel, err := fetchLatestRelease(repo, *timeout, pickToken(*token))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching release for %s: %v\n", repo, err)
		os.Exit(1)
	}

	var matches []asset
	if *showAll {
		matches = rel.Assets
	} else {
		matches = filterAssets(rel.Assets, splitSet(*platforms), splitSet(*archs))
		if *pickBest {
			matches = pickBestPerPlatform(matches)
		}
	}

	switch strings.ToLower(*format) {
	case "json":
		printJSON(rel, matches)
	case "urls":
		printURLs(matches)
	case "table":
		printTable(rel, matches)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --format %q (want table|urls|json)\n", *format)
		os.Exit(2)
	}

	if *download && len(matches) > 0 {
		if err := downloadAssets(matches, *outDir, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "download error: %v\n", err)
			os.Exit(1)
		}
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `gh-latest - get the latest GitHub release download URLs

Usage:
  gh-latest [flags]

Defaults target SagerNet/sing-box. Pass any GitHub repo via --repo.

Examples:
  gh-latest
  gh-latest --repo cli/cli
  gh-latest --repo https://github.com/golang/go
  gh-latest --platforms windows --archs amd64
  gh-latest --format urls --download --output-dir ./dl
  gh-latest --all --format json              # show every asset
  gh-latest --best=false --format json       # show every match, no platform dedup
  gh-latest --from-file list.txt --output result.txt  # batch: many repos at once
  gh-latest                                  # default: write result + download to E:\github_app
  gh-latest -download=false                  # only write github_result.txt, no download
  gh-latest -download -download-dir D:\apps  # custom target dir
  # ...or just drop URLs into github_list.txt next to the exe and run gh-latest

Flags:
`)
	flag.PrintDefaults()
}

func parseRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", fmt.Errorf("empty repo")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := urlParse(s)
		if err != nil {
			return "", fmt.Errorf("invalid URL: %w", err)
		}
		if !strings.EqualFold(u.Host, "github.com") {
			return "", fmt.Errorf("not a github.com URL: %s", s)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("URL does not contain owner/repo: %s", s)
		}
		return parts[0] + "/" + parts[1], nil
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("repo must be 'owner/name', got %q", s)
	}
	return s, nil
}

func pickToken(flagVal string) string {
	if t := strings.TrimSpace(flagVal); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	return ""
}
