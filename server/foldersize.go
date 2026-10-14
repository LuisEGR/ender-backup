package main

import (
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// folderSize is the total size of the files in the destination folder, recomputed in the
// background (walking a big folder on a USB drive takes a moment).
type folderSize struct {
	mu      sync.Mutex
	bytes   int64
	known   bool
	at      time.Time
	running bool
}

// FolderBytes returns the folder's size as of the last count (false until the first
// count finishes) and starts a new count if that one is older than 30 seconds.
func (d *Destination) FolderBytes() (int64, bool) {
	f := &d.size
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running && time.Since(f.at) > 30*time.Second {
		f.running = true
		go func() {
			var total int64
			filepath.WalkDir(d.root, func(p string, de fs.DirEntry, err error) error {
				if err == nil && de.Type().IsRegular() {
					if info, err := de.Info(); err == nil {
						total += info.Size()
					}
				}
				return nil
			})
			f.mu.Lock()
			f.bytes, f.known, f.at, f.running = total, true, time.Now(), false
			f.mu.Unlock()
		}()
	}
	return f.bytes, f.known
}
