package main

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Live progress of a compression run, for the settings page and the app: how many items
// this run has done out of how many, what's being converted right now (for a video, how
// far and in which phase), and roughly how long is left.

// Video phases: the CRF search (encoding and scoring short samples), then the full encode.
const (
	phaseSearch = "finding the quality"
	phaseProbe  = "checking the size"
	phaseEncode = "encoding"
)

// searchShare is the part of a video's work the CRF search takes (0–1): it encodes and
// scores about three times the sampled seconds (two 2-second samples, or the whole of a
// short video), and scoring costs more than encoding, against one encode of the whole
// video. On a 30 s 1080p clip the search took about half the time.
func searchShare(duration float64, vmaf bool) float64 {
	if !vmaf || duration <= 0 {
		return 0
	}
	sampled := 4.0
	if duration < 6 {
		sampled = duration
	}
	work := 3 * sampled * 2.5
	return max(0.1, min(0.9, work/(work+duration)))
}

// fallbackVideoSecsPerMB guesses the work per MB of video before one has been measured
// (SVT-AV1 preset 8 on a 4K iPhone clip with a typical desktop CPU, search included).
const fallbackVideoSecsPerMB = 3.0

// recentPhotos is how many photos the moving average of photo work covers.
const recentPhotos = 50

// CompressionProgress is the live state of a run ("active" false or absent when idle).
type CompressionProgress struct {
	Active bool           `json:"active"`
	Done   int64          `json:"done"`  // items finished in this run
	Total  int64          `json:"total"` // done + still to do (photos and videos being compressed)
	Items  []ProgressItem `json:"items,omitempty"`
	// ETASeconds is a rough estimate of the time left; absent until it can be guessed.
	ETASeconds *int64 `json:"eta_seconds,omitempty"`
}

// ProgressItem is a file being converted now. Percent and phase are for videos only.
type ProgressItem struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // "photo" or "video"
	Percent *float64 `json:"percent,omitempty"`
	Phase   string   `json:"phase,omitempty"`
}

type progressItem struct {
	rel, kind string
	size      int64
	started   time.Time
	phase     string
	frac      float64 // of the whole file, 0–1 (videos)
	inFlight  int     // photos in flight when it started (itself included)
	// When the full encode began, and how far the video was then.
	encodeStart time.Time
	encodeFrom  float64
}

// progressTracker keeps the bookkeeping of the converter's runs. It's safe for
// concurrent use.
type progressTracker struct {
	mu       sync.Mutex
	now      func() time.Time // time.Now; tests replace it
	running  bool
	complete bool // the last run ended with nothing left (or compression turned off)
	started  bool // a run has begun since the server started
	done     int64
	items    map[string]*progressItem
	// Effective seconds per photo (its time divided by how many converted at once), recent.
	photoWork []float64
	// Finished videos: seconds of work and their source bytes.
	videoSecs  float64
	videoBytes int64
}

func (p *progressTracker) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// beginRun marks the converter as working. The count of items done restarts unless the
// previous run was only paused (by indexing).
func (p *progressTracker) beginRun() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.complete {
		p.done = 0
	}
	p.started, p.running, p.complete = true, true, false
}

// endRun marks the converter as idle; complete says nothing was left to do.
func (p *progressTracker) endRun(complete bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running, p.complete = false, complete
}

func (p *progressTracker) photosInFlight() int {
	n := 0
	for _, it := range p.items {
		if it.kind == "photo" {
			n++
		}
	}
	return n
}

// start records that rel (size bytes) is being converted.
func (p *progressTracker) start(rel string, size int64, video bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.items == nil {
		p.items = map[string]*progressItem{}
	}
	it := &progressItem{rel: rel, kind: "photo", size: size, started: p.clock()}
	if video {
		it.kind, it.phase = "video", phaseSearch
	} else {
		it.inFlight = p.photosInFlight() + 1
	}
	p.items[rel] = it
}

// update records a video's phase and how far the whole video has got (0–1).
func (p *progressTracker) update(rel, phase string, frac float64) {
	frac = max(0, min(1, frac))
	p.mu.Lock()
	defer p.mu.Unlock()
	it := p.items[rel]
	if it == nil {
		return
	}
	if phase == phaseEncode && it.encodeStart.IsZero() {
		it.encodeStart, it.encodeFrom = p.clock(), max(it.frac, frac)
	}
	if phase == phaseEncode || it.encodeStart.IsZero() {
		it.phase = phase
	}
	it.frac = max(it.frac, frac) // never goes back
}

// videoLeft estimates the seconds left on a video in progress: from the pace of its
// encode once that has got going (the search's pace says little about it), else by its
// size at secsPerByte.
func (it *progressItem) videoLeft(now time.Time, secsPerByte float64) (left float64, measured bool) {
	if done := it.frac - it.encodeFrom; !it.encodeStart.IsZero() && done >= 0.05 {
		elapsed := now.Sub(it.encodeStart).Seconds()
		return elapsed / done * (1 - it.frac), true
	}
	return max(0, float64(it.size)*secsPerByte-now.Sub(it.started).Seconds()), false
}

// finish records that rel is no longer being converted. counted is false when it was
// interrupted (it will be tried again), so it adds neither to the count nor to timings.
func (p *progressTracker) finish(rel string, counted bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	it := p.items[rel]
	if it == nil {
		return
	}
	delete(p.items, rel)
	if !counted {
		return
	}
	p.done++
	took := p.clock().Sub(it.started).Seconds()
	if it.kind == "video" {
		p.videoSecs += took
		p.videoBytes += it.size
		return
	}
	// How many converted at the same time: the most of when it started and when it ended.
	together := max(it.inFlight, p.photosInFlight()+1)
	p.photoWork = append(p.photoWork, took/float64(together))
	if len(p.photoWork) > recentPhotos {
		p.photoWork = p.photoWork[len(p.photoWork)-recentPhotos:]
	}
}

// pendingWork is what's still to be converted, items in progress included (they're
// recorded only when they finish).
type pendingWork struct {
	Photos, Videos int64
	VideoBytes     int64
}

// snapshot returns the progress, nil when nothing is running.
func (p *progressTracker) snapshot(left pendingWork) *CompressionProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running || (len(p.items) == 0 && left.Photos+left.Videos == 0) {
		return nil
	}
	now := p.clock()
	out := &CompressionProgress{Active: true, Done: p.done, Total: p.done + left.Photos + left.Videos}
	if out.Total < out.Done+int64(len(p.items)) {
		out.Total = out.Done + int64(len(p.items))
	}
	var photosNow, videosNow int64
	var videoBytesNow int64
	keys := make([]string, 0, len(p.items))
	for k := range p.items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // videos first, then the oldest
		a, b := p.items[keys[i]], p.items[keys[j]]
		if a.kind != b.kind {
			return a.kind == "video"
		}
		if !a.started.Equal(b.started) {
			return a.started.Before(b.started)
		}
		return a.rel < b.rel
	})
	for _, k := range keys {
		it := p.items[k]
		item := ProgressItem{Name: path.Base(it.rel), Kind: it.kind}
		if it.kind == "video" {
			pct := float64(int(it.frac*1000)) / 10
			item.Percent, item.Phase = &pct, it.phase
			videosNow++
			videoBytesNow += it.size
		} else {
			photosNow++
		}
		out.Items = append(out.Items, item)
	}

	// Rate of video work: from finished videos, else from the one in progress, else a guess.
	secsPerByte := fallbackVideoSecsPerMB / (1 << 20)
	if p.videoBytes > 0 {
		secsPerByte = p.videoSecs / float64(p.videoBytes)
	} else {
		for _, it := range p.items {
			if left, ok := it.videoLeft(now, 0); it.kind == "video" && ok && it.size > 0 {
				secsPerByte = (now.Sub(it.started).Seconds() + left) / float64(it.size)
			}
		}
	}
	var eta float64
	// Photos: queued ones at the measured pace; those in flight are about half done.
	if photosLeft := max(0, left.Photos-photosNow); photosLeft+photosNow > 0 {
		if len(p.photoWork) == 0 {
			return out // no pace measured yet
		}
		var sum float64
		for _, w := range p.photoWork {
			sum += w
		}
		eta += (float64(photosLeft) + float64(photosNow)/2) * sum / float64(len(p.photoWork))
	}
	// Videos in progress: their elapsed time against how far they've got.
	for _, it := range p.items {
		if it.kind != "video" {
			continue
		}
		left, _ := it.videoLeft(now, secsPerByte)
		eta += left
	}
	// Queued videos: by size, at the measured rate.
	eta += float64(max(0, left.VideoBytes-videoBytesNow)) * secsPerByte
	if q := left.Videos - videosNow; q > 0 && left.VideoBytes == 0 {
		eta += float64(q) * 60 // sizes unknown: a minute each
	}
	secs := int64(eta + 0.5)
	out.ETASeconds = &secs
	return out
}

// parseFFmpegProgress reads ffmpeg's -progress output (key=value lines) and calls fn
// with the output time in seconds as it advances. It returns when r ends.
func parseFFmpegProgress(r io.Reader, fn func(seconds float64)) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		// out_time_ms is in microseconds too (an old ffmpeg quirk).
		if k != "out_time_us" && k != "out_time_ms" {
			if k == "out_time" {
				if s, ok := parseClock(v); ok {
					fn(s)
				}
			}
			continue
		}
		us, err := strconv.ParseInt(v, 10, 64)
		if err != nil || us < 0 {
			continue // "N/A" before the first frame
		}
		fn(float64(us) / 1e6)
	}
}

// parseClock parses ffmpeg's HH:MM:SS.micro time.
func parseClock(v string) (float64, bool) {
	var h, m int
	var s float64
	if n, err := fmt.Sscanf(v, "%d:%d:%f", &h, &m, &s); err != nil || n != 3 || h < 0 {
		return 0, false
	}
	return float64(h*3600+m*60) + s, true
}

// compressionProgress is the destination's live progress (nil when idle).
func (d *Destination) compressionProgress(photos, videos bool) *CompressionProgress {
	d.conv.progress.mu.Lock()
	running := d.conv.progress.running
	d.conv.progress.mu.Unlock()
	if !running {
		return nil
	}
	left, err := d.store.pendingWork(photos, videos)
	if err != nil {
		return nil
	}
	return d.conv.progress.snapshot(left)
}

// pendingWork counts the photos and videos not converted yet, and the videos' bytes.
func (s *Store) pendingWork(photos, videos bool) (pendingWork, error) {
	var w pendingWork
	var total int64
	video := fmt.Sprintf(videoSQL, "rel")
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(`+video+`), 0), COALESCE(SUM(CASE WHEN `+video+` THEN size END), 0) FROM (`+
		s.pendingConversionsQuery("rel, MAX(size) AS size", photos, videos)+` GROUP BY rel)`).Scan(&total, &w.Videos, &w.VideoBytes)
	w.Photos = total - w.Videos
	return w, err
}
