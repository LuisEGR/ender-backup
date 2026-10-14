package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFFmpegProgress(t *testing.T) {
	in := strings.Join([]string{
		"frame=0", "out_time_us=N/A", "out_time=N/A", "progress=continue",
		"frame=30", "out_time_us=1000000", "out_time_ms=1000000", "out_time=00:00:01.000000", "progress=continue",
		"frame=90", "out_time_us=3500000", "progress=continue",
		"out_time=01:02:03.500000",
		"garbage", "out_time_us=-5", "progress=end",
	}, "\r\n")
	var got []float64
	parseFFmpegProgress(strings.NewReader(in), func(s float64) { got = append(got, s) })
	want := []float64{1, 1, 1, 3.5, 3723.5}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestProgressBookkeeping(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := &progressTracker{now: clk.now}
	if p.snapshot(pendingWork{Photos: 5}) != nil {
		t.Fatal("progress before a run")
	}
	p.beginRun()
	// Nothing measured yet: counts but no estimate.
	p.start("a/1.heic", 3<<20, false)
	p.start("a/2.heic", 3<<20, false)
	s := p.snapshot(pendingWork{Photos: 10})
	if s == nil || !s.Active || s.Done != 0 || s.Total != 10 || len(s.Items) != 2 || s.ETASeconds != nil {
		t.Fatalf("start: %+v", s)
	}
	if s.Items[0].Name != "1.heic" || s.Items[0].Kind != "photo" || s.Items[0].Percent != nil {
		t.Fatalf("photo item %+v", s.Items[0])
	}
	// Two photos converting together, 4 s each: 2 s of work per photo.
	clk.advance(4 * time.Second)
	p.finish("a/1.heic", true)
	p.finish("a/2.heic", true)
	s = p.snapshot(pendingWork{Photos: 8})
	if s.Done != 2 || s.Total != 10 || s.ETASeconds == nil || *s.ETASeconds != 16 {
		t.Fatalf("after two photos: %+v eta %v", s, s.ETASeconds)
	}
	// An interrupted one counts for nothing.
	p.start("a/3.heic", 1, false)
	clk.advance(time.Hour)
	p.finish("a/3.heic", false)
	if s = p.snapshot(pendingWork{Photos: 8}); s.Done != 2 || *s.ETASeconds != 16 {
		t.Fatalf("interrupted: %+v", s)
	}

	// A paused run (indexing) keeps its count; a complete one starts over.
	p.endRun(false)
	if p.snapshot(pendingWork{Photos: 8}) != nil {
		t.Fatal("progress while idle")
	}
	p.beginRun()
	if s = p.snapshot(pendingWork{Photos: 8}); s.Done != 2 {
		t.Fatalf("resumed run: %+v", s)
	}

	// A video: the search, then the encode; its progress never goes back.
	p.start("v/clip.mov", 100<<20, true)
	s = p.snapshot(pendingWork{Videos: 2, VideoBytes: 300 << 20})
	if it := s.Items[0]; it.Kind != "video" || it.Phase != phaseSearch || it.Percent == nil || *it.Percent != 0 {
		t.Fatalf("video item %+v", it)
	}
	// Before any measurement: the fallback rate for the whole of both videos.
	if want := int64(300 * fallbackVideoSecsPerMB); *s.ETASeconds != want {
		t.Fatalf("video eta %d, want %d", *s.ETASeconds, want)
	}
	// The search: its pace says nothing about the encode, so the size still decides.
	p.update("v/clip.mov", phaseSearch, 0.1)
	clk.advance(20 * time.Second)
	s = p.snapshot(pendingWork{Videos: 2, VideoBytes: 300 << 20})
	if *s.Items[0].Percent != 10 || *s.ETASeconds != int64(300*fallbackVideoSecsPerMB)-20 {
		t.Fatalf("searching: %+v eta %d", s.Items[0], *s.ETASeconds)
	}
	// The encode starts at 20% after 20 s and reaches 60% 40 s later.
	p.update("v/clip.mov", phaseEncode, 0.2)
	clk.advance(40 * time.Second)
	p.update("v/clip.mov", phaseEncode, 0.6)
	p.update("v/clip.mov", phaseSearch, 0.3) // a late report from the search: ignored
	s = p.snapshot(pendingWork{Videos: 2, VideoBytes: 300 << 20})
	it := s.Items[0]
	if *it.Percent != 60 || it.Phase != phaseEncode {
		t.Fatalf("video at %+v", it)
	}
	// 40% in 40 s: 40 s left, 100 s in all for 100 MB (1 s/MB), so 200 s for the next one.
	if *s.ETASeconds != 240 {
		t.Fatalf("eta %d, want 240", *s.ETASeconds)
	}
	clk.advance(40 * time.Second)
	p.finish("v/clip.mov", true)
	s = p.snapshot(pendingWork{Videos: 1, VideoBytes: 50 << 20})
	if s.Done != 3 || s.Total != 4 || *s.ETASeconds != 50 {
		t.Fatalf("after video: %+v eta %d", s, *s.ETASeconds)
	}
	p.endRun(true)
	p.beginRun()
	if s = p.snapshot(pendingWork{Photos: 1}); s.Done != 0 || s.Total != 1 {
		t.Fatalf("new run: %+v", s)
	}
	// Nothing left and nothing in progress: no progress to show.
	if p.snapshot(pendingWork{}) != nil {
		t.Fatal("progress with nothing to do")
	}
}

func TestPendingWork(t *testing.T) {
	root := t.TempDir()
	st, err := OpenStore(filepath.Join(root, "index.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for name, size := range map[string]int{"a.heic": 10, "b.jpg": 20, "c.mov": 1000, "d.mp4": 500, "e.av1.mp4": 300, "f.txt": 5} {
		if err := os.WriteFile(filepath.Join(root, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ScanFolder(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	w, err := st.pendingWork(true, true)
	if err != nil {
		t.Fatal(err)
	}
	if w != (pendingWork{Photos: 2, Videos: 2, VideoBytes: 1500}) {
		t.Fatalf("pending %+v", w)
	}
	st.recordConversion(convertCandidate{rel: "c.mov", name: "c.mov", size: 1000}, "", 0, "av1", 30, os.ErrInvalid, convDetail{})
	if w, _ = st.pendingWork(false, true); w != (pendingWork{Videos: 1, VideoBytes: 500}) {
		t.Fatalf("pending videos %+v", w)
	}
	if w, _ = st.pendingWork(true, false); w != (pendingWork{Photos: 2}) {
		t.Fatalf("pending photos %+v", w)
	}
}

func TestSearchShare(t *testing.T) {
	for _, c := range []struct {
		duration float64
		vmaf     bool
		lo, hi   float64
	}{
		{30, true, 0.45, 0.6}, {600, true, 0.1, 0.1}, {3, true, 0.85, 0.9}, {30, false, 0, 0}, {0, true, 0, 0},
	} {
		if s := searchShare(c.duration, c.vmaf); s < c.lo || s > c.hi {
			t.Errorf("searchShare(%v, %v) = %.2f, want %.2f–%.2f", c.duration, c.vmaf, s, c.lo, c.hi)
		}
	}
}

// ffmpeg reports how far an encode has got.
func TestRunFFmpegProgress(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mov")
	makeTestVideo(t, src)
	var last float64
	n := 0
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src, "-c:v", "libx264", "-an", filepath.Join(dir, "out.mp4")}
	if err := runFFmpeg(context.Background(), findVideoTools().ffmpeg, args, func(s float64) { last = s; n++ }); err != nil {
		t.Fatal(err)
	}
	if n == 0 || last < 1.9 || last > 2.1 {
		t.Fatalf("%d reports, last %.2f s", n, last)
	}
	// Errors still carry ffmpeg's message.
	err := runFFmpeg(context.Background(), findVideoTools().ffmpeg, []string{"-hide_banner", "-i", filepath.Join(dir, "missing.mov"), filepath.Join(dir, "x.mp4")}, func(float64) {})
	if err == nil || !strings.Contains(err.Error(), "missing.mov") {
		t.Fatalf("error %v", err)
	}
}
