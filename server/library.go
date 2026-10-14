package main

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LibrarySummary is what the backup folder holds, for the settings page: photos and
// videos (originals, whether sent by the app or already in the folder) and what the
// compressed copies weigh against their originals.
type LibrarySummary struct {
	Photos      MediaCount `json:"photos"`
	Videos      MediaCount `json:"videos"`
	PhotoCopies CopyCount  `json:"photo_copies"`
	VideoCopies CopyCount  `json:"video_copies"`
}

// MediaCount: Count photos (or videos) are backed up, whether as the original, a
// compressed copy, or both; Bytes is what their originals still on disk take, CopyBytes
// what their compressed copies take.
type MediaCount struct {
	Count     int64 `json:"count"`
	Bytes     int64 `json:"bytes"`
	CopyBytes int64 `json:"copy_bytes"`
}

type CopyCount struct {
	Count   int64 `json:"count"`
	Before  int64 `json:"before"`  // the originals' size
	After   int64 `json:"after"`   // the copies' size
	Removed int64 `json:"removed"` // originals deleted after compressing
}

var photoExts = map[string]bool{".heic": true, ".heif": true, ".jpg": true, ".jpeg": true, ".png": true,
	".dng": true, ".gif": true, ".webp": true, ".tif": true, ".tiff": true, ".raw": true}

// isCopy reports compressed copies made by this server (counted with their originals).
func isCopy(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".avif") || strings.HasSuffix(n, ".av1.mp4")
}

func (s *Store) LibrarySummary() (LibrarySummary, error) {
	var sum LibrarySummary
	type conv struct {
		out     int64
		removed bool
	}
	convs := map[string]conv{}
	counted := map[string]bool{} // copies shared by duplicate originals count once (dedupe.go)
	crow, err := s.db.Query(`SELECT src_rel, out_rel, format, src_size, out_size, src_removed FROM conversions WHERE out_rel <> ''`)
	if err != nil {
		return sum, err
	}
	for crow.Next() {
		var rel, out, format string
		var before, after int64
		var removed bool
		if err := crow.Scan(&rel, &out, &format, &before, &after, &removed); err != nil {
			crow.Close()
			return sum, err
		}
		if counted[strings.ToLower(out)] {
			// a duplicate original sharing a copy already counted
			convs[strings.ToLower(rel)] = conv{0, removed}
			continue
		}
		counted[strings.ToLower(out)] = true
		convs[strings.ToLower(rel)] = conv{after, removed}
		c := &sum.PhotoCopies
		if format == "av1" {
			c = &sum.VideoCopies
		}
		c.Count++
		c.Before += before
		c.After += after
		if removed {
			c.Removed++
		}
	}
	crow.Close()
	if err := crow.Err(); err != nil {
		return sum, err
	}

	kind := func(rel string) *MediaCount {
		ext := strings.ToLower(filepath.Ext(rel))
		switch {
		case photoExts[ext]:
			return &sum.Photos
		case videoExts[ext]:
			return &sum.Videos
		}
		return nil
	}
	// Originals the folder knows about (the app's uploads and files found in the folder).
	// An original deleted after compressing is still listed with the app's uploads: it
	// counts once, by its copy.
	seen := map[string]bool{}
	rows, err := s.db.Query(`SELECT rel_path, size FROM files UNION ALL SELECT rel_path, size FROM scanned`)
	if err != nil {
		return sum, err
	}
	for rows.Next() {
		var rel string
		var size int64
		if err := rows.Scan(&rel, &size); err != nil {
			rows.Close()
			return sum, err
		}
		key := strings.ToLower(rel)
		if seen[key] || isCopy(rel) {
			continue
		}
		seen[key] = true
		m := kind(rel)
		if m == nil {
			continue
		}
		m.Count++
		if c, ok := convs[key]; ok {
			m.CopyBytes += c.out
			if c.removed {
				continue // the original is gone
			}
		}
		m.Bytes += size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return sum, err
	}
	// Copies whose original was deleted and isn't listed any more.
	for key, c := range convs {
		if seen[key] {
			continue
		}
		if m := kind(key); m != nil {
			m.Count++
			m.CopyBytes += c.out
		}
	}
	return sum, nil
}

// libraryCache keeps the summary for a few seconds: the settings page asks every 2 s.
type libraryCache struct {
	mu   sync.Mutex
	root string
	at   time.Time
	val  LibrarySummary
}

func (c *libraryCache) get(d *Destination) LibrarySummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root == d.root && time.Since(c.at) < 10*time.Second {
		return c.val
	}
	if v, err := d.store.LibrarySummary(); err == nil {
		c.root, c.at, c.val = d.root, time.Now(), v
	}
	return c.val
}
