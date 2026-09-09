package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFilesPresent pins the up_to_date safeguard: if the user deleted
// a file from the download directory, the next batch run must
// re-fetch it instead of trusting the stored URL set. The mechanism
// is a Stat per URL inside runBatch; this test exercises just that
// helper in isolation so the behavior is regression-protected.
func TestFilesPresent(t *testing.T) {
	dir := t.TempDir()

	// Two URLs, both pointing at the same name (the helper keys by
	// filename, which is the only thing that lives on disk).
	urls := []string{
		"https://example.com/anything/Xray-windows-64.zip",
		"https://example.com/anything/sing-box-1.14.0-windows-amd64.zip",
	}

	// Both missing.
	if filesPresent(dir, urls) {
		t.Errorf("expected false when no files exist")
	}

	// One present, one missing.
	if err := os.WriteFile(filepath.Join(dir, "Xray-windows-64.zip"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if filesPresent(dir, urls) {
		t.Errorf("expected false when one file is missing")
	}

	// Both present.
	if err := os.WriteFile(filepath.Join(dir, "sing-box-1.14.0-windows-amd64.zip"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !filesPresent(dir, urls) {
		t.Errorf("expected true when all files exist")
	}

	// Empty URL list is trivially satisfied (the loop never runs).
	if !filesPresent(dir, nil) {
		t.Errorf("expected true for empty url list")
	}
}
