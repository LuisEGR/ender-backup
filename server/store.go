package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the metadata index. It records every file received from a device
// and which assets are complete, so the phone can learn what is safe to delete.
type Store struct {
	db   *sql.DB
	root string // destination folder; file rel_paths are relative to it
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	folder     TEXT NOT NULL DEFAULT '' COLLATE NOCASE, -- subfolder of the destination, fixed on first upload
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL
);
-- One row per photo/video (a PHAsset on iOS).
CREATE TABLE IF NOT EXISTS assets (
	device_id              TEXT NOT NULL,
	asset_id               TEXT NOT NULL,
	media_type             TEXT NOT NULL DEFAULT '',
	created_at             INTEGER NOT NULL DEFAULT 0,
	completed_at           INTEGER,
	deleted_from_device_at INTEGER,
	PRIMARY KEY (device_id, asset_id)
);
-- One row per resource of an asset (e.g. a Live Photo has a photo + a video).
CREATE TABLE IF NOT EXISTS files (
	device_id     TEXT NOT NULL,
	asset_id      TEXT NOT NULL,
	resource      TEXT NOT NULL,
	original_name TEXT NOT NULL,
	rel_path      TEXT NOT NULL,
	size          INTEGER NOT NULL,
	sha256        TEXT NOT NULL,
	uploaded_at   INTEGER NOT NULL,
	created_at    INTEGER NOT NULL DEFAULT 0, -- capture time sent by the phone (0 = unknown)
	PRIMARY KEY (device_id, asset_id, resource)
);
CREATE INDEX IF NOT EXISTS files_sha256 ON files (sha256, size);
`

// migrations bring databases created by older versions up to date.
var migrations = []struct{ table, column, ddl string }{
	{"files", "created_at", `ALTER TABLE files ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`},
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(m.ddl); err != nil {
				return err
			}
		}
	}
	// Created after the migrations so it can use new columns.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS files_name_size ON files (original_name, size)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS files_uploaded ON files (uploaded_at)`); err != nil {
		return err
	}
	if _, err := db.Exec(scannedSchema); err != nil {
		return err
	}
	// Columns added to tables that scannedSchema creates.
	for _, m := range []struct{ table, column, ddl string }{
		{"conversions", "lost", `ALTER TABLE conversions ADD COLUMN lost TEXT NOT NULL DEFAULT ''`},
		{"conversions", "level", `ALTER TABLE conversions ADD COLUMN level TEXT NOT NULL DEFAULT ''`},
		{"conversions", "vmaf", `ALTER TABLE conversions ADD COLUMN vmaf REAL NOT NULL DEFAULT 0`},
		{"conversions", "settings", `ALTER TABLE conversions ADD COLUMN settings TEXT NOT NULL DEFAULT ''`},
		{"conversions", "source", `ALTER TABLE conversions ADD COLUMN source TEXT NOT NULL DEFAULT ''`},
		{"conversions", "search_secs", `ALTER TABLE conversions ADD COLUMN search_secs REAL NOT NULL DEFAULT 0`},
		{"conversions", "encode_secs", `ALTER TABLE conversions ADD COLUMN encode_secs REAL NOT NULL DEFAULT 0`},
		{"conversions", "restore", `ALTER TABLE conversions ADD COLUMN restore INTEGER NOT NULL DEFAULT 0`}, // original asked back (restore.go)
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(m.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func OpenStore(dbPath, root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	st := &Store{db: db, root: root}
	st.redoOldVideoCopies()
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Meta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) TouchDevice(id, name string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO devices (id, name, first_seen, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen,
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE devices.name END`,
		id, name, now, now)
	return err
}

// DeviceFolder returns the subfolder a device's files are stored in. It is chosen
// from the device name on first use and then never changes, so renaming the phone
// doesn't split its backup. Two devices with the same name get "iPhone", "iPhone (2)".
func (s *Store) DeviceFolder(id, name string) (string, error) {
	var folder string
	err := s.db.QueryRow(`SELECT folder FROM devices WHERE id = ?`, id).Scan(&folder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if folder != "" {
		return folder, nil
	}
	base := sanitizeFilename(name)
	if base == "" || strings.HasPrefix(base, ".") {
		base = "device-" + sanitizeFilename(id)
		if len(base) > 15 {
			base = base[:15]
		}
	}
	for i := 1; ; i++ {
		folder = base
		if i > 1 {
			folder = fmt.Sprintf("%s (%d)", base, i)
		}
		var taken int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE folder = ? AND id <> ?`, folder, id).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			break
		}
	}
	now := time.Now().Unix()
	_, err = s.db.Exec(`INSERT INTO devices (id, name, folder, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET folder = excluded.folder`, id, name, folder, now, now)
	return folder, err
}

type FileRecord struct {
	DeviceID     string
	AssetID      string
	Resource     string
	OriginalName string
	RelPath      string
	Size         int64
	SHA256       string
	CreatedAt    int64 // capture time, unix seconds (0 = unknown)
}

func (s *Store) PutFile(f FileRecord) error {
	_, err := s.db.Exec(`INSERT INTO files
		(device_id, asset_id, resource, original_name, rel_path, size, sha256, uploaded_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id, asset_id, resource) DO UPDATE SET
			original_name = excluded.original_name, rel_path = excluded.rel_path,
			size = excluded.size, sha256 = excluded.sha256, uploaded_at = excluded.uploaded_at,
			created_at = excluded.created_at`,
		f.DeviceID, f.AssetID, f.Resource, f.OriginalName, f.RelPath, f.Size, f.SHA256, time.Now().Unix(), f.CreatedAt)
	return err
}

// FindByContent returns the rel_path of an already-stored file with identical
// content, from any phone, so the same photo is only ever stored once.
func (s *Store) FindByContent(sha string, size int64) (string, bool, error) {
	rels, err := s.strings(`SELECT rel_path FROM files WHERE sha256 = ? AND size = ?`, sha, size)
	if err != nil {
		return "", false, err
	}
	for _, rel := range rels {
		if s.present(rel, size) {
			return rel, true, nil
		}
	}
	return "", false, nil
}

// strings runs a one-column query and returns all values. Reading everything before
// using the results matters: the index has a single connection, so querying again while
// rows are open would deadlock.
func (s *Store) strings(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ResourceFingerprint identifies a file on the phone without reading it.
type ResourceFingerprint struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// MatchesContent reports whether every resource of an asset is already stored
// (from any phone) as a file with the same name and size, captured at the same
// time when both sides know it. This recognises photos after an app reinstall,
// on a new phone, or when two phones share photos.
func (s *Store) MatchesContent(resources []ResourceFingerprint, createdAt int64) (bool, error) {
	paths, err := s.ContentMatch(resources, createdAt)
	return len(paths) > 0, err
}

// ContentMatch is MatchesContent returning the matching file of each resource.
func (s *Store) ContentMatch(resources []ResourceFingerprint, createdAt int64) ([]string, error) {
	if len(resources) == 0 {
		return nil, nil
	}
	var paths []string
	for _, r := range resources {
		name := sanitizeFilename(r.Name)
		if name == "" || r.Size <= 0 {
			return nil, nil
		}
		rows, err := s.db.Query(scannedMatchQuery, name, r.Size, name, r.Size, name, r.Size)
		if err != nil {
			return nil, err
		}
		var candidates []string
		for rows.Next() {
			var rel string
			var created int64
			if err := rows.Scan(&rel, &created); err != nil {
				rows.Close()
				return nil, err
			}
			if created == 0 || createdAt == 0 || created == createdAt {
				candidates = append(candidates, rel)
			}
		}
		rows.Close()
		found := ""
		for _, rel := range candidates {
			if s.present(rel, r.Size) {
				found = rel
				break
			}
		}
		if found == "" {
			return nil, nil
		}
		paths = append(paths, found)
	}
	return paths, nil
}

func (s *Store) fileOnDisk(rel string, size int64) bool {
	st, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel)))
	return err == nil && st.Mode().IsRegular() && st.Size() == size
}

// CommitAsset marks an asset complete once every listed resource has been
// received and is present on disk. It returns the resources that are missing.
func (s *Store) CommitAsset(deviceID, assetID, mediaType string, createdAt int64, resources []string) ([]string, error) {
	if len(resources) == 0 {
		return nil, errors.New("no resources listed")
	}
	var missing []string
	for _, r := range resources {
		var rel string
		var size int64
		err := s.db.QueryRow(`SELECT rel_path, size FROM files
			WHERE device_id = ? AND asset_id = ? AND resource = ?`, deviceID, assetID, r).Scan(&rel, &size)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !s.present(rel, size)) {
			missing = append(missing, r)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		return missing, nil
	}
	_, err := s.db.Exec(`INSERT INTO assets (device_id, asset_id, media_type, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_id, asset_id) DO UPDATE SET media_type = excluded.media_type,
			created_at = excluded.created_at, completed_at = excluded.completed_at`,
		deviceID, assetID, mediaType, createdAt, time.Now().Unix())
	return nil, err
}

// BackedUp returns the subset of assetIDs that are complete AND whose files are
// all still present on disk with the recorded size — i.e. safe to delete on the phone.
func (s *Store) BackedUp(deviceID string, assetIDs []string) ([]string, error) {
	out := []string{}
	for _, id := range assetIDs {
		ok, err := s.isBackedUp(deviceID, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Store) isBackedUp(deviceID, assetID string) (bool, error) {
	paths, err := s.assetFiles(deviceID, assetID)
	return len(paths) > 0, err
}

// assetFiles returns the files of a committed asset this device uploaded, or nil
// unless all of them are still on disk.
func (s *Store) assetFiles(deviceID, assetID string) ([]string, error) {
	var completed sql.NullInt64
	err := s.db.QueryRow(`SELECT completed_at FROM assets WHERE device_id = ? AND asset_id = ?`,
		deviceID, assetID).Scan(&completed)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !completed.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT rel_path, size FROM files WHERE device_id = ? AND asset_id = ?`, deviceID, assetID)
	if err != nil {
		return nil, err
	}
	type file struct {
		rel  string
		size int64
	}
	var files []file
	for rows.Next() {
		var f file
		if err := rows.Scan(&f.rel, &f.size); err != nil {
			rows.Close()
			return nil, err
		}
		files = append(files, f)
	}
	rows.Close()
	var paths []string
	for _, f := range files {
		if !s.present(f.rel, f.size) {
			return nil, nil
		}
		paths = append(paths, f.rel)
	}
	return paths, nil
}

func (s *Store) MarkDeleted(deviceID string, assetIDs []string) error {
	now := time.Now().Unix()
	for _, id := range assetIDs {
		if _, err := s.db.Exec(`UPDATE assets SET deleted_from_device_at = ?
			WHERE device_id = ? AND asset_id = ?`, now, deviceID, id); err != nil {
			return err
		}
	}
	return nil
}

type DeviceStats struct {
	ID, Name, Folder string
	LastSeen         time.Time
	Assets, Deleted  int64
	Files, Bytes     int64
	LastBackup       int64 // unix time of the newest file received, 0 if none
}

func (s *Store) Stats() ([]DeviceStats, error) {
	rows, err := s.db.Query(`SELECT d.id, d.name, d.folder, d.last_seen,
		(SELECT COUNT(*) FROM assets a WHERE a.device_id = d.id AND a.completed_at IS NOT NULL),
		(SELECT COUNT(*) FROM assets a WHERE a.device_id = d.id AND a.deleted_from_device_at IS NOT NULL),
		(SELECT COUNT(*) FROM files f WHERE f.device_id = d.id),
		(SELECT COALESCE(SUM(size), 0) FROM files f WHERE f.device_id = d.id),
		(SELECT COALESCE(MAX(uploaded_at), 0) FROM files f WHERE f.device_id = d.id)
		FROM devices d ORDER BY d.last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceStats
	for rows.Next() {
		var d DeviceStats
		var last int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Folder, &last, &d.Assets, &d.Deleted, &d.Files, &d.Bytes, &d.LastBackup); err != nil {
			return nil, err
		}
		d.LastSeen = time.Unix(last, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecentFile is a file a phone sent, for the overview's "Recently backed up".
type RecentFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	At     int64  `json:"at"` // unix time it arrived
	Device string `json:"device"`
}

// RecentFiles lists the newest files received, newest first.
func (s *Store) RecentFiles(n int) ([]RecentFile, error) {
	rows, err := s.db.Query(`SELECT f.original_name, f.size, f.uploaded_at, COALESCE(d.name, '')
		FROM files f LEFT JOIN devices d ON d.id = f.device_id
		ORDER BY f.uploaded_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecentFile{}
	for rows.Next() {
		var f RecentFile
		if err := rows.Scan(&f.Name, &f.Size, &f.At, &f.Device); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MissingFiles lists recorded files that no longer exist on disk (or changed size).
func (s *Store) MissingFiles() ([]FileRecord, error) {
	rows, err := s.db.Query(`SELECT device_id, asset_id, resource, original_name, rel_path, size, sha256 FROM files`)
	if err != nil {
		return nil, err
	}
	var all []FileRecord
	for rows.Next() {
		var f FileRecord
		if err := rows.Scan(&f.DeviceID, &f.AssetID, &f.Resource, &f.OriginalName, &f.RelPath, &f.Size, &f.SHA256); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, f)
	}
	rows.Close()
	var out []FileRecord
	for _, f := range all {
		if !s.present(f.RelPath, f.Size) {
			out = append(out, f)
		}
	}
	return out, nil
}

// redoOldVideoCopies runs once: AV1 copies made before the server tracked what a copy
// can't keep (and before it kept cover images) are deleted, so they're made again — and
// their originals are protected when needed. Videos whose original is already gone stay.
func (s *Store) redoOldVideoCopies() {
	var done string
	if s.db.QueryRow(`SELECT value FROM meta WHERE key = 'av1_redo_v2'`).Scan(&done) == nil {
		return
	}
	rels, _ := s.strings(`SELECT out_rel FROM conversions WHERE format = 'av1' AND out_rel <> '' AND src_removed = 0`)
	for _, rel := range rels {
		os.Remove(filepath.Join(s.root, filepath.FromSlash(rel)))
	}
	s.db.Exec(`DELETE FROM conversions WHERE format = 'av1' AND src_removed = 0`)
	s.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('av1_redo_v2', '1')`)
}
