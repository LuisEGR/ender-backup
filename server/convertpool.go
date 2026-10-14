package main

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Running conversions: as many at once as the computer has CPU threads (each encode uses
// one), started as soon as one finishes, within a memory budget so large photos (a 72 MP
// panorama needs ~1.7 GB while converting) don't exhaust memory. The conversions run in a
// low-priority helper process (convertproc.go), so they only use CPU nothing else wants.

// convertWorkers is how many photos are converted at once.
func convertWorkers() int { return max(1, runtime.NumCPU()) }

// convertMemoryBudget caps the estimated memory of conversions in progress.
const convertMemoryBudget = 4 << 30

// convertMemory estimates the memory needed to convert a photo, from its pixel count
// (decoded image, 4:2:0 copy and encoder buffers).
func convertMemory(path string) int64 {
	const fallback = 300 << 20
	f, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer f.Close()
	var cfg image.Config
	switch strings.ToLower(filepath.Ext(path)) {
	case ".heic", ".heif":
		cfg.Width, cfg.Height, err = heifSize(f)
	default:
		cfg, _, err = image.DecodeConfig(f)
	}
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return fallback
	}
	return int64(cfg.Width) * int64(cfg.Height) * 24
}

// runConversions converts pending photos until there are none, settings turn compression
// off, or the converter is stopped. It returns false when stopped.
func (d *Destination) runConversions(settings func() CompressSettings, events *eventLog, wait func(time.Duration) bool) bool {
	workers := convertWorkers()
	type finished struct {
		memory int64
		slots  int
	}
	done := make(chan finished, workers)
	inFlight, memory := 0, int64(0) // inFlight counts worker slots in use
	// Stopping the converter cancels conversions in progress (ffmpeg is killed).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-d.conv.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	d.conv.progress.beginRun()
	complete := false
	defer func() { d.conv.progress.endRun(complete) }() // runs last, after waiting for conversions
	lastCleanup := time.Time{}
	finishOne := func() bool {
		select {
		case <-d.conv.stop:
			return false
		case f := <-done:
			inFlight -= f.slots
			memory -= f.memory
			return true
		}
	}
	defer func() { // let conversions in progress finish (stopping waits for them)
		for inFlight > 0 {
			f := <-done
			inFlight -= f.slots
			memory -= f.memory
		}
	}()
	for {
		s := settings()
		photos, videos := s.kinds()
		if running, _ := d.Indexing(); running || !s.active() {
			if inFlight > 0 {
				if !finishOne() {
					return false
				}
				continue
			}
			complete = !s.active() // turned off: a new run starts over; indexing only pauses it
			return true
		}
		if s.RemoveOriginals && time.Since(lastCleanup) > 30*time.Second {
			lastCleanup = time.Now()
			if n, err := d.store.removeConvertedOriginals(); err != nil {
				events.add("Removing originals failed: %v", err)
			} else if n > 0 {
				events.add("Deleted %d originals that have a converted copy", n)
			}
		}
		if inFlight >= workers {
			if !finishOne() {
				return false
			}
			continue
		}
		// Next photo not already being converted.
		pending, err := d.store.nextConversions(workers+inFlight+1, photos, videos)
		if err != nil {
			events.add("Compression: %v", err)
			if inFlight > 0 {
				if !finishOne() {
					return false
				}
				continue
			}
			return wait(time.Minute)
		}
		var next *convertCandidate
		d.conv.mu.Lock()
		for i := range pending {
			if !d.conv.current[pending[i].rel] {
				next = &pending[i]
				break
			}
		}
		d.conv.mu.Unlock()
		if next == nil {
			if inFlight > 0 {
				if !finishOne() {
					return false
				}
				continue
			}
			complete = true
			return true // all done
		}
		// A video is encoded alone (ffmpeg uses every CPU thread itself).
		slots, need := 1, int64(0)
		if isVideo(next.rel) {
			slots = workers
		} else {
			need = min(convertMemory(filepath.Join(d.root, filepath.FromSlash(next.rel))), convertMemoryBudget)
		}
		if inFlight > 0 && (memory+need > convertMemoryBudget || inFlight+slots > workers) {
			if !finishOne() { // wait for memory to free up
				return false
			}
			continue
		}
		c := *next
		d.conv.mu.Lock()
		if d.conv.current == nil {
			d.conv.current, d.conv.claimed = map[string]bool{}, map[string]bool{}
		}
		d.conv.current[c.rel] = true
		d.conv.mu.Unlock()
		inFlight += slots
		memory += need
		d.conv.progress.start(c.rel, c.size, isVideo(c.rel))
		go func() {
			d.convertOne(ctx, c, s, events)
			d.conv.progress.finish(c.rel, ctx.Err() == nil)
			d.conv.mu.Lock()
			delete(d.conv.current, c.rel)
			d.conv.mu.Unlock()
			done <- finished{need, slots}
		}()
		select {
		case <-d.conv.stop:
			return false
		default:
		}
	}
}

// heifSize returns a HEIC's pixel size from its "ispe" properties without decoding it
// (the largest one: a photo is often stored as a grid of tiles plus the full size).
func heifSize(r io.Reader) (int, int, error) {
	head := make([]byte, 1<<20) // the metadata is at the start of the file
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	w, h := 0, 0
	for i := 4; i+16 <= len(head); i++ {
		if string(head[i:i+4]) != "ispe" {
			continue
		}
		pw := int(binary.BigEndian.Uint32(head[i+8:]))
		ph := int(binary.BigEndian.Uint32(head[i+12:]))
		if pw*ph > w*h && pw < 1<<16 && ph < 1<<16 {
			w, h = pw, ph
		}
	}
	if w == 0 {
		return 0, 0, errors.New("no image size")
	}
	return w, h, nil
}
