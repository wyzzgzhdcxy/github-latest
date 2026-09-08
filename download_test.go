package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilenameFromURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://github.com/SagerNet/sing-box/releases/download/v1.14.0/sing-box-1.14.0-windows-amd64.zip", "sing-box-1.14.0-windows-amd64.zip"},
		{"https://example.com/file.tar.gz?ref=abc", "file.tar.gz"},
		{"https://example.com/file.tar.gz#fragment", "file.tar.gz"},
		{"https://example.com", "example.com"}, // no path → use host
		{"", "download"},                        // empty → "download" fallback
	}
	for _, tt := range tests {
		got := filenameFromURL(tt.in)
		if got != tt.want {
			t.Errorf("filenameFromURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDownloadAll_SkipAndMove exercises end-to-end download with a local
// HTTP server: 2 files are fetched, then re-running skips them, and the
// cache directory is left empty after the moves.
func TestDownloadAll_SkipAndMove(t *testing.T) {
	resetProxy(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/a.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PAYLOAD-A"))
	})
	mux.HandleFunc("/b.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PAYLOAD-B"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	resultPath := filepath.Join(tmp, "result.txt")
	if err := os.WriteFile(resultPath, []byte(
		"# test batch\n"+srv.URL+"/a.zip\n"+srv.URL+"/b.zip\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	dlDir := filepath.Join(tmp, "dl")
	cacheDir := filepath.Join(tmp, "cache")

	if err := downloadFromResultFile(resultPath, dlDir, cacheDir); err != nil {
		t.Fatal(err)
	}

	for _, fname := range []string{"a.zip", "b.zip"} {
		target := filepath.Join(dlDir, fname)
		data, err := os.ReadFile(target)
		if err != nil {
			t.Errorf("%s missing in download dir: %v", fname, err)
			continue
		}
		want := "PAYLOAD-" + strings.ToUpper(fname[:1])
		if string(data) != want {
			t.Errorf("%s content = %q, want %q", fname, data, want)
		}
	}

	entries, _ := os.ReadDir(cacheDir)
	if len(entries) != 0 {
		t.Errorf("cache dir not cleaned: %v", entries)
	}

	if err := downloadFromResultFile(resultPath, dlDir, cacheDir); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dlDir)
	if len(entries) != 2 {
		t.Errorf("download dir has %d files, want 2", len(entries))
	}
}

// TestDownloadOne_Progress ensures downloadOne writes the file with the
// right contents. Progress display goes to stderr and is not asserted.
func TestDownloadOne_Progress(t *testing.T) {
	resetProxy(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	dest, err := downloadOne(srv.URL+"/greeting.txt", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("got %q, want %q", data, "hello world")
	}
}

// TestNewHTTPClient_NoProxy asserts the transport wiring for --no-proxy:
// when noProxy is true, the transport's Proxy function must be nil. When
// false, it must be a non-nil proxyFunc.
func TestNewHTTPClient_NoProxy(t *testing.T) {
	resetProxy(t)
	SetProxyConfig(true, "")
	trNo, ok := newHTTPClient(5 * time.Second).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", trNo)
	}
	if trNo.Proxy != nil {
		t.Errorf("noProxy=true: transport.Proxy should be nil")
	}

	resetProxy(t)
	trYes, ok := newHTTPClient(5 * time.Second).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", trYes)
	}
	if trYes.Proxy == nil {
		t.Errorf("noProxy=false: transport.Proxy should be a function (proxyFunc)")
	}
}

// TestProxyFunc_ExplicitFlag verifies that --proxy URL overrides env
// vars: regardless of HTTP_PROXY in the environment, the explicit URL
// is what proxyFunc returns.
func TestProxyFunc_ExplicitFlag(t *testing.T) {
	resetProxy(t)
	t.Setenv("HTTP_PROXY", "http://from-env:1234")
	SetProxyConfig(false, "http://explicit-flag:5678")

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/foo", nil)
	u, err := proxyFunc(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected non-nil proxy URL")
	}
	if u.Host != "explicit-flag:5678" {
		t.Errorf("expected host=explicit-flag:5678, got %q", u.Host)
	}
}

// TestProxyFunc_EnvVarFallback verifies that with no --proxy flag, the
// function falls back to HTTP_PROXY env var. (Important: http.ProxyFromEnvironment
// caches env at first call, so proxyFunc reads os.Getenv fresh on every
// request — this test would have failed against the cached API.)
func TestProxyFunc_EnvVarFallback(t *testing.T) {
	resetProxy(t)
	t.Setenv("HTTP_PROXY", "http://from-env:1234")
	t.Setenv("HTTPS_PROXY", "http://from-env:1234")
	explicitProxyURL = ""

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/foo", nil)
	u, err := proxyFunc(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected non-nil proxy URL")
	}
	if u.Host != "from-env:1234" {
		t.Errorf("expected host=from-env:1234, got %q", u.Host)
	}
}

// resetProxy resets the global proxy config so a test starts from a
// known state. Called automatically by every test in this file.
func resetProxy(t *testing.T) {
	t.Helper()
	SetProxyConfig(false, "")
	t.Cleanup(func() { SetProxyConfig(false, "") })
}

// TestMoveFile verifies that moveFile works on a single volume (rename
// path) and removes the source. Cross-drive fallback is exercised by
// the live E:\github_app run, not by a unit test.
func TestMoveFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst")
	if err := os.WriteFile(src, []byte("payload-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}

	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dst not present after move: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src still present after move: %v", err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "payload-bytes" {
		t.Errorf("dst content = %q, want %q", data, "payload-bytes")
	}
}
