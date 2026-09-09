package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion lets migrate() apply each upgrade exactly once. Bump it
// whenever you change the urls table layout. Each row in the urls table
// represents one (release_url, type) pair with a single pkg filename.
//
// History:
//   v1 — current: (release_url, type, pkg, download_url, updated_at, status)
//
// The on-disk schema_version table may also contain v2 / v3 from an
// earlier (windows_pkg / linux_pkg / mac_pkg) shape; those entries are
// treated as already-rolled-past and ignored. createURLsTable is the
// source of truth for the current layout.
const schemaVersion = 1

const createURLsTable = `
CREATE TABLE IF NOT EXISTS urls (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    release_url  TEXT    NOT NULL,
    type         TEXT,
    pkg          TEXT,
    download_url TEXT,
    updated_at   TEXT,
    status       TEXT
);
`

// One row per (release_url, type) pair so the same repo can be tracked
// independently for win / linux / mac. UNIQUE keeps re-inserts idempotent
// (so external tooling that re-syncs a config can run freely).
const createURLsUniqueIdx = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_urls_release_type
    ON urls(release_url, type);
`

const createSchemaVersionTable = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY
);
`

// config holds global key/value overrides (download_dir, proxy, ...)
// so the user can persist settings in the DB instead of repeating CLI
// flags. Empty value still replaces; to skip proxy entirely, use the
// --no-proxy flag.
const createConfigTable = `
CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value TEXT
);
`

type urlRow struct {
	ID          int64
	ReleaseURL  string
	Type        string
	Pkg         string
	DownloadURL string
	UpdatedAt   string
	Status      string
}

// openDB opens the SQLite database at path, creating the parent directory
// if needed and ensuring the urls table + latest schema exist. The caller
// must Close().
func openDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db %s: %w", path, err)
	}
	if _, err := db.Exec(createSchemaVersionTable); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema_version table: %w", err)
	}
	if _, err := db.Exec(createURLsTable); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create urls table: %w", err)
	}
	if _, err := db.Exec(createURLsUniqueIdx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create urls unique index: %w", err)
	}
	if _, err := db.Exec(createConfigTable); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create config table: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	return db, nil
}

// migrate brings older databases up to schemaVersion. The current
// canonical shape (v1) is what createURLsTable already writes, so a DB
// freshly opened against this binary needs no column work — we just
// ensure a v1 row exists in schema_version. Higher recorded versions
// (v2/v3 from the abandoned windows_pkg / linux_pkg / mac_pkg design)
// are left on disk untouched and treated as "rolled past".
func migrate(db *sql.DB) error {
	var current int
	row := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if current >= schemaVersion {
		return nil
	}
	// Any future v1 → v2 migration steps would go here, guarded by
	// hasColumn / addColumnIfMissing like before.
	if _, err := db.Exec(`INSERT OR REPLACE INTO schema_version(version) VALUES (?)`, schemaVersion); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) bool {
	var n int
	row := db.QueryRow(
		fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info(%q) WHERE name = ?`, table),
		column,
	)
	return row.Scan(&n) == nil && n > 0
}

// listURLs returns every row in the urls table in id order, with empty
// strings substituted for NULL columns.
func listURLs(db *sql.DB) ([]urlRow, error) {
	const q = `SELECT id, release_url,
	                 COALESCE(type,         ''),
	                 COALESCE(pkg,          ''),
	                 COALESCE(download_url, ''),
	                 COALESCE(updated_at,   ''),
	                 COALESCE(status,       '')
	            FROM urls
	           ORDER BY id`
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("query urls: %w", err)
	}
	defer rows.Close()
	var out []urlRow
	for rows.Next() {
		var r urlRow
		if err := rows.Scan(&r.ID, &r.ReleaseURL, &r.Type, &r.Pkg,
			&r.DownloadURL, &r.UpdatedAt, &r.Status); err != nil {
			return nil, fmt.Errorf("scan url row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// markFetchError records that a release URL could not be fetched
// (network, API, parse, etc.). The error is per-row because the row
// is the unit of work: even if all rows share the same release_url,
// each one deserves its own "fetch failed" timestamp.
func markFetchError(db *sql.DB, rowID int64, errMsg string) error {
	return updateResult(db, rowID, "fetch_error: "+truncate(errMsg, 180))
}

// updateResult records the latest status + timestamp on a single row
// (identified by id). status is free-form but conventionally one of:
//   "success", "partial", "failed", "failed (not_found: ...)",
//   "skipped", "skipped (no_config)", "skipped (up_to_date)",
//   "fetch_error: ...", "parse_error: ...".
//
// Updating by id (not by release_url) keeps the (release_url, type)
// rows independent: a linux row's "not_found" must not overwrite a
// win row's "success" on the same repo, and a linux row's download
// URL must not clobber the win row's URL.
func updateResult(db *sql.DB, rowID int64, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE urls
		    SET updated_at = ?,
		        status     = ?
		  WHERE id = ?`,
		now, status, rowID,
	)
	if err != nil {
		return fmt.Errorf("update row %d: %w", rowID, err)
	}
	return nil
}

// markDownloaded records the URLs that were just downloaded for this
// row (CSV) and updates the status + timestamp on that one row. Pass
// an empty downloadURLs to leave the previous value intact (e.g.
// status-only updates).
func markDownloaded(db *sql.DB, rowID int64, downloadURLs, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE urls
		    SET download_url = COALESCE(?, download_url),
		        updated_at   = ?,
		        status       = ?
		  WHERE id = ?`,
		nullableString(downloadURLs), now, status, rowID,
	)
	if err != nil {
		return fmt.Errorf("mark downloaded row %d: %w", rowID, err)
	}
	return nil
}

// configuredPackages returns the non-empty, trimmed pkg filename the
// user has pre-configured for THIS (release_url, type) row, or nil if
// none. Returns a single-element slice so callers can treat it the same
// way as the old three-platform model.
func (r urlRow) configuredPackages() []string {
	pkg := strings.TrimSpace(r.Pkg)
	if pkg == "" {
		return nil
	}
	return []string{pkg}
}

// splitStoredDownloadURL returns the non-empty set of URLs we recorded on
// the last successful download for this row. Empty cells are ignored.
func (r urlRow) splitStoredDownloadURL() []string {
	if r.DownloadURL == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(r.DownloadURL, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// normalizeRowType maps a user-friendly type label to the canonical
// short form stored in the DB. Empty input returns "" so an "I just
// want a row, no specific platform" insert is still allowed.
func normalizeRowType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "win", "windows", "win64", "win32", "win7":
		return "win"
	case "linux", "lin":
		return "linux"
	case "mac", "macos", "darwin", "osx":
		return "mac"
	}
	return t
}

// getConfig returns the value stored under key, or fallback if the key
// is absent / the read fails. The DB must already be open.
func getConfig(db *sql.DB, key, fallback string) string {
	var v string
	err := db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v
}

// setConfig writes value under key, replacing any existing value. Use
// empty value to record "use the default for this key" (e.g. proxy="").
// To skip the proxy entirely, set config key `no_proxy` to "true".
func setConfig(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO config(key, value) VALUES(?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set config %s: %w", key, err)
	}
	return nil
}
