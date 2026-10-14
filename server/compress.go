package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Background compression: converts photos in the destination (uploaded or already there)
// to AVIF next to the original. The index remembers each original's name and size, so a
// photo still counts as backed up after its original is deleted (if the user opts in).

// CompressSettings are the user's choices (config.json / settings page).
type CompressSettings struct {
	Enabled         bool   `json:"compress"`
	Format          string `json:"format"`      // "avif"
	PhotoLevel      string `json:"photo_level"` // how close to the original (levels.go)
	RemoveOriginals bool   `json:"remove_originals"`
	// Videos: a separate switch; AV1 with ffmpeg at the CRF reaching VideoLevel.
	Videos     bool   `json:"compress_videos"`
	VideoLevel string `json:"video_level"`
}

// photos and videos report which kinds are being compressed (videos need ffmpeg).
func (c CompressSettings) kinds() (photos, videos bool) {
	return c.Enabled, c.Videos && findVideoTools().reason == ""
}

func (c CompressSettings) active() bool {
	p, v := c.kinds()
	return p || v
}

// QualitySet is an AVIF quality (10–100) per kind of source photo (levels.go maps each
// perceptual level to one).
type QualitySet struct {
	HEIC  int `json:"heic"`
	JPEG  int `json:"jpeg"`
	PNG   int `json:"png"`
	Other int `json:"other"`
}

// For returns the quality for a file, by its extension.
func (q QualitySet) For(name string) int {
	switch strings.ToLower(path.Ext(name)) {
	case ".heic", ".heif":
		return q.HEIC
	case ".jpg", ".jpeg":
		return q.JPEG
	case ".png":
		return q.PNG
	}
	return q.Other
}

// merged takes the valid values of u over q.
func (q QualitySet) merged(u QualitySet) QualitySet {
	pick := func(old, new int) int {
		if new >= 10 && new <= 100 {
			return new
		}
		return old
	}
	return QualitySet{pick(q.HEIC, u.HEIC), pick(q.JPEG, u.JPEG), pick(q.PNG, u.PNG), pick(q.Other, u.Other)}
}

func (q QualitySet) String() string {
	return fmt.Sprintf("HEIC %d, JPEG %d, PNG %d, other %d", q.HEIC, q.JPEG, q.PNG, q.Other)
}

const conversionsSchema = `
CREATE TABLE IF NOT EXISTS conversions (
	src_rel      TEXT PRIMARY KEY,
	src_name     TEXT NOT NULL COLLATE NOCASE,
	src_size     INTEGER NOT NULL,
	out_rel      TEXT NOT NULL DEFAULT '',  -- '' when conversion failed (see error)
	out_size     INTEGER NOT NULL DEFAULT 0,
	format       TEXT NOT NULL DEFAULT '',
	quality      INTEGER NOT NULL DEFAULT 0,
	converted_at INTEGER NOT NULL,
	src_removed  INTEGER NOT NULL DEFAULT 0,
	error        TEXT NOT NULL DEFAULT '',
	lost         TEXT NOT NULL DEFAULT '' -- what the copy couldn't keep: the original is never deleted
);
CREATE INDEX IF NOT EXISTS conversions_name_size ON conversions (src_name, src_size);
`

// converted returns where a file's converted copy is, if it exists on disk.
func (s *Store) converted(rel string) (string, bool) {
	var out string
	var size int64
	err := s.db.QueryRow(`SELECT out_rel, out_size FROM conversions WHERE src_rel = ? AND out_rel <> ''`, rel).Scan(&out, &size)
	if err != nil || !s.fileOnDisk(out, size) {
		return "", false
	}
	return out, true
}

// present reports whether a stored file is still available: the file itself, or its
// converted copy if the original was converted (and possibly deleted).
func (s *Store) present(rel string, size int64) bool {
	if s.fileOnDisk(rel, size) {
		return true
	}
	var restore int
	if s.db.QueryRow(`SELECT restore FROM conversions WHERE src_rel = ?`, rel).Scan(&restore) == nil && restore == 1 {
		return false // the original was asked back: the copy doesn't count (restore.go)
	}
	_, ok := s.converted(rel)
	return ok
}

// resolve returns the path to act on for a stored file: the original if it's still there,
// otherwise its converted copy.
func (s *Store) resolve(rel string) string {
	if _, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel))); err == nil {
		return rel
	}
	if out, ok := s.converted(rel); ok {
		return out
	}
	return rel
}

type convertCandidate struct {
	rel, name string
	size      int64
}

// convertibleSQL matches paths of photo formats we convert.
const convertibleSQL = `(lower(%[1]s) LIKE '%%.jpg' OR lower(%[1]s) LIKE '%%.jpeg' OR lower(%[1]s) LIKE '%%.png' OR lower(%[1]s) LIKE '%%.heic' OR lower(%[1]s) LIKE '%%.heif')`

// videoSQL matches paths of videos we convert.
const videoSQL = `(lower(%[1]s) LIKE '%%.mov' OR lower(%[1]s) LIKE '%%.mp4' OR lower(%[1]s) LIKE '%%.m4v')`

// pendingConversionsQuery selects photos and/or videos (uploaded or already in the folder)
// that haven't been converted or tried yet. Our own AV1 copies are never sources.
func (s *Store) pendingConversionsQuery(selectCols string, photos, videos bool) string {
	var parts []string
	if photos {
		parts = append(parts, fmt.Sprintf(convertibleSQL, "rel_path"))
	}
	if videos {
		parts = append(parts, "("+fmt.Sprintf(videoSQL, "rel_path")+" AND lower(rel_path) NOT LIKE '%.av1.mp4')")
	}
	cond := "0"
	if len(parts) > 0 {
		cond = "(" + strings.Join(parts, " OR ") + ")"
	}
	return `SELECT ` + selectCols + ` FROM (
		SELECT rel_path AS rel, original_name AS name, size FROM files WHERE ` + cond + `
		UNION
		SELECT rel_path, name, size FROM scanned WHERE ` + cond + `
		UNION
		SELECT rel_path, rel_path, size FROM album_copies WHERE ` + cond + `
	) AS c WHERE NOT EXISTS (SELECT 1 FROM conversions v WHERE v.src_rel = c.rel)`
}

// nextConversions returns up to n items that haven't been converted (or tried) yet: photos
// first (quick), then videos.
func (s *Store) nextConversions(n int, photos, videos bool) ([]convertCandidate, error) {
	rows, err := s.db.Query(s.pendingConversionsQuery("rel, name, size", photos, videos)+
		` ORDER BY `+fmt.Sprintf(videoSQL, "rel")+`, rel LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []convertCandidate
	for rows.Next() {
		var c convertCandidate
		if err := rows.Scan(&c.rel, &c.name, &c.size); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// convDetail is how a copy was made, kept with it so results can be compared later
// without relying on the activity log.
type convDetail struct {
	Level      string   // the quality level's ID (levels.go)
	VMAF       float64  // videos: measured on the samples (0 if not measured)
	Settings   string   // encoder settings, e.g. "libsvtav1 preset 8, 10-bit, CRF found by VMAF search"
	Source     string   // the original, e.g. "2160x3840 · 30 fps · HLG · 27.6 Mb/s · 23.4 s"
	SearchSecs float64  // time spent finding the CRF
	EncodeSecs float64  // time spent making the copy
	Lost       []string // what the copy couldn't keep (its original is then never deleted)
}

func (s *Store) recordConversion(c convertCandidate, outRel string, outSize int64, format string, quality int, convErr error, det convDetail) error {
	msg := ""
	if convErr != nil {
		msg = convErr.Error()
		outRel, outSize = "", 0
	}
	_, err := s.db.Exec(`INSERT INTO conversions (src_rel, src_name, src_size, out_rel, out_size, format, quality, converted_at, error, lost,
			level, vmaf, settings, source, search_secs, encode_secs)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(src_rel) DO UPDATE SET out_rel = excluded.out_rel, out_size = excluded.out_size,
			format = excluded.format, quality = excluded.quality, converted_at = excluded.converted_at,
			error = excluded.error, lost = excluded.lost, level = excluded.level, vmaf = excluded.vmaf,
			settings = excluded.settings, source = excluded.source, search_secs = excluded.search_secs,
			encode_secs = excluded.encode_secs`,
		c.rel, path.Base(c.name), c.size, outRel, outSize, format, quality, time.Now().Unix(), msg, strings.Join(det.Lost, "; "),
		det.Level, det.VMAF, det.Settings, det.Source, det.SearchSecs, det.EncodeSecs)
	return err
}

// ConversionStats summarises progress for the settings page.
type ConversionStats struct {
	Converted  int64  `json:"converted"`
	Remaining  int64  `json:"remaining"`
	Failed     int64  `json:"failed"`
	NotSmaller int64  `json:"not_smaller"` // kept as they were: compressing wouldn't save 10%
	KeptFull   int64  `json:"kept_full"`   // originals kept: the copy couldn't hold all their metadata
	Removed    int64  `json:"removed"`     // originals deleted
	SavedBytes int64  `json:"saved_bytes"` // original size minus converted size
	SrcBytes   int64  `json:"src_bytes"`
	Current    string `json:"current,omitempty"`
	// Progress is the live state of a run in progress (absent when idle).
	Progress *CompressionProgress `json:"progress,omitempty"`
}

func (s *Store) conversionStats(photos, videos bool) (ConversionStats, error) {
	var st ConversionStats
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(out_rel <> ''), 0), COALESCE(SUM(out_rel = '' AND error NOT LIKE 'not smaller%'), 0),
		COALESCE(SUM(out_rel = '' AND error LIKE 'not smaller%'), 0), COALESCE(SUM(src_removed), 0),
		COALESCE(SUM(CASE WHEN out_rel <> '' THEN src_size - out_size END), 0),
		COALESCE(SUM(CASE WHEN out_rel <> '' THEN src_size END), 0),
		COALESCE(SUM(out_rel <> '' AND lost <> ''), 0)
		FROM conversions`).Scan(&st.Converted, &st.Failed, &st.NotSmaller, &st.Removed, &st.SavedBytes, &st.SrcBytes, &st.KeptFull)
	if err != nil {
		return st, err
	}
	err = s.db.QueryRow(s.pendingConversionsQuery("COUNT(DISTINCT rel)", photos, videos)).Scan(&st.Remaining)
	return st, err
}

// removeConvertedOriginals deletes originals whose converted copy is safely on disk.
func (s *Store) removeConvertedOriginals() (int, error) {
	type pending struct {
		src, out string
		outSize  int64
	}
	// Never an original whose copy couldn't keep everything (e.g. drone telemetry).
	rows, err := s.db.Query(`SELECT src_rel, out_rel, out_size FROM conversions WHERE src_removed = 0 AND out_rel <> '' AND lost = ''`)
	if err != nil {
		return 0, err
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.src, &p.out, &p.outSize); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, p)
	}
	rows.Close()

	removed := 0
	for _, p := range list {
		if !s.fileOnDisk(p.out, p.outSize) {
			continue // converted copy missing: keep the original
		}
		if err := os.Remove(filepath.Join(s.root, filepath.FromSlash(p.src))); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := s.db.Exec(`UPDATE conversions SET src_removed = 1 WHERE src_rel = ?`, p.src); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *Store) isFavorite(rel string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM favorites WHERE rel_path = ?`, rel).Scan(&n)
	return n > 0
}

// converter runs conversions for one Destination in the background.
type converter struct {
	mu      sync.Mutex
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	current map[string]bool // photos being converted right now
	claimed map[string]bool // output names taken by conversions in progress
	dupKeys map[string]bool // dupKeys of originals being converted (dedupe.go)
	// progress of the current run (progress.go)
	progress progressTracker
}

// Wake nudges the converter (new uploads, settings changed).
func (d *Destination) WakeConverter() {
	if d.conv.wake == nil {
		return
	}
	select {
	case d.conv.wake <- struct{}{}:
	default:
	}
}

// StartConverter converts photos while settings().Enabled, one at a time, after the
// folder has been indexed.
func (d *Destination) StartConverter(settings func() CompressSettings, events *eventLog) {
	d.conv.wake = make(chan struct{}, 1)
	d.conv.stop = make(chan struct{})
	d.conv.done = make(chan struct{})
	go func() {
		defer close(d.conv.done)
		wait := func(timeout time.Duration) bool {
			select {
			case <-d.conv.stop:
				return false
			case <-d.conv.wake:
			case <-time.After(timeout):
			}
			return true
		}
		d.dedupeCopies(events, d.conv.stop)
		for {
			if running, _ := d.Indexing(); running {
				if !wait(2 * time.Second) {
					return
				}
				continue
			}
			if !settings().active() {
				if !wait(time.Hour) {
					return
				}
				continue
			}
			if !d.runConversions(settings, events, wait) {
				return
			}
			if running, _ := d.Indexing(); running {
				continue // paused for indexing: resume right after it
			}
			// Nothing left (or compression turned off): wait for new photos or settings.
			if settings().active() && !wait(10*time.Minute) {
				return
			}
		}
	}()
}

func (d *Destination) convertOne(ctx context.Context, c convertCandidate, s CompressSettings, events *eventLog) {
	src := filepath.Join(d.root, filepath.FromSlash(c.rel))
	video := isVideo(c.rel)
	format, quality, ext := "avif", findPhotoLevel(s.PhotoLevel).Qualities.For(c.rel), ".avif"
	if video {
		format, quality, ext = "av1", findVideoLevel(s.VideoLevel).FallbackCRF, ".av1.mp4"
	}
	if !d.store.fileOnDisk(c.rel, c.size) {
		d.store.recordConversion(c, "", 0, format, quality, errors.New("original not on disk"), convDetail{})
		return
	}
	// A duplicate of an original already compressed (e.g. "IMG_1 1.HEIC" next to
	// "IMG_1.HEIC") shares that copy instead of getting its own. If its twin is being
	// converted right now, wait for it.
	key := dupKey(c.rel, c.size)
	for {
		d.conv.mu.Lock()
		if d.conv.dupKeys == nil {
			d.conv.dupKeys = map[string]bool{}
		}
		busy := d.conv.dupKeys[key]
		if !busy {
			d.conv.dupKeys[key] = true
		}
		d.conv.mu.Unlock()
		if !busy {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	defer func() {
		d.conv.mu.Lock()
		delete(d.conv.dupKeys, key)
		d.conv.mu.Unlock()
	}()
	if twin, out, outSize, q, ok := d.store.existingCopy(c.rel, c.size); ok {
		d.store.recordConversion(c, out, outSize, format, q, nil, convDetail{Settings: "same copy as " + twin + " (duplicate original)"})
		events.add("%s is a duplicate of %s: it shares that compressed copy", c.rel, twin)
		return
	}
	// IMG_1234.HEIC -> IMG_1234.avif next to it (IMG_1234_1.avif if that name is taken,
	// on disk or by another photo being converted at the same time, e.g. IMG_1234.JPG).
	dir, base := path.Split(c.rel)
	stem := strings.TrimSuffix(base, path.Ext(base))
	d.conv.mu.Lock()
	if d.conv.current == nil {
		d.conv.current, d.conv.claimed = map[string]bool{}, map[string]bool{}
	}
	outRel := dir + stem + ext
	for i := 1; ; i++ {
		_, err := os.Stat(filepath.Join(d.root, filepath.FromSlash(outRel)))
		if errors.Is(err, os.ErrNotExist) && !d.conv.claimed[strings.ToLower(outRel)] {
			break
		}
		outRel = fmt.Sprintf("%s%s_%d%s", dir, stem, i, ext)
	}
	d.conv.claimed[strings.ToLower(outRel)] = true
	d.conv.mu.Unlock()
	defer func() {
		d.conv.mu.Lock()
		delete(d.conv.claimed, strings.ToLower(outRel))
		d.conv.mu.Unlock()
	}()

	start := time.Now()
	var size int64
	var err error
	detail := ""
	var det convDetail
	if video {
		var r av1Result
		report := func(phase string, frac float64) { d.conv.progress.update(c.rel, phase, frac) }
		r, err = convertToAV1(ctx, src, filepath.Join(d.root, filepath.FromSlash(outRel)), findVideoLevel(s.VideoLevel), report)
		size, quality = r.Size, r.CRF
		det = convDetail{Level: s.VideoLevel, VMAF: r.VMAF, Settings: r.Settings, Source: r.Source,
			SearchSecs: r.SearchSecs, EncodeSecs: r.EncodeSecs, Lost: r.Lost}
		if r.VMAF > 0 {
			detail = fmt.Sprintf(", CRF %d, VMAF %.1f", r.CRF, r.VMAF)
		} else {
			detail = fmt.Sprintf(", CRF %d", r.CRF)
		}
		if r.Estimated {
			detail = fmt.Sprintf(" at CRF %d (estimated from a sample)", r.CRF)
		}
	} else {
		size, err = helper.convert(src, filepath.Join(d.root, filepath.FromSlash(outRel)), quality)
		det = convDetail{Level: s.PhotoLevel, Settings: fmt.Sprintf("AVIF quality %d, 4:2:0", quality),
			EncodeSecs: time.Since(start).Seconds()}
	}
	if ctx.Err() != nil {
		return // stopped (settings changed or shutting down): try again later
	}
	if err == nil && float64(size) > float64(c.size)*(1-minSaving) {
		// Already efficient (often an iPhone HEIC or video): keep the original as it is.
		os.Remove(filepath.Join(d.root, filepath.FromSlash(outRel)))
		err = fmt.Errorf("not smaller: the copy would be %.0f%% of the original%s", 100*float64(size)/float64(c.size), detail)
		d.store.recordConversion(c, "", 0, format, quality, err, det)
		events.add("Kept %s as it is (%v)", c.rel, err)
		return
	}
	if err := d.store.recordConversion(c, outRel, size, format, quality, err, det); err != nil {
		events.add("Compression: %v", err)
	}
	if err != nil {
		events.add("Couldn't convert %s: %v", c.rel, err)
		return
	}
	if d.store.isFavorite(c.rel) {
		_ = setFavoriteTag(filepath.Join(d.root, filepath.FromSlash(outRel)), true)
	}
	events.add("Converted %s → %s (%s → %s, %s%s)", c.rel, path.Base(outRel), humanBytes(c.size), humanBytes(size),
		time.Since(start).Round(100*time.Millisecond), detail)
	if len(det.Lost) > 0 {
		events.add("The copy of %s couldn't keep: %s. Its original won't be deleted.", c.rel, strings.Join(det.Lost, ", "))
	}
}

func (d *Destination) stopConverter() {
	if d.conv.stop == nil {
		return
	}
	close(d.conv.stop)
	<-d.conv.done
	d.conv.stop = nil
}

func (d *Destination) converting() string {
	d.conv.mu.Lock()
	defer d.conv.mu.Unlock()
	var names []string
	for rel := range d.conv.current {
		names = append(names, path.Base(rel))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *Server) compressSettings() CompressSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compress
}

// compressUpdate is a change to the compression settings; nil fields stay as they are.
type compressUpdate struct {
	Compress        *bool   `json:"compress"`
	Format          *string `json:"format"`
	PhotoLevel      *string `json:"photo_level"`
	Videos          *bool   `json:"compress_videos"`
	VideoLevel      *string `json:"video_level"`
	RemoveOriginals *bool   `json:"remove_originals"`
}

// applyCompression changes the settings in memory, nudges the converter and returns the
// result. Callers save it to the config file.
func (s *Server) applyCompression(u compressUpdate) CompressSettings {
	s.mu.Lock()
	c := &s.compress
	if u.Compress != nil {
		c.Enabled = *u.Compress
	}
	if u.Format != nil && *u.Format == "avif" { // the only format for now
		c.Format = *u.Format
	}
	if u.PhotoLevel != nil && validPhotoLevel(*u.PhotoLevel) {
		c.PhotoLevel = *u.PhotoLevel
	}
	if u.Videos != nil {
		c.Videos = *u.Videos
	}
	if u.VideoLevel != nil && validVideoLevel(*u.VideoLevel) {
		c.VideoLevel = *u.VideoLevel
	}
	if u.RemoveOriginals != nil {
		c.RemoveOriginals = *u.RemoveOriginals
	}
	result := *c
	s.mu.Unlock()
	d, release := s.use()
	d.WakeConverter()
	release()
	return result
}

// compressionInfo is what the app shows: the settings and how far conversion has got.
func (s *Server) compressionInfo(d *Destination) map[string]any {
	c := s.compressSettings()
	stats, _ := d.store.conversionStats(c.kinds())
	stats.Current = d.converting()
	progress := d.compressionProgress(c.kinds())
	info := map[string]any{
		"enabled": c.Enabled, "format": c.Format, "quality": findPhotoLevel(c.PhotoLevel).Qualities.HEIC,
		"photo_level": findPhotoLevel(c.PhotoLevel).Label, "video_level": findVideoLevel(c.VideoLevel).Label,
		"compress_videos": c.Videos, "remove_originals": c.RemoveOriginals, "not_smaller": stats.NotSmaller,
		"kept_full": stats.KeptFull,
		"formats":   []string{"avif"}, "converted": stats.Converted, "remaining": stats.Remaining,
		"failed": stats.Failed, "removed": stats.Removed, "src_bytes": stats.SrcBytes,
		"out_bytes": stats.SrcBytes - stats.SavedBytes, "saved_bytes": stats.SavedBytes, "current": stats.Current,
	}
	if reason := findVideoTools().reason; reason != "" {
		info["video_reason"] = reason
	}
	if progress != nil {
		info["progress"] = progress
	}
	return info
}

// handleCompression lets a paired phone turn compression and deleting originals on or
// off. Format and quality are chosen on the computer only.
func (s *Server) handleCompression(w http.ResponseWriter, r *http.Request) {
	var u compressUpdate
	if !readJSON(w, r, &u) {
		return
	}
	u.Format, u.PhotoLevel, u.VideoLevel = nil, nil, nil // quality is chosen on the computer
	before := s.compressSettings()
	c := s.applyCompression(u)
	err := s.updateConfig(func(cfg *Config) {
		cfg.Compress, cfg.Format, cfg.PhotoLevel, cfg.RemoveOriginals = c.Enabled, c.Format, c.PhotoLevel, c.RemoveOriginals
		cfg.CompressVideos, cfg.VideoLevel = c.Videos, c.VideoLevel
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "changed for this session, but saving the settings failed: "+err.Error())
		return
	}
	if c != before {
		state := "off"
		if c.Enabled {
			state = fmt.Sprintf("on (%s, %s", strings.ToUpper(c.Format), findPhotoLevel(c.PhotoLevel))
			if c.RemoveOriginals {
				state += ", deleting originals"
			}
			state += ")"
		}
		if c.Videos {
			state += fmt.Sprintf("; videos AV1, %s", findVideoLevel(c.VideoLevel))
		} else {
			state += "; videos off"
		}
		s.events.add("%s set compression %s", deviceLabel(r), state)
	}
	d, release := s.use()
	defer release()
	writeJSON(w, s.compressionInfo(d))
}

func deviceLabel(r *http.Request) string {
	if n := r.Header.Get("X-Device-Name"); n != "" {
		return n
	}
	return "A phone"
}
