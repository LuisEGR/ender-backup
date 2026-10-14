package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Videos are compressed to AV1 (SVT-AV1) with ffmpeg, when it's installed: on an iPhone
// 4K HDR video, CRF 30 kept VMAF ≈ 98 at 70% of the size — but the same CRF gives very
// different quality on other videos, so the CRF is searched per video (levels.go). The AV1 copy keeps the
// HDR (when the source has it), the exact frame timing, the audio (copied), the date and
// place, and is upright (rotation applied).

// videoExts are the video files that get compressed.
var videoExts = map[string]bool{".mov": true, ".mp4": true, ".m4v": true}

func isVideo(name string) bool { return videoExts[strings.ToLower(filepath.Ext(name))] }

type videoTools struct {
	ffmpeg, ffprobe string
	reason          string // why videos can't be compressed ("" when they can)
	dolbyVision     bool   // the AV1 encoder can carry Dolby Vision metadata (ffmpeg 7+)
}

var (
	videoToolsOnce sync.Once
	videoToolsInfo videoTools
)

// findVideoTools looks for ffmpeg/ffprobe with the SVT-AV1 encoder (cached).
func findVideoTools() videoTools {
	videoToolsOnce.Do(func() {
		find := func(name string) string {
			if p, err := exec.LookPath(name); err == nil {
				return p
			}
			var dirs []string
			switch runtime.GOOS {
			case "darwin":
				dirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}
			case "linux":
				dirs = []string{"/usr/bin", "/usr/local/bin", "/snap/bin"}
			case "windows":
				dirs = []string{filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\WinGet\Links`), `C:\ProgramData\chocolatey\bin`}
				name += ".exe"
			}
			for _, d := range dirs {
				if _, err := os.Stat(filepath.Join(d, name)); err == nil {
					return filepath.Join(d, name)
				}
			}
			return ""
		}
		t := videoTools{ffmpeg: find("ffmpeg"), ffprobe: find("ffprobe")}
		if t.ffmpeg == "" || t.ffprobe == "" {
			t.reason = "Install ffmpeg on this computer to compress videos (winget install ffmpeg, brew install ffmpeg, or apt install ffmpeg)."
		} else {
			cmd := exec.Command(t.ffmpeg, "-hide_banner", "-encoders")
			hideWindow(cmd)
			out, _ := cmd.Output()
			if !strings.Contains(string(out), "libsvtav1") {
				t.reason = "This computer's ffmpeg has no SVT-AV1 encoder; install a full build of ffmpeg to compress videos."
			} else {
				help := exec.Command(t.ffmpeg, "-hide_banner", "-h", "encoder=libsvtav1")
				hideWindow(help)
				h, _ := help.Output()
				t.dolbyVision = strings.Contains(string(h), "dolbyvision")
			}
		}
		videoToolsInfo = t
	})
	return videoToolsInfo
}

type videoProbe struct {
	Streams []struct {
		Index          int    `json:"index"`
		CodecType      string `json:"codec_type"`
		CodecName      string `json:"codec_name"`
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		PixFmt         string `json:"pix_fmt"`
		ColorPrimaries string `json:"color_primaries"`
		ColorTransfer  string `json:"color_transfer"`
		ColorSpace     string `json:"color_space"`
		AvgFrameRate   string `json:"avg_frame_rate"`
		BitRate        string `json:"bit_rate"`
		Disposition    struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		Tags struct {
			HandlerName string `json:"handler_name"`
		} `json:"tags"`
		SideData []struct {
			Type     string  `json:"side_data_type"`
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}

// mainVideo is the video stream (not a cover image), or -1.
func (p videoProbe) mainVideo() int {
	for i, s := range p.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			return i
		}
	}
	return -1
}

// portrait: taller than wide as shown (iPhones store portrait video landscape, rotated 90°).
func (p videoProbe) portrait() bool {
	i := p.mainVideo()
	if i < 0 {
		return false
	}
	s := p.Streams[i]
	w, h := s.Width, s.Height
	for _, sd := range s.SideData {
		if r := int(math.Abs(sd.Rotation)) % 180; r == 90 {
			w, h = h, w
		}
	}
	return h > w
}

func (p videoProbe) fromIPhone() bool {
	return strings.EqualFold(p.Format.Tags["com.apple.quicktime.make"], "Apple")
}

// fromDJI: DJI drones and cameras name themselves in the encoder tag ("DJI Air3") or
// carry their telemetry track.
func (p videoProbe) fromDJI() bool {
	if strings.Contains(strings.ToUpper(p.Format.Tags["encoder"]), "DJI") {
		return true
	}
	for _, s := range p.Streams {
		if strings.Contains(s.Tags.HandlerName, "DJI") {
			return true
		}
	}
	return false
}

// describe sums up the video stream for the record: "2160x3840 · 30 fps · HLG · 27.6 Mb/s · 23.4 s".
func (p videoProbe) describe() string {
	for _, s := range p.Streams {
		if s.CodecType != "video" || s.Disposition.AttachedPic == 1 {
			continue
		}
		parts := []string{fmt.Sprintf("%dx%d", s.Width, s.Height)}
		if a, b, ok := strings.Cut(s.AvgFrameRate, "/"); ok {
			n, _ := strconv.ParseFloat(a, 64)
			d, _ := strconv.ParseFloat(b, 64)
			if d > 0 {
				parts = append(parts, fmt.Sprintf("%.3g fps", n/d))
			}
		}
		switch s.ColorTransfer {
		case "arib-std-b67":
			parts = append(parts, "HLG")
		case "smpte2084":
			parts = append(parts, "PQ")
		}
		if p.hasDolbyVision() {
			parts = append(parts, "Dolby Vision")
		}
		if br, _ := strconv.ParseFloat(s.BitRate, 64); br > 0 {
			parts = append(parts, fmt.Sprintf("%.1f Mb/s", br/1e6))
		}
		parts = append(parts, s.CodecName, fmt.Sprintf("%.1f s", p.duration()))
		return strings.Join(parts, " · ")
	}
	return ""
}

// appleTags reports QuickTime metadata keys (location, make, model…) on the source.
func (p videoProbe) appleTags() bool {
	for k := range p.Format.Tags {
		if strings.HasPrefix(k, "com.apple.quicktime.") {
			return true
		}
	}
	return false
}

// cover returns the index of an embedded thumbnail (cover image) stream, or -1.
func (p videoProbe) cover() int {
	for _, s := range p.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 1 && (s.CodecName == "mjpeg" || s.CodecName == "png") {
			return s.Index
		}
	}
	return -1
}

func (p videoProbe) hasDolbyVision() bool {
	for _, s := range p.Streams {
		for _, sd := range s.SideData {
			if strings.Contains(sd.Type, "DOVI") {
				return true
			}
		}
	}
	return false
}

// notCarried lists what an AV1 MP4 copy lacks, in plain words. With carried (the
// metadata tracks and user data were copied over, see carrytracks.go) at most Dolby
// Vision is missing (when ffmpeg can't encode it); otherwise also the tracks, cover and
// camera name.
func (p videoProbe) notCarried(carried, keptDolbyVision bool) []string {
	var lost []string
	tracks := map[string]int{}
	for _, s := range p.Streams {
		switch {
		case carried:
		case s.CodecType == "data":
			name := s.Tags.HandlerName
			switch {
			case strings.Contains(name, "DJI meta"):
				name = "DJI flight telemetry"
			case strings.Contains(name, "DJI"):
				name = "DJI debug data"
			case strings.Contains(name, "Core Media Metadata"):
				name = "Apple timed metadata"
			case name == "":
				name = "a data track"
			}
			tracks[name]++
		case s.CodecType == "video" && s.Disposition.AttachedPic == 1:
			lost = append(lost, "thumbnail")
		}
		for _, sd := range s.SideData {
			if strings.Contains(sd.Type, "DOVI") && !keptDolbyVision {
				lost = append(lost, "Dolby Vision layer (the HDR itself is kept)")
			}
		}
	}
	for name, n := range tracks {
		if n > 1 {
			name = fmt.Sprintf("%s (%d tracks)", name, n)
		}
		lost = append(lost, name)
	}
	if enc := p.Format.Tags["encoder"]; enc != "" && !strings.HasPrefix(enc, "Lav") && !carried {
		lost = append(lost, "camera name ("+enc+")")
	}
	sort.Strings(lost)
	return lost
}

func probeVideo(ctx context.Context, t videoTools, path string) (videoProbe, error) {
	var p videoProbe
	cmd := exec.CommandContext(ctx, t.ffprobe, "-v", "error", "-show_entries",
		"stream=index,codec_type,codec_name,width,height,pix_fmt,color_primaries,color_transfer,color_space,avg_frame_rate,bit_rate:stream_tags=handler_name:stream_disposition=attached_pic:stream_side_data=side_data_type,rotation:format=duration:format_tags",
		"-of", "json", path)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return p, fmt.Errorf("ffprobe: %w", err)
	}
	return p, json.Unmarshal(out, &p)
}

func (p videoProbe) duration() float64 {
	d, _ := strconv.ParseFloat(p.Format.Duration, 64)
	return d
}

// av1Result describes a finished video conversion.
type av1Result struct {
	Size int64
	CRF  int
	VMAF float64 // measured on the samples (0 when ffmpeg has no VMAF)
	// Estimated: no copy was made; Size is the size the samples predict (larger than the original).
	Estimated              bool
	Settings               string // encoder settings, for the record
	Source                 string // the original's resolution, frame rate, HDR, bitrate, length
	SearchSecs, EncodeSecs float64
	// What the copy couldn't keep from the original (its original is then never deleted).
	Lost []string
}

// convertToAV1 encodes src to an AV1 MP4 at dst, at the CRF that reaches the level's VMAF
// for this video (searched on short samples). report (may be nil) hears the phase and how
// far it has got (0–1).
func convertToAV1(ctx context.Context, src, dst string, level videoLevel, report func(phase string, frac float64)) (av1Result, error) {
	// report hears how far the whole video has got; share is the search's part of it.
	if report == nil {
		report = func(string, float64) {}
	}
	t := findVideoTools()
	if t.reason != "" {
		return av1Result{}, errors.New(t.reason)
	}
	in, err := probeVideo(ctx, t, src)
	if err != nil {
		return av1Result{}, err
	}
	enc := videoEncodeArgs(in, t.dolbyVision)
	res := av1Result{CRF: level.FallbackCRF, Source: in.describe(), Settings: "libsvtav1 " + strings.Join(enc[2:], " ")}
	searchStart := time.Now()
	share := searchShare(in.duration(), hasVMAF(t))
	if level.ByCamera {
		var kind string
		res.CRF, kind = cameraCRF(in)
		res.Settings += fmt.Sprintf(", CRF %d for %s", res.CRF, kind)
		share = min(0.3, 3/max(in.duration(), 1)) // one short sample, against the whole encode
		report(phaseProbe, 0)
		// One 2-second sample tells whether the copy can be any smaller; when not (often
		// with iPhone HEVC), the full encode is skipped.
		if rate, err := sampleRate(ctx, t, src, in, enc, res.CRF); err == nil && rate > 0 {
			if st, err := os.Stat(src); err == nil {
				if est := rate * in.duration(); est > float64(st.Size()) {
					res.Size, res.Estimated = int64(est), true
					res.SearchSecs = time.Since(searchStart).Seconds()
					return res, nil
				}
			}
		}
	} else if hasVMAF(t) {
		report(phaseSearch, 0)
		res.Settings += ", CRF by VMAF search"
		rates := &sampleRates{}
		if res.CRF, res.VMAF, err = searchCRF(ctx, t, src, in, enc, level.VMAF, func(f float64) { report(phaseSearch, share*f) }, rates); err != nil {
			return av1Result{}, err
		}
		// The samples already show the copy's size: when it clearly can't be smaller (about a
		// quarter of iPhone videos), skip the full encode — the slowest part — and report the
		// estimate, which the caller turns into "kept as it is".
		if st, err := os.Stat(src); err == nil && in.duration() > 0 {
			if est := rates.rate(res.CRF) * in.duration(); est > float64(st.Size()) {
				res.Size, res.Estimated = int64(est), true
				res.SearchSecs = time.Since(searchStart).Seconds()
				return res, nil
			}
		}
	}
	res.SearchSecs = time.Since(searchStart).Seconds()
	encodeStart := time.Now()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src, "-map", "0:v:0", "-map", "0:a?"}
	// Apple's metadata keys (location, make, model, local date) need use_metadata_tags.
	// The moov stays at the end (no faststart) so carryTracks can add to it.
	movflags := "-faststart"
	if in.appleTags() {
		movflags = "+use_metadata_tags"
	}
	args = append(args, "-map_metadata", "0", "-movflags", movflags,
		"-fps_mode", "passthrough", // keep the exact frame timing (iPhones vary it slightly)
		"-crf", strconv.Itoa(res.CRF), "-c:a", "copy")
	args = append(args, enc...)
	tmp := dst + ".tmp.mp4"
	args = append(args, tmp)
	report(phaseEncode, share)
	total := in.duration()
	onTime := func(sec float64) {
		if total > 0 {
			report(phaseEncode, share+(1-share)*min(1, sec/total))
		}
	}
	if err := runFFmpeg(ctx, t.ffmpeg, args, onTime); err != nil {
		os.Remove(tmp)
		return av1Result{}, err
	}
	res.EncodeSecs = time.Since(encodeStart).Seconds()
	// What ffmpeg can't write (metadata tracks, the original's user data) is copied over.
	carried, cerr := carryTracks(src, tmp)
	res.Lost = in.notCarried(cerr == nil, t.dolbyVision)
	if cerr != nil {
		res.Lost = append(res.Lost, "(copying metadata tracks failed: "+cerr.Error()+")")
	}
	// Some videos can't reach the level even at the lowest CRF the search allows (e.g. a
	// 2-second 4K HDR clip scored VMAF 89 at CRF 18). The copy is still kept, but it
	// doesn't replace the original.
	if res.VMAF > 0 && res.VMAF < level.VMAF-0.5 {
		res.Lost = append(res.Lost, fmt.Sprintf("full quality (VMAF %.1f, the level asks for %.0f)", res.VMAF, level.VMAF))
	}
	_ = carried
	// The copy must be the whole video.
	out, err := probeVideo(ctx, t, tmp)
	if err != nil || math.Abs(out.duration()-in.duration()) > 0.5 {
		os.Remove(tmp)
		return av1Result{}, fmt.Errorf("AV1 copy is %.1f s long, original %.1f s", out.duration(), in.duration())
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chtimes(tmp, st.ModTime(), st.ModTime())
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return av1Result{}, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return av1Result{}, err
	}
	res.Size = st.Size()
	return res, nil
}

// videoEncodeArgs are the AV1 settings for a source: 10-bit, the source's colour
// description (which keeps HDR), and its Dolby Vision metadata when the encoder can carry
// it (iPhone profile 8.4 becomes AV1 profile 10.4, HLG-compatible).
func videoEncodeArgs(in videoProbe, dolbyVision bool) []string {
	args := []string{"-c:v:0", "libsvtav1", "-preset", "8", "-pix_fmt:v:0", "yuv420p10le"}
	for _, s := range in.Streams {
		if s.CodecType != "video" {
			continue
		}
		if s.ColorPrimaries != "" && s.ColorPrimaries != "unknown" {
			args = append(args, "-color_primaries", s.ColorPrimaries)
		}
		if s.ColorTransfer != "" && s.ColorTransfer != "unknown" {
			args = append(args, "-color_trc", s.ColorTransfer)
		}
		if s.ColorSpace != "" && s.ColorSpace != "unknown" {
			args = append(args, "-colorspace", s.ColorSpace)
		}
		if dolbyVision && in.hasDolbyVision() {
			args = append(args, "-dolbyvision", "1", "-strict", "unofficial") // "unofficial": the MP4 box saying so
		}
		break
	}
	return args
}

var (
	vmafOnce sync.Once
	vmafOK   bool
)

// hasVMAF reports whether ffmpeg can measure VMAF (libvmaf).
func hasVMAF(t videoTools) bool {
	vmafOnce.Do(func() {
		cmd := exec.Command(t.ffmpeg, "-hide_banner", "-filters")
		hideWindow(cmd)
		out, _ := cmd.Output()
		vmafOK = strings.Contains(string(out), "libvmaf")
	})
	return vmafOK
}

// searchCRF finds the CRF whose VMAF (mean over two 2-second samples, at full
// resolution like the levels were calibrated — scoring 4K at 1080p hid detail loss and
// picked far too high a CRF) is closest to the target: two probes, a linear estimate,
// and one check of the estimate.
// report hears how far the search has got (0–1, on the usual three measurements).
// sampleRates records the samples' encoded size per CRF.
type sampleRates struct {
	mu    sync.Mutex
	bytes map[int]int64
	secs  float64
}

func (r *sampleRates) add(crf int, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bytes == nil {
		r.bytes = map[int]int64{}
	}
	r.bytes[crf] += n
}

// rate is the bytes per second at crf, from the nearest measured CRF.
func (r *sampleRates) rate(crf int) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	best, bestDist := -1, 1<<30
	for c := range r.bytes {
		if d := abs(c - crf); d < bestDist {
			best, bestDist = c, d
		}
	}
	if best < 0 || r.secs <= 0 {
		return 0
	}
	return float64(r.bytes[best]) / r.secs
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sampleRate encodes 2 seconds from the middle of the video at crf and returns the copy's
// bytes per second of video (0 for clips too short to sample).
func sampleRate(ctx context.Context, t videoTools, src string, in videoProbe, enc []string, crf int) (float64, error) {
	d := in.duration()
	if d < 4 {
		return 0, nil
	}
	work, err := os.MkdirTemp("", "photo-backup-sample-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	sample := filepath.Join(work, "s.mkv")
	args := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-ss", fmt.Sprintf("%.3f", d/2-1), "-t", "2", "-i", src,
		"-map", "0:v:0", "-an", "-fps_mode", "passthrough", "-crf", strconv.Itoa(crf)}, enc...)
	if err := runLowPriority(ctx, t.ffmpeg, append(args, sample)); err != nil {
		return 0, err
	}
	st, err := os.Stat(sample)
	if err != nil {
		return 0, err
	}
	return float64(st.Size()) / 2, nil
}

// rates collects the samples' encoded sizes, which tell early whether a copy can be any
// smaller than the original.
func searchCRF(ctx context.Context, t videoTools, src string, in videoProbe, enc []string, target float64, report func(float64), rates *sampleRates) (int, float64, error) {
	d := in.duration()
	type seg struct{ start, length float64 }
	segs := []seg{{0, d}}
	if d >= 6 {
		segs = []seg{{d * 0.25, 2}, {d * 0.65, 2}}
	}
	for _, sg := range segs {
		rates.secs += sg.length
	}
	work, err := os.MkdirTemp("", "photo-backup-crf-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(work)
	// Each sample is encoded and then scored; three measurements are usual (up to four).
	steps, step := float64(3*2*len(segs)), 0.0
	var stepMu sync.Mutex
	advance := func() {
		stepMu.Lock()
		step++
		report(min(0.95, step/steps))
		stepMu.Unlock()
	}
	// score encodes one sample at a CRF and returns its VMAF against the original.
	score := func(crf int, i int) (float64, error) {
		sg := segs[i]
		sample := filepath.Join(work, fmt.Sprintf("s%d_%d.mkv", i, crf))
		ss, tt := fmt.Sprintf("%.3f", sg.start), fmt.Sprintf("%.3f", sg.length)
		args := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-ss", ss, "-t", tt, "-i", src,
			"-map", "0:v:0", "-an", "-fps_mode", "passthrough", "-crf", strconv.Itoa(crf)}, enc...)
		if err := runLowPriority(ctx, t.ffmpeg, append(args, sample)); err != nil {
			return 0, err
		}
		if st, err := os.Stat(sample); err == nil {
			rates.add(crf, st.Size())
		}
		advance()
		// Frames are paired by index (timestamps of the copy and the original can round apart).
		// Every other frame is scored: half the work, and 2 seconds still give 30–60 frames.
		graph := fmt.Sprintf("[0:v]setpts=N/TB[a];[1:v]setpts=N/TB[b];[a][b]libvmaf=n_threads=%d:n_subsample=2", runtime.NumCPU())
		cmd := exec.CommandContext(ctx, t.ffmpeg, "-hide_banner", "-i", sample, "-ss", ss, "-t", tt, "-i", src,
			"-lavfi", graph, "-f", "null", "-")
		lowPriorityChild(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("vmaf: %v", err)
		}
		k := strings.LastIndex(string(out), "VMAF score: ")
		if k < 0 {
			return 0, errors.New("vmaf: no score")
		}
		v, err := strconv.ParseFloat(strings.Fields(string(out)[k+len("VMAF score: "):])[0], 64)
		if err != nil {
			return 0, err
		}
		advance()
		return v, nil
	}
	// measureAll scores every sample at each CRF at once: short encodes spend much of their
	// time starting up and VMAF has single-threaded stretches, so one at a time left
	// about a third of the CPU idle during the search.
	measureAll := func(crfs ...int) ([]float64, error) {
		sums := make([]float64, len(crfs))
		errs := make([]error, len(crfs)*len(segs))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for ci, crf := range crfs {
			for i := range segs {
				wg.Add(1)
				go func(ci, crf, i int) {
					defer wg.Done()
					v, err := score(crf, i)
					mu.Lock()
					sums[ci] += v
					errs[ci*len(segs)+i] = err
					mu.Unlock()
				}(ci, crf, i)
			}
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		for ci := range sums {
			sums[ci] /= float64(len(segs))
		}
		return sums, nil
	}
	measure := func(crf int) (float64, error) {
		v, err := measureAll(crf)
		if err != nil {
			return 0, err
		}
		return v[0], nil
	}
	clamp := func(c float64) int { return int(math.Round(math.Max(18, math.Min(55, c)))) }
	estimate := func(c1 int, v1 float64, c2 int, v2 float64) int {
		if math.Abs(v1-v2) < 0.05 { // flat: the video is easy, use the cheaper end
			if v1 >= target {
				return clamp(float64(max(c1, c2)) + 6)
			}
			return clamp(float64(min(c1, c2)) - 6)
		}
		return clamp(float64(c1) + (v1-target)*float64(c2-c1)/(v1-v2))
	}
	lo, hi := 28, 40
	probes, err := measureAll(lo, hi)
	if err != nil {
		return 0, 0, err
	}
	vlo, vhi := probes[0], probes[1]
	crf := estimate(lo, vlo, hi, vhi)
	if crf == lo {
		return lo, vlo, nil
	}
	if crf == hi {
		return hi, vhi, nil
	}
	v, err := measure(crf)
	if err != nil {
		return 0, 0, err
	}
	// Refine once from the closer probe (VMAF isn't linear in CRF), never below the target.
	if v < target {
		near, vnear := lo, vlo
		if crf < lo {
			near, vnear = crf-4, 0
		}
		if vnear > 0 {
			if c := estimate(near, vnear, crf, v); c < crf {
				if v2, err := measure(c); err == nil {
					return c, v2, nil
				}
			}
		}
		return clamp(float64(crf) - 2), v, nil
	}
	return crf, v, nil
}

// runLowPriority runs ffmpeg below normal priority, so the computer stays responsive and
// the server keeps answering the phone.
func runLowPriority(ctx context.Context, ffmpeg string, args []string) error {
	return runFFmpeg(ctx, ffmpeg, args, nil)
}

// runFFmpeg is runLowPriority that, with onTime, also reports the output time (seconds)
// as the encode advances (ffmpeg's -progress on its stdout).
func runFFmpeg(ctx context.Context, ffmpeg string, args []string, onTime func(seconds float64)) error {
	if onTime != nil {
		args = append([]string{"-progress", "pipe:1", "-nostats"}, args...)
	}
	var cmd *exec.Cmd
	if runtime.GOOS != "windows" {
		if nice, err := exec.LookPath("nice"); err == nil {
			cmd = exec.CommandContext(ctx, nice, append([]string{"-n", "10", ffmpeg}, args...)...)
		}
	}
	if cmd == nil {
		cmd = exec.CommandContext(ctx, ffmpeg, args...)
	}
	lowPriorityChild(cmd)
	var out []byte
	var err error
	if onTime == nil {
		out, err = cmd.CombinedOutput()
	} else {
		var stderr tailBuffer
		cmd.Stderr = &stderr
		var stdout io.ReadCloser
		if stdout, err = cmd.StdoutPipe(); err == nil {
			if err = cmd.Start(); err == nil {
				parseFFmpegProgress(stdout, onTime)
				io.Copy(io.Discard, stdout)
				err = cmd.Wait()
			}
		}
		out = stderr.Bytes()
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, msg)
	}
	return nil
}

// tailBuffer keeps the last 4 KB written to it (ffmpeg's error messages).
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = append([]byte(nil), t.b[len(t.b)-4096:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return t.b }
