package main

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// batchResult records the outcome of downloading every asset for one row
// from the urls table. The fields feed both stderr summary lines and the
// per-row DB update at the end of runBatch.
type batchResult struct {
	Downloaded int
	Skipped    int
	Failed     int
	Packages   []string // filenames actually moved into downloadDir (success + skipped, in input order)
}

func (r batchResult) Status() string {
	switch {
	case r.Downloaded == 0 && r.Skipped == 0 && r.Failed > 0:
		return "failed"
	case r.Failed > 0:
		return "partial"
	case r.Downloaded == 0 && r.Skipped > 0:
		return "skipped"
	default:
		return "success"
	}
}

func (r batchResult) PackageCSV() string {
	if len(r.Packages) == 0 {
		return ""
	}
	return strings.Join(r.Packages, ",")
}

type batchOpts struct {
	timeout     time.Duration
	token       string
	download    bool
	downloadDir string
	cacheDir    string
}

// runBatch opens dbPath, reads every row from the urls table, and for
// each: fetches the latest GitHub release, keeps only the assets
// whose names match the row's configured pkg (exact name or glob),
// then decides whether to download based on the stored download_url:
//
//   - if no pkg is configured → skip silently (no network)
//   - if the current release's matched asset URLs (sorted) match the
//     stored download_url set exactly → skip the download entirely
//   - otherwise → download every matched asset (force overwrite) and
//     store the new URLs in download_url
//
// Per-row failures (parse error, fetch error, no release match for
// the configured pattern) are stored in the DB and logged to stderr;
// they never abort the batch.
func runBatch(dbPath string, opts batchOpts) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := listURLs(db)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no URLs in %s (insert rows via your own SQL)", dbPath)
	}

	cachePath := opts.cacheDir
	if cachePath == "" {
		cachePath = filepath.Join(os.TempDir(), "gh-latest-cache")
	}

	ok := 0
	skippedNoConfig := 0
	for _, row := range rows {
		raw := row.ReleaseURL
		configured := row.configuredPackages()

		// No package configured for any platform → nothing to track.
		// Skip silently (no network call) and record the row so the
		// DB status column flags it as "intentionally untouched".
		if len(configured) == 0 {
			skippedNoConfig++
			fmt.Fprintf(os.Stderr, "[skip] %s: no packages configured\n", raw)
			if uerr := updateResult(db, raw, "skipped (no_config)"); uerr != nil {
				fmt.Fprintf(os.Stderr, "[db] %s: %v\n", raw, uerr)
			}
			continue
		}

		repo, perr := parseRepo(raw)
		if perr != nil {
			_ = updateResult(db, raw, "parse_error: "+truncate(perr.Error(), 180))
			fmt.Fprintf(os.Stderr, "[skip] %s: %v\n", raw, perr)
			continue
		}

		rel, ferr := fetchLatestRelease(repo, opts.timeout, opts.token)
		if ferr != nil {
			_ = markFetchError(db, raw, ferr.Error())
			fmt.Fprintf(os.Stderr, "[fetch] %s: %v\n", repo, ferr)
			continue
		}

		// The configured pkg is the authoritative spec. Exact
		// matches or globs (path.Match '*' / '?') all keep their
		// case; everything else is dropped. This is the only
		// selection step — there is no platform/arch pre-filter
		// anymore, so any release naming convention works
		// (e.g. WSL's "Microsoft.WSL_*.msixbundle" which has no
		// "windows" token in the filename).
		matches := filterByConfigured(rel.Assets, configured)

		if len(matches) == 0 {
			if uerr := updateResult(db, raw,
				"failed (not_found: "+strings.Join(configured, ",")+" in "+rel.TagName+")"); uerr != nil {
				fmt.Fprintf(os.Stderr, "[db] %s: %v\n", raw, uerr)
			}
			fmt.Fprintf(os.Stderr, "[not_found] %s %s: configured %s\n",
				repo, rel.TagName, strings.Join(configured, ","))
			continue
		}

		sortByPlatform(matches)

		// Build the set of URLs we would download for this row, in
		// deterministic order. Compare against what we stored last
		// time: equal sets means GitHub hasn't changed anything we'd
		// care about, so we can skip the download — but only if the
		// local files are also still on disk. A user who manually
		// deleted Xray-windows-64.zip from downloadDir should not be
		// left without a copy just because the URL hasn't moved.
		assetURLs := make([]string, 0, len(matches))
		for _, a := range matches {
			assetURLs = append(assetURLs, a.BrowserDownloadURL)
		}
		currentSet := normalizeURLSet(assetURLs)
		storedSet := normalizeURLSet(row.splitStoredDownloadURL())

		if equalURLSets(currentSet, storedSet) && len(storedSet) > 0 && filesPresent(opts.downloadDir, storedSet) {
			fmt.Fprintf(os.Stderr, "[up_to_date] %s %s (%d urls)\n",
				repo, rel.TagName, len(currentSet))
			if uerr := updateResult(db, raw,
				"skipped (up_to_date "+rel.TagName+")"); uerr != nil {
				fmt.Fprintf(os.Stderr, "[db] %s: %v\n", raw, uerr)
			}
			ok++
			continue
		}

		// Anything else (no stored URL, or URL set drifted) → download.
		// download_url in the DB is the durable record of what we last
		// fetched; the user inspects the DB or the download dir, not a
		// temp file. No result file is written.
		dumpURLs := assetURLs

		if opts.download {
			// skipIfExists=false: the URL comparison above is the gate,
			// so a local file is stale whenever we get here. Overwrite.
			result := downloadAll(dumpURLs, opts.downloadDir, cachePath, false)
			fmt.Fprintf(os.Stderr,
				"[%s] %s %s -> %d ok, %d skip, %d fail (%s)\n",
				result.Status(), repo, rel.TagName,
				result.Downloaded, result.Skipped, result.Failed,
				result.PackageCSV())
			status := result.Status() + " (" + rel.TagName + ")"
			// Only record download_url when at least one file actually
			// landed in downloadDir — otherwise the next run would
			// wrongly think it's up to date.
			newURLs := ""
			if result.Downloaded > 0 {
				newURLs = strings.Join(currentSet, ",")
			}
			if uerr := markDownloaded(db, raw, newURLs, status); uerr != nil {
				fmt.Fprintf(os.Stderr, "[db] %s: %v\n", raw, uerr)
			}
		} else {
			// Listing-only run: still record that we know the new URL
			// set so the next -download run can do the up_to_date
			// check without re-fetching (well, it will re-fetch, but
			// the recorded URLs document the candidate).
			if uerr := markDownloaded(db, raw, strings.Join(currentSet, ","),
				"listed ("+rel.TagName+")"); uerr != nil {
				fmt.Fprintf(os.Stderr, "[db] %s: %v\n", raw, uerr)
			}
		}
		ok++
	}

	fmt.Fprintf(os.Stderr,
		"processed %d/%d rows from %s (%d skipped: no packages configured)\n",
		ok, len(rows), dbPath, skippedNoConfig)
	return nil
}

// filterByConfigured keeps assets whose Name matches at least one
// configured pattern (case-insensitive). Patterns are interpreted as
// path.Match globs — '*' matches any non-'/' sequence, '?' matches one
// character, '[abc]' is a character class. A pattern with no
// wildcards behaves like an exact match.
//
// Glob support lets the user pin a family name across release versions
// whose exact filename changes every release, e.g.
// "Microsoft.WSL_*_ARM64.msixbundle" matches
// "Microsoft.WSL_2.7.13.0_x64_ARM64.msixbundle" and any later
// version. Multiple matching assets are all kept (input order
// preserved); the caller decides what to do with more than one
// download per row.
func filterByConfigured(assets []asset, configured []string) []asset {
	if len(configured) == 0 {
		return nil
	}
	out := make([]asset, 0, len(assets))
patterns:
	for _, a := range assets {
		lower := strings.ToLower(a.Name)
		for _, c := range configured {
			pat := strings.ToLower(strings.TrimSpace(c))
			if pat == "" {
				continue
			}
			ok, err := path.Match(pat, lower)
			if err != nil {
				// Malformed pattern (e.g. stray '['). Skip this
				// pattern and let the next one try — we don't
				// want one bad row to abort the whole batch.
				continue
			}
			if ok {
				out = append(out, a)
				continue patterns
			}
		}
	}
	return out
}

// normalizeURLSet returns the unique, sorted, trimmed slice of URLs in
// the input. Comparison of two sets is then a simple len + elementwise
// check. Empty entries are dropped.
func normalizeURLSet(urls []string) []string {
	seen := make(map[string]bool, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func equalURLSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// filenamesFromURLs returns the last path segment of each URL in input
// order. Kept for backwards-compat with any future "listed" reporting
// that needs the planned package names.
func filenamesFromURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		out = append(out, filenameFromURL(u))
	}
	return out
}

// filesPresent reports whether every URL in urls has its target
// filename present in dir. Used by runBatch to refuse the
// "up_to_date" shortcut when a user has manually deleted a file from
// the download directory — they'd rather have the program re-fetch
// it than silently miss a file. Returns false on the first miss.
func filesPresent(dir string, urls []string) bool {
	for _, u := range urls {
		target := filepath.Join(dir, filenameFromURL(u))
		if _, err := os.Stat(target); err != nil {
			return false
		}
	}
	return true
}

// parseRepo normalizes a GitHub repo identifier to "owner/name". It
// accepts the shorthand form ("cli/cli") or a full
// "https://github.com/owner/name" URL (the release page is the same
// as the repo URL; both are accepted for convenience).
func parseRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", fmt.Errorf("empty repo")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
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

// exeDir returns the directory containing the running executable, or "" if
// it cannot be determined.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// hasUsableDB reports whether the SQLite file at dbPath is fit to open as
// the batch input. We accept:
//   - the file already exists, or
//   - its parent directory exists and is writable (so openDB can create it)
//
// If neither holds, batch mode is skipped and the tool falls back to
// single-repo mode (--repo), so an empty / typo'd --db never errors out.
func hasUsableDB(dbPath string) bool {
	if dbPath == "" {
		return false
	}
	if _, err := os.Stat(dbPath); err == nil {
		return true
	}
	dir := filepath.Dir(dbPath)
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return info.IsDir()
}