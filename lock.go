package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const lockFileName = "gh-latest.lock"

// acquireLock creates a lock file in the exe directory and returns a release
// function. If the lock file already exists (another instance is running),
// it returns an error so the caller can exit immediately.
//
// The lock file is removed when the returned release func is called. Use
// defer to ensure cleanup on normal completion.
func acquireLock() (func(), error) {
	dir := exeDir()
	if dir == "" {
		dir = "."
	}
	lockPath := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another instance is running (lock: %s)", lockPath)
		}
		return nil, fmt.Errorf("create lock file: %w", err)
	}
	// PID + start time help when debugging stale locks.
	fmt.Fprintf(f, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	_ = f.Close()
	return func() { _ = os.Remove(lockPath) }, nil
}
