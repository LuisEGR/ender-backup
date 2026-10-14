package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Indexing files that were already in the destination folder (copied with Image
// Capture, another backup tool, …) so photos the phone has that are already there
// count as backed up and are not uploaded again.

const scannedSchema = `
CREATE TABLE IF NOT EXISTS scanned (
	rel_path TEXT PRIMARY KEY,
	name     TEXT NOT NULL COLLATE NOCASE,
	size     INTEGER NOT NULL,
	mtime    INTEGER NOT NULL,
	sha256   TEXT,             -- computed lazily, only when an upload has the same name and size
	seen     INTEGER NOT NULL  -- scan generation, to drop files that disappeared
);
CREATE INDEX IF NOT EXISTS scanned_name_size ON scanned (name, size);
` + favoritesSchema + conversionsSchema + albumCopiesSchema

// skipDir reports folders that never hold the user's photos: our own index, hidden
// and system folders (.Spotlight-V100, .Trashes, $RECYCLE.BIN, …).
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") || name == "System Volume Information"
}

// ScanFolder records every file under the destination (name, size, mtime). Files
// whose size and mtime are unchanged keep their cached hash; files no longer on
// disk are forgotten. progress is called with the running count.
func (s *Store) ScanFolder(ctx context.Context, progress func(int)) (int, error) {
	// A strictly increasing generation (not the clock, which can repeat between quick scans).
	var gen int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(seen), 0) + 1 FROM scanned`).Scan(&gen); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { tx.Rollback() }() // no-op after a commit
	upsert := `INSERT INTO scanned (rel_path, name, size, mtime, seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(rel_path) DO UPDATE SET name = excluded.name, seen = excluded.seen,
			sha256 = CASE WHEN scanned.size = excluded.size AND scanned.mtime = excluded.mtime THEN scanned.sha256 END,
			size = excluded.size, mtime = excluded.mtime`

	n := 0
	walkErr := filepath.WalkDir(s.root, func(p string, de fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // unreadable entry: skip it, keep scanning
		}
		name := de.Name()
		if de.IsDir() {
			if p != s.root && skipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !de.Type().IsRegular() {
			return nil // hidden files, including macOS "._" resource forks on exFAT drives
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return nil
		}
		if _, err := tx.Exec(upsert, filepath.ToSlash(rel), name, info.Size(), info.ModTime().Unix(), gen); err != nil {
			return err
		}
		n++
		// Commit in small batches: the index has a single connection, so this keeps
		// the phone's requests from waiting on the scan.
		if n%200 == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = s.db.Begin(); err != nil {
				return err
			}
			progress(n)
		}
		return nil
	})
	if walkErr != nil {
		return n, walkErr
	}
	if _, err := tx.Exec(`DELETE FROM scanned WHERE seen <> ?`, gen); err != nil {
		return n, err
	}
	return n, tx.Commit()
}

// FindScannedDuplicate looks for a pre-existing file with the same name and size as
// an upload and, if one exists, compares content by hash (computed once and cached).
func (s *Store) FindScannedDuplicate(name string, size int64, sha string) (string, bool, error) {
	type candidate struct{ rel, sha string }
	var cands []candidate
	rows, err := s.db.Query(`SELECT rel_path, COALESCE(sha256, '') FROM scanned WHERE name = ? AND size = ?`, name, size)
	if err != nil {
		return "", false, err
	}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.rel, &c.sha); err != nil {
			rows.Close()
			return "", false, err
		}
		cands = append(cands, c)
	}
	rows.Close()

	for _, c := range cands {
		if !s.fileOnDisk(c.rel, size) {
			continue
		}
		if c.sha == "" {
			if c.sha, err = hashFile(filepath.Join(s.root, filepath.FromSlash(c.rel))); err != nil {
				continue
			}
			s.db.Exec(`UPDATE scanned SET sha256 = ? WHERE rel_path = ?`, c.sha, c.rel)
		}
		if c.sha == sha {
			return c.rel, true, nil
		}
	}
	return "", false, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scannedMatchQuery extends content matching (MatchesContent) to pre-existing files.
// Their capture time is unknown, so they match on name and size.
// Converted originals (possibly deleted) also match by their original name and size.
const scannedMatchQuery = `SELECT rel_path, created_at FROM files WHERE original_name = ? AND size = ?
	UNION ALL SELECT rel_path, 0 FROM scanned WHERE name = ? AND size = ?
	UNION ALL SELECT src_rel, 0 FROM conversions WHERE src_name = ? AND src_size = ? AND out_rel <> ''`

// indexer runs ScanFolder in the background for a Destination.
type indexer struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running atomic.Bool
	count   atomic.Int64
}

// StartIndexing scans the destination in the background, unless a scan is running.
func (d *Destination) StartIndexing(events *eventLog) {
	d.idx.mu.Lock()
	defer d.idx.mu.Unlock()
	if d.idx.running.Load() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.idx.cancel, d.idx.done = cancel, done
	d.idx.running.Store(true)
	go func() {
		defer close(done)
		defer d.idx.running.Store(false)
		start := time.Now()
		events.add("Indexing files already in %s…", d.root)
		n, err := d.store.ScanFolder(ctx, func(n int) { d.idx.count.Store(int64(n)) })
		d.idx.count.Store(int64(n))
		switch {
		case ctx.Err() != nil:
		case err != nil:
			events.add("Indexing %s failed: %v", d.root, err)
		default:
			events.add("Indexed %d files in %s (%s)", n, d.root, time.Since(start).Round(time.Second))
		}
	}()
}

// Indexing reports whether a scan is running and how many files it has seen.
func (d *Destination) Indexing() (running bool, count int64) {
	return d.idx.running.Load(), d.idx.count.Load()
}

// stopIndexing cancels a running scan and waits for it.
func (d *Destination) stopIndexing() {
	d.idx.mu.Lock()
	cancel, done := d.idx.cancel, d.idx.done
	d.idx.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// waitIndexed blocks until the current scan (if any) finishes. Used by tests.
func (d *Destination) waitIndexed() {
	d.idx.mu.Lock()
	done := d.idx.done
	d.idx.mu.Unlock()
	if done != nil {
		<-done
	}
}
