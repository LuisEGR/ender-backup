package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestVideo writes a 2 s 320x240 H.264 video with audio and a creation date.
func makeTestVideo(t *testing.T, path string) {
	t.Helper()
	tools := findVideoTools()
	if tools.reason != "" {
		t.Skip(tools.reason)
	}
	cmd := exec.Command(tools.ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-crf", "8", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", // a big original: room to save
		"-metadata", "creation_time=2024-05-06T07:08:09Z", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test video: %v %s", err, out)
	}
}

func TestConvertToAV1(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "IMG_1.MOV"), filepath.Join(dir, "IMG_1.av1.mp4")
	makeTestVideo(t, src)
	var phases []string
	var encoded float64
	report := func(phase string, frac float64) {
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
		if phase == phaseEncode {
			encoded = frac
		}
	}
	res, err := convertToAV1(context.Background(), src, dst, findVideoLevel(defaultVideoLevel), report)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) == 0 || phases[len(phases)-1] != phaseEncode || encoded < 0.95 {
		t.Fatalf("progress: phases %v, encode reached %.2f", phases, encoded)
	}
	p, err := probeVideo(context.Background(), findVideoTools(), dst)
	if err != nil {
		t.Fatal(err)
	}
	var codecs []string
	for _, s := range p.Streams {
		codecs = append(codecs, s.CodecName)
	}
	if len(codecs) < 2 || codecs[0] != "av1" || codecs[1] != "aac" {
		t.Fatalf("streams %v, want av1 + aac", codecs)
	}
	if d := p.duration(); d < 1.9 || d > 2.1 {
		t.Fatalf("duration %.2f", d)
	}
	out, _ := exec.Command(findVideoTools().ffprobe, "-v", "error", "-show_entries", "format_tags=creation_time",
		"-of", "csv=p=0", dst).Output()
	if string(out) == "" || string(out)[:10] != "2024-05-06" {
		t.Fatalf("creation time not kept: %q", out)
	}
	t.Logf("AV1 copy: %d bytes, CRF %d, VMAF %.1f", res.Size, res.CRF, res.VMAF)
}

// With only "compress videos" on, videos get an AV1 copy and photos are left alone.
func TestVideoSwitchOnly(t *testing.T) {
	e := newTestEnv(t)
	makeTestVideo(t, filepath.Join(e.root, "IMG_2.MOV"))
	writeFile(t, filepath.Join(e.root, "IMG_3.JPG"), string(testJPEG(t, 100)))
	d, release := e.srv.use()
	release()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	e.srv.mu.Lock()
	e.srv.compress = CompressSettings{Format: "avif", PhotoLevel: defaultPhotoLevel, Videos: true, VideoLevel: defaultVideoLevel}
	e.srv.mu.Unlock()
	d.StartConverter(e.srv.compressSettings, &e.srv.events)
	waitFor(t, "video converted", func() bool { s, _ := d.store.conversionStats(false, true); return s.Converted == 1 && s.Remaining == 0 })
	if _, err := os.Stat(filepath.Join(e.root, "IMG_2.av1.mp4")); err != nil {
		t.Fatal("no AV1 copy")
	}
	if _, err := os.Stat(filepath.Join(e.root, "IMG_3.avif")); err == nil {
		t.Fatal("photo converted although photo compression is off")
	}
}

// With PHOTO_BACKUP_TEST_VIDEOS=<folder or file>: what an AV1 copy of each video there couldn't keep.
func TestWhatCopiesCantKeep(t *testing.T) {
	dir := os.Getenv("PHOTO_BACKUP_TEST_VIDEOS")
	if dir == "" {
		t.Skip("set PHOTO_BACKUP_TEST_VIDEOS to run")
	}
	paths := []string{dir} // a folder of videos, or one video
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		paths = nil
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	for _, path := range paths {
		if !isVideo(path) {
			continue
		}
		p, err := probeVideo(context.Background(), findVideoTools(), path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		t.Logf("%s: apple tags %v, cover %d → can't keep: %v", filepath.Base(path), p.appleTags(), p.cover(), p.notCarried(false, false))
	}
}

// With PHOTO_BACKUP_TEST_VIDEOS=<folder or file>: a quick AV1 copy of each video gets the
// original's metadata tracks (same samples) and user data copied over.
func TestCarryTracksReal(t *testing.T) {
	dir := os.Getenv("PHOTO_BACKUP_TEST_VIDEOS")
	if dir == "" {
		t.Skip("set PHOTO_BACKUP_TEST_VIDEOS to run")
	}
	tools := findVideoTools()
	paths := []string{dir}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		paths = nil
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	// Data streams and their packet counts, and the format's tags.
	describe := func(path string) (string, string) {
		out, _ := exec.Command(tools.ffprobe, "-v", "error", "-count_packets", "-show_entries",
			"stream=codec_type,codec_tag_string,nb_read_packets", "-of", "csv=p=0", path).Output()
		var data []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(line, "data") {
				data = append(data, strings.TrimSpace(line))
			}
		}
		tags, _ := exec.Command(tools.ffprobe, "-v", "error", "-show_entries", "format_tags=encoder,com.apple.quicktime.model",
			"-of", "csv=p=0", path).Output()
		return strings.Join(data, " | "), strings.TrimSpace(string(tags))
	}
	for _, src := range paths {
		if !isVideo(src) {
			continue
		}
		in, _ := probeVideo(context.Background(), tools, src)
		dst := filepath.Join(t.TempDir(), "copy.mp4")
		flags := "-faststart"
		if in.appleTags() {
			flags = "+use_metadata_tags"
		}
		cmd := exec.Command(tools.ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-map", "0:v:0", "-map", "0:a?",
			"-map_metadata", "0", "-movflags", flags, "-fps_mode", "passthrough", "-vf", "scale=320:-2",
			"-c:v", "libsvtav1", "-preset", "12", "-crf", "55", "-c:a", "copy", dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: encode: %v %s", src, err, out)
		}
		what, err := carryTracks(src, dst)
		if err != nil {
			t.Fatalf("%s: carry: %v", src, err)
		}
		od, ot := describe(src)
		nd, nt := describe(dst)
		t.Logf("%s: carried %v\n  original data: %s  tags: %s\n  copy data:     %s  tags: %s", filepath.Base(src), what, od, ot, nd, nt)
		if od != nd {
			t.Errorf("%s: data tracks differ", src)
		}
		if b, _ := os.ReadFile(dst); strings.Contains(od, "djmd") && !bytes.Contains(b, []byte("dvtm_Air3.proto")) {
			t.Errorf("%s: DJI telemetry header not in the copy", src)
		}
	}
}

// With PHOTO_BACKUP_TEST_VIDEO=<file>: the full conversion (quality search, Dolby Vision,
// metadata tracks) leaves nothing behind when ffmpeg supports it all.
func TestConvertToAV1Real(t *testing.T) {
	src := os.Getenv("PHOTO_BACKUP_TEST_VIDEO")
	if src == "" {
		t.Skip("set PHOTO_BACKUP_TEST_VIDEO to run")
	}
	dst := filepath.Join(t.TempDir(), "copy.av1.mp4")
	res, err := convertToAV1(context.Background(), src, dst, findVideoLevel(defaultVideoLevel), nil)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := probeVideo(context.Background(), findVideoTools(), src)
	out, _ := probeVideo(context.Background(), findVideoTools(), dst)
	t.Logf("%s: %d%% of the original, CRF %d, VMAF %.1f, not kept: %v; Dolby Vision in %v → out %v",
		filepath.Base(src), 100*res.Size/fileSize(src), res.CRF, res.VMAF, res.Lost, in.hasDolbyVision(), out.hasDolbyVision())
	if findVideoTools().dolbyVision && in.hasDolbyVision() && !out.hasDolbyVision() {
		t.Error("Dolby Vision not kept")
	}
	if len(res.Lost) > 0 && findVideoTools().dolbyVision {
		t.Errorf("not kept: %v", res.Lost)
	}
}

func fileSize(p string) int64 {
	st, _ := os.Stat(p)
	return st.Size()
}

func TestCameraCRF(t *testing.T) {
	probe := func(js string) videoProbe {
		var p videoProbe
		if err := json.Unmarshal([]byte(js), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		js   string
		crf  int
		kind string
	}{
		// iPhone portrait: stored landscape, rotated 90°
		{`{"streams":[{"codec_type":"video","width":3840,"height":2160,"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]}],"format":{"tags":{"com.apple.quicktime.make":"Apple"}}}`, 44, "iPhone portrait"},
		{`{"streams":[{"codec_type":"video","width":3840,"height":2160}],"format":{"tags":{"com.apple.quicktime.make":"Apple"}}}`, 32, "iPhone landscape"},
		{`{"streams":[{"codec_type":"video","width":3840,"height":2160}],"format":{"tags":{"encoder":"DJI Air3"}}}`, 38, "DJI"},
		{`{"streams":[{"codec_type":"video","width":1920,"height":1080}],"format":{"tags":{}}}`, 32, "other camera"},
	}
	for _, c := range cases {
		if crf, kind := cameraCRF(probe(c.js)); crf != c.crf || kind != c.kind {
			t.Errorf("%s: got %d %s, want %d %s", c.js, crf, kind, c.crf, c.kind)
		}
	}
}
