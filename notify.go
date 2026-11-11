package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// default ntfy timeout is intentionally short: a notification is
// best-effort decoration on top of the real download work and must
// never delay or block the batch.
const defaultNtfyTimeout = 3 * time.Second

// ntfyLogName is the fallback diagnostic log written next to the exe.
// The binary is built with -H windowsgui, so stderr goes nowhere —
// without this file a failed notification is invisible (exactly how
// the first "no notification arrived" report was hard to diagnose).
// Only failures are appended; a healthy run touches nothing.
const ntfyLogName = "gh-latest.log"

// plog appends one timestamped line to the diagnostic log beside the
// executable, then mirrors it to stderr for console builds. Errors
// writing the log are ignored: logging must never break the batch.
func plog(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n",
		time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	fmt.Fprint(os.Stderr, line)

	dir := exeDir()
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, ntfyLogName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// notifyEntry is one repo's worth of freshly downloaded assets. Kept
// minimal so building the message is cheap.
type notifyEntry struct {
	Repo     string
	Tag      string
	Packages []string
}

// Notifier wraps the optional ntfy endpoint. A zero value disables
// every Send() call so callers never have to check before invoking.
type Notifier struct {
	URL   string // base URL, e.g. http://111.229.201.94:48081
	Topic string // path segment, e.g. toDEzxy...
	User  string // optional basic-auth user; non-empty → attach Authorization header
	Pass  string // optional basic-auth pass
	HTTP  *http.Client

	// wg tracks in-flight Send() goroutines so main can Wait() before
	// exiting. Without this the process can terminate while the HTTP
	// request is still in the TCP handshake, silently dropping the
	// notification — the exact bug that made a successful download
	// produce no ntfy message.
	wg sync.WaitGroup
}

// newNotifier returns a disabled Notifier by default; the caller
// fills URL and Topic when both config keys are present. A nil return
// is the "feature off" signal used by main to skip an allocation.
func newNotifier() *Notifier {
	return &Notifier{HTTP: &http.Client{Timeout: defaultNtfyTimeout}}
}

// Enabled reports whether Send() will actually go out.
func (n *Notifier) Enabled() bool {
	return n != nil && n.URL != "" && n.Topic != ""
}

// Send posts a single notification (title + plain-text body). The
// ntfy server is expected to be reachable; any transport / timeout /
// non-2xx error is logged and swallowed so the batch's outcome is
// independent of the notification path.
//
// The HTTP call runs on a goroutine so the batch loop never blocks on
// a slow server. Callers that are about to exit MUST call Wait()
// afterwards, otherwise the process may terminate mid-request.
func (n *Notifier) Send(title, body string) {
	if !n.Enabled() || body == "" {
		return
	}
	endpoint := strings.TrimRight(n.URL, "/") + "/" + strings.TrimLeft(n.Topic, "/")
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
		if err != nil {
			plog("ntfy: build request: %v", err)
			return
		}
		req.Header.Set("Title", title)
		req.Header.Set("Tags", "inbox_box")
		// Basic auth: user+pass are independent fields on the
		// Notifier so the user can leave them empty for an open
		// server without affecting other ntfy deployments. A
		// half-set credential (only user) is silently treated
		// as "no auth" so we don't ship a broken Authorization
		// header.
		if n.User != "" && n.Pass != "" {
			req.SetBasicAuth(n.User, n.Pass)
		}
		resp, err := n.HTTP.Do(req)
		if err != nil {
			plog("ntfy: POST %s: %v", endpoint, err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			plog("ntfy: %s returned HTTP %s", endpoint, resp.Status)
			return
		}
		plog("ntfy: sent %q (%d bytes body)", title, len(body))
	}()
}

// Wait blocks until every in-flight Send() has finished, or until
// d elapses — whichever comes first. The deadline is a safety net: a
// hung / unreachable ntfy server must never keep the batch process
// alive indefinitely.
//
// Returns true if all sends drained before the deadline.
func (n *Notifier) Wait(d time.Duration) bool {
	if n == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// BuildBatchNotification composes the human-readable body for a batch
// run that produced `entries` (only those with non-empty Packages).
// One block per repo, one line per file, plus a final summary line so
// a phone glance tells you how big the run was.
//
// Empty entries returns ("", "") so the caller can skip the Send
// call entirely.
func BuildBatchNotification(entries []notifyEntry) (title, body string) {
	if len(entries) == 0 {
		return "", ""
	}
	var b strings.Builder
	fileTotal := 0
	for _, e := range entries {
		if e.Tag != "" {
			fmt.Fprintf(&b, "%s %s\n", e.Repo, e.Tag)
		} else {
			fmt.Fprintf(&b, "%s\n", e.Repo)
		}
		for _, p := range e.Packages {
			fmt.Fprintf(&b, "  - %s\n", p)
			fileTotal++
		}
	}
	fmt.Fprintf(&b, "\n%d 个仓库,%d 个新文件", len(entries), fileTotal)
	return "gh-latest 下载完成", b.String()
}
