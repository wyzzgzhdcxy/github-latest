package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type batchOpts struct {
	timeout     time.Duration
	token       string
	platforms   string
	archs       string
	pickBest    bool
	showAll     bool
	download    bool
	downloadDir string
	cacheDir    string
}

// runBatch reads URLs from inputPath, fetches each repo's latest release
// download URLs (applying the same --platforms/--archs/--best filtering as
// single mode), and writes them to outputPath. The output file is truncated
// first. Per-URL failures are recorded as "# <repo>: ERROR ..." comment
// lines and do not abort the run.
func runBatch(inputPath, outputPath string, opts batchOpts) error {
	urls, err := readURLList(inputPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", inputPath, err)
	}
	if len(urls) == 0 {
		return fmt.Errorf("no URLs in %s", inputPath)
	}

	out, err := os.Create(outputPath) // truncate, no append
	if err != nil {
		return fmt.Errorf("create %s: %w", outputPath, err)
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()

	ok := 0
	for _, line := range urls {
		repo, perr := parseRepo(line)
		if perr != nil {
			fmt.Fprintf(w, "# %s: ERROR %v\n", line, perr)
			fmt.Fprintf(os.Stderr, "[skip] %s: %v\n", line, perr)
			continue
		}

		rel, ferr := fetchLatestRelease(repo, opts.timeout, opts.token)
		if ferr != nil {
			fmt.Fprintf(w, "# %s: ERROR %v\n", repo, ferr)
			fmt.Fprintf(os.Stderr, "[fetch] %s: %v\n", repo, ferr)
			continue
		}

		matches := rel.Assets
		if !opts.showAll {
			matches = filterAssets(matches, splitSet(opts.platforms), splitSet(opts.archs))
			if opts.pickBest {
				matches = pickBestPerPlatform(matches)
			}
		}

		if len(matches) == 0 {
			fmt.Fprintf(w, "# %s %s: (no matching assets)\n", repo, rel.TagName)
			continue
		}

		sortByPlatform(matches)
		fmt.Fprintf(w, "# %s %s (%d)\n", repo, rel.TagName, len(matches))
		for _, a := range matches {
			fmt.Fprintln(w, a.BrowserDownloadURL)
		}
		ok++
	}

	fmt.Fprintf(os.Stderr, "wrote %d/%d repos to %s\n", ok, len(urls), outputPath)

	if opts.download {
		// Flush the bufio.Writer BEFORE reading the file back; otherwise
		// the buffered bytes are still in memory and downloadFromResultFile
		// sees an empty/stale file.
		_ = w.Flush()
		if err := downloadFromResultFile(outputPath, opts.downloadDir, opts.cacheDir); err != nil {
			fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		}
	}

	return nil
}

// readURLList reads non-empty, non-comment lines from path.
// Blank lines and lines starting with '#' (after trimming) are ignored.
func readURLList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
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

// hasDefaultList reports whether <exe-dir>/github_list.txt exists.
func hasDefaultList() bool {
	dir := exeDir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "github_list.txt"))
	return err == nil
}
