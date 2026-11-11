package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestNotifierEndToEnd forces a real download against a throwaway DB and
// verifies the notification actually leaves the process. Not a regular
// unit test — it needs network + a live ntfy. Guarded by an env var so
// `go test ./...` stays hermetic.
func TestNotifierEndToEnd(t *testing.T) {
	dbPath := os.Getenv("NTFY_E2E_DB")
	if dbPath == "" {
		t.Skip("set NTFY_E2E_DB to run the live ntfy end-to-end check")
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	n := newNotifier()
	n.URL = getConfig(db, "ntfy_url", "")
	n.Topic = getConfig(db, "ntfy_topic", "")
	n.User = getConfig(db, "ntfy_user", "")
	n.Pass = getConfig(db, "ntfy_pass", "")
	if !n.Enabled() {
		t.Fatalf("notifier not enabled; check config keys")
	}
	fmt.Printf("notifier target: %s/%s (user=%s)\n", n.URL, n.Topic, n.User)

	err = runBatch(dbPath, batchOpts{
		timeout:     30 * time.Second,
		token:       resolveToken(),
		download:    true,
		downloadDir: getConfig(db, "download_dir", `C:\Users\wangchaojun\AppData\Local\Temp\ntfy_e2e_dl`),
		cacheDir:    os.TempDir(),
		notifier:    n,
	})
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
}
