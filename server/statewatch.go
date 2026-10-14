package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// The folder's state, for phones that cache what's backed up: every 30 s the server checks
// that the files it knows (uploads and files found in the folder) are still on disk, and
// when any disappear or come back it changes a state token. A phone whose token differs
// rechecks everything, so files deleted on the computer stop counting as backed up.

type folderState struct {
	mu      sync.Mutex
	epoch   string // random per run: a restart also makes phones recheck once
	counter int
	missing map[string]bool
	stop    chan struct{}
	done    chan struct{}
}

// StateToken identifies the current state of the folder's files.
func (d *Destination) StateToken() string {
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	return fmt.Sprintf("%s-%d", d.state.epoch, d.state.counter)
}

// missingFiles lists recorded files (uploads and indexed files) that aren't on disk, taking
// compressed copies into account.
func (s *Store) missingFiles() (map[string]bool, error) {
	type rec struct {
		rel  string
		size int64
	}
	var recs []rec
	for _, q := range []string{`SELECT rel_path, size FROM files`, `SELECT rel_path, size FROM scanned`} {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.rel, &r.size); err != nil {
				rows.Close()
				return nil, err
			}
			recs = append(recs, r)
		}
		rows.Close()
	}
	missing := map[string]bool{}
	for _, r := range recs {
		if !s.fileOnDisk(r.rel, r.size) && !s.present(r.rel, r.size) {
			missing[r.rel] = true
		}
	}
	return missing, nil
}

// sweep checks the recorded files and changes the token if the set of missing ones changed.
// It reports false when it couldn't check (the folder is being indexed).
func (d *Destination) sweep() bool {
	if running, _ := d.Indexing(); running {
		return false // the index is being rebuilt; check once it's done
	}
	missing, err := d.store.missingFiles()
	if err != nil {
		return false
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	if d.state.missing != nil && !sameSet(missing, d.state.missing) {
		d.state.counter++
	}
	d.state.missing = missing
	return true
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// StartWatching checks the folder every interval until the destination is closed.
func (d *Destination) StartWatching(interval time.Duration) {
	b := make([]byte, 4)
	rand.Read(b)
	d.state.epoch = hex.EncodeToString(b)
	d.state.stop = make(chan struct{})
	d.state.done = make(chan struct{})
	go func() {
		defer close(d.state.done)
		for {
			next := interval
			if !d.sweep() {
				// Soon after indexing, so the first check (the baseline) isn't 30 s late and a
				// file deleted meanwhile isn't taken as normal.
				next = 2 * time.Second
			}
			select {
			case <-d.state.stop:
				return
			case <-time.After(next):
			}
		}
	}()
}

func (d *Destination) stopWatching() {
	if d.state.stop == nil {
		return
	}
	close(d.state.stop)
	<-d.state.done
	d.state.stop = nil
}
