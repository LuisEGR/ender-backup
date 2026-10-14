package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
)

// testTIFF builds a big-endian EXIF block with an Orientation tag and, in the Exif
// sub-IFD, DateTimeOriginal.
func testTIFF(orientation uint16, date string) []byte {
	be := binary.BigEndian
	var b bytes.Buffer
	b.WriteString("MM")
	binary.Write(&b, be, uint16(42))
	binary.Write(&b, be, uint32(8)) // IFD0 at 8
	// IFD0: 2 entries
	binary.Write(&b, be, uint16(2))
	entry := func(tag, typ uint16, count, value uint32) {
		binary.Write(&b, be, tag)
		binary.Write(&b, be, typ)
		binary.Write(&b, be, count)
		binary.Write(&b, be, value)
	}
	entry(0x0112, 3, 1, uint32(orientation)<<16) // SHORT, left-justified
	subIFD := uint32(8 + 2 + 2*12 + 4)
	entry(0x8769, 4, 1, subIFD)
	binary.Write(&b, be, uint32(0)) // no next IFD
	// Exif sub-IFD: DateTimeOriginal (ASCII, 20 bytes incl. NUL) stored after it.
	binary.Write(&b, be, uint16(1))
	dateAt := subIFD + 2 + 12 + 4
	entry(0x9003, 2, 20, dateAt)
	binary.Write(&b, be, uint32(0))
	b.WriteString(date)
	b.WriteByte(0)
	return b.Bytes()
}

// jpegWithExif encodes img as JPEG and inserts an APP1 EXIF segment after SOI.
func jpegWithExif(t *testing.T, img image.Image, tiff []byte) []byte {
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	seg := append([]byte("Exif\x00\x00"), tiff...)
	var out bytes.Buffer
	out.Write(enc.Bytes()[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(seg)+2))
	out.Write(seg)
	out.Write(enc.Bytes()[2:])
	return out.Bytes()
}

func TestConvertJPEGKeepsExifAndOrientation(t *testing.T) {
	// 64x32, left half red: orientation 6 (rotate 90° clockwise) must give 32x64 upright.
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{0, 0, 255, 255}
			if x < 32 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "IMG_1.JPG")
	os.WriteFile(src, jpegWithExif(t, img, testTIFF(6, "2024:05:06 07:08:09")), 0o644)
	dst := filepath.Join(dir, "IMG_1.avif")

	size, err := convertToAVIF(src, dst, 60)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	if int64(len(data)) != size {
		t.Fatalf("size %d, file %d", size, len(data))
	}
	out, err := avif.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := out.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
		t.Fatalf("output %v, want 32x64 (rotated)", b.Size())
	}
	// After a clockwise turn the red (left) half is at the top.
	if r, _, bl, _ := out.At(16, 8).RGBA(); r < bl {
		t.Fatalf("top of rotated image isn't red: r=%d b=%d", r>>8, bl>>8)
	}
	ex, err := avif.DecodeExif(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("no EXIF in output: %v", err)
	}
	if ex.DateTimeOriginal != "2024:05:06 07:08:09" || ex.Orientation != 1 {
		t.Fatalf("EXIF date=%q orientation=%d", ex.DateTimeOriginal, ex.Orientation)
	}
}

// With PHOTO_BACKUP_TEST_IMAGES set to a folder of real photos, converts each and checks
// that date, GPS and camera survive.
func TestConvertRealPhotos(t *testing.T) {
	dir := os.Getenv("PHOTO_BACKUP_TEST_IMAGES")
	if dir == "" {
		t.Skip("set PHOTO_BACKUP_TEST_IMAGES to run")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !convertible(e.Name()) {
			continue
		}
		src := filepath.Join(dir, e.Name())
		dst := filepath.Join(t.TempDir(), strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))+".avif")
		size, err := convertToAVIF(src, dst, 60)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		srcInfo, _ := os.Stat(src)
		data, _ := os.ReadFile(dst)
		got, err := avif.DecodeExif(bytes.NewReader(data))
		if err != nil {
			t.Errorf("%s: no EXIF in output: %v", e.Name(), err)
			continue
		}
		orig, _ := os.ReadFile(src)
		wantICC, _ := heicICC(orig)
		if strings.HasSuffix(strings.ToLower(e.Name()), ".jpg") {
			wantICC, _ = jpegICC(orig)
		}
		if gotICC, _ := heicICC(data); !bytes.Equal(wantICC, gotICC) {
			t.Errorf("%s: colour profile not kept (%d bytes, output %d)", e.Name(), len(wantICC), len(gotICC))
		}
		if _, err := avif.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("%s: output doesn't decode: %v", e.Name(), err)
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".heic") {
			want, err := heic.DecodeExif(bytes.NewReader(orig))
			if err == nil && (want.DateTimeOriginal != got.DateTimeOriginal || want.GPSLatitude != got.GPSLatitude || want.Model != got.Model) {
				t.Errorf("%s: EXIF changed: %+v vs %+v", e.Name(), want, got)
			}
		}
		t.Logf("%s: %d KB -> %d KB (%.0f%%), taken %s, GPS %.4f,%.4f, %s, orientation %d, ICC %d bytes",
			e.Name(), srcInfo.Size()/1024, size/1024, 100*float64(size)/float64(srcInfo.Size()),
			got.DateTimeOriginal, got.GPSLatitude, got.GPSLongitude, got.Model, got.Orientation, len(wantICC))
	}
}

// testJPEG is a 256x256 gradient saved at high JPEG quality, so AVIF has room to save
// (copies saving under 10% aren't kept).
func testJPEG(t *testing.T, shade uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(x), uint8(y), shade, 255
		}
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	seg := append([]byte("Exif\x00\x00"), testTIFF(1, "2024:01:02 03:04:05")...)
	var out bytes.Buffer
	out.Write(enc.Bytes()[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(seg)+2))
	out.Write(seg)
	out.Write(enc.Bytes()[2:])
	return out.Bytes()
}

// waitFor polls cond until it holds (or fails the test after 60 s).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Compressed photos stay "backed up" after their originals are deleted, for the phone
// that sent them, for other phones, and against re-uploads.
func TestCompressionKeepsPhotosBackedUp(t *testing.T) {
	e := newTestEnv(t)
	existing := testJPEG(t, 90)
	writeFile(t, filepath.Join(e.root, "old", "IMG_0005.JPG"), string(existing)) // copied before, e.g. Image Capture
	writeFile(t, filepath.Join(e.root, "clip.MOV"), "video")                     // videos are left alone

	d, release := e.srv.use()
	release()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	e.srv.mu.Lock()
	e.srv.compress = CompressSettings{Enabled: true, Format: "avif", PhotoLevel: defaultPhotoLevel}
	e.srv.mu.Unlock()
	d.StartConverter(e.srv.compressSettings, &e.srv.events)

	data := testJPEG(t, 200)
	e.uploadAs("dev1", "A1", "IMG_0001.JPG", "1700000000", data)
	e.do("POST", "/api/commit", mustJSON(commitRequest{DeviceID: "dev1", AssetID: "A1", MediaType: "image", Resources: []string{"photo"}}), nil)

	stats := func() ConversionStats { s, _ := d.store.conversionStats(true, false); return s }
	waitFor(t, "both photos converted", func() bool { s := stats(); return s.Converted == 2 && s.Remaining == 0 })
	for _, p := range []string{"IMG_0001.JPG", "IMG_0001.avif", "old/IMG_0005.JPG", "old/IMG_0005.avif", "clip.MOV"} {
		if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(p))); err != nil {
			t.Fatalf("%s: %v (originals are kept by default)", p, err)
		}
	}

	e.srv.mu.Lock()
	e.srv.compress.RemoveOriginals = true
	e.srv.mu.Unlock()
	d.WakeConverter()
	waitFor(t, "originals deleted", func() bool { return stats().Removed == 2 })
	for _, p := range []string{"IMG_0001.JPG", "old/IMG_0005.JPG"} {
		if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Fatalf("%s still there", p)
		}
	}

	// A rescan forgets the deleted originals; they must still count.
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()

	n, n5 := int64(len(data)), int64(len(existing))
	if got := e.checkAssets("dev1", photo("A1", "IMG_0001.JPG", n, 1700000000)); len(got) != 1 {
		t.Fatalf("sending phone: backed_up = %v", got)
	}
	got := e.checkAssets("phoneB", photo("B1", "IMG_0001.JPG", n, 1700000000), photo("B5", "IMG_0005.JPG", n5, 0))
	if len(got) != 2 {
		t.Fatalf("other phone: backed_up = %v, want [B1 B5]", got)
	}
	out := e.uploadAs("phoneB", "B1", "IMG_0001.JPG", "1700000000", data)
	if out["rel_path"] != "IMG_0001.JPG" {
		t.Fatalf("re-upload stored as a new file: %v", out)
	}
	if _, err := os.Stat(filepath.Join(e.root, "IMG_0001.JPG")); !os.IsNotExist(err) {
		t.Fatal("re-upload recreated the original")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// An ICC profile split across JPEG APP2 segments is reassembled and attached to the AVIF.
func TestConvertKeepsJPEGColourProfile(t *testing.T) {
	icc := bytes.Repeat([]byte("P3-profile-bytes"), 5000) // 80 KB: needs two segments
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	var enc bytes.Buffer
	jpeg.Encode(&enc, img, nil)
	var out bytes.Buffer
	out.Write(enc.Bytes()[:2])
	for i, part := range [][]byte{icc[:60000], icc[60000:]} {
		seg := append([]byte("ICC_PROFILE\x00"), byte(i+1), 2)
		seg = append(seg, part...)
		out.Write([]byte{0xFF, 0xE2})
		binary.Write(&out, binary.BigEndian, uint16(len(seg)+2))
		out.Write(seg)
	}
	out.Write(enc.Bytes()[2:])

	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.jpg"), filepath.Join(dir, "a.avif")
	os.WriteFile(src, out.Bytes(), 0o644)
	if _, err := convertToAVIF(src, dst, 60); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	got, err := heicICC(data)
	if err != nil || !bytes.Equal(got, icc) {
		t.Fatalf("profile not kept: %v (%d bytes)", err, len(got))
	}
	if _, err := avif.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("output doesn't decode: %v", err)
	}
}

// A paired phone can turn compression on and off (saved); format and quality are
// only changed on the computer.
func TestPhoneChangesCompression(t *testing.T) {
	e := newTestEnv(t)
	e.srv.compress = CompressSettings{Format: "avif", PhotoLevel: defaultPhotoLevel, VideoLevel: defaultVideoLevel}
	var info struct {
		Compression map[string]any `json:"compression"`
	}
	e.do("GET", "/api/info", nil, &info)
	if info.Compression["enabled"] != false || info.Compression["photo_level"] != "Indistinguishable side by side" {
		t.Fatalf("info compression = %v", info.Compression)
	}
	var out map[string]any
	if code := e.do("POST", "/api/compression", []byte(`{"compress":true,"photo_level":"small","format":"avif"}`), &out); code != 200 {
		t.Fatalf("status %d %v", code, out)
	}
	if out["enabled"] != true || out["photo_level"] != "Indistinguishable side by side" || out["remove_originals"] != false {
		t.Fatalf("after change = %v (quality must not change from the phone)", out)
	}
	cfg, err := LoadConfig(e.srv.cfgPath)
	if err != nil || !cfg.Compress || cfg.PhotoLevel != defaultPhotoLevel {
		t.Fatalf("saved config: %+v %v", cfg, err)
	}
	e.do("POST", "/api/compression", []byte(`{"compress_videos":true,"video_level":"good"}`), &out)
	if out["compress_videos"] != true || out["video_level"] != findVideoLevel(defaultVideoLevel).Label {
		t.Fatalf("videos from the phone = %v / level %v (level must not change from the phone)", out["compress_videos"], out["video_level"])
	}
	if code := e.do("POST", "/api/compression", []byte(`{`), nil); code != 400 {
		t.Fatalf("bad JSON: %d", code)
	}
}

// Photos converted at the same time never write to the same output name.
func TestParallelConversionNames(t *testing.T) {
	e := newTestEnv(t)
	for i, name := range []string{"IMG_7.JPG", "IMG_7.jpeg", "IMG_7.PNG", "IMG_8.JPG", "IMG_9.JPG", "IMG_10.JPG", "IMG_11.JPG", "IMG_12.JPG"} {
		data := testJPEG(t, uint8(20*i))
		if strings.HasSuffix(name, ".PNG") {
			img := image.NewRGBA(image.Rect(0, 0, 256, 256))
			for i := range img.Pix {
				img.Pix[i] = uint8(i * 7 % 251) // busy pattern: a big PNG
			}
			var b bytes.Buffer
			png.Encode(&b, img)
			data = b.Bytes()
		}
		writeFile(t, filepath.Join(e.root, name), string(data))
	}
	d, release := e.srv.use()
	release()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	e.srv.mu.Lock()
	e.srv.compress = CompressSettings{Enabled: true, Format: "avif", PhotoLevel: defaultPhotoLevel}
	e.srv.mu.Unlock()
	d.StartConverter(e.srv.compressSettings, &e.srv.events)
	var st ConversionStats
	waitFor(t, "all done", func() bool {
		st, _ = d.store.conversionStats(true, false)
		return st.Remaining == 0 && st.Converted+st.NotSmaller+st.Failed == 8
	})
	outs, _ := filepath.Glob(filepath.Join(e.root, "*.avif"))
	if st.Converted < 7 || int64(len(outs)) != st.Converted {
		t.Fatalf("%d converted (%d kept as they were, %d failed), %d AVIF files: %v", st.Converted, st.NotSmaller, st.Failed, len(outs), outs)
	}
}

// The test binary also serves as the conversion helper process, like the real program.
func TestMain(m *testing.M) {
	if os.Getenv(convertWorkerEnv) == "1" {
		runConvertWorker()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestHeifSize(t *testing.T) {
	dir := os.Getenv("PHOTO_BACKUP_TEST_IMAGES")
	if dir == "" {
		t.Skip("set PHOTO_BACKUP_TEST_IMAGES to run")
	}
	f, err := os.Open(filepath.Join(dir, "real.HEIC"))
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	w, h, err := heifSize(f)
	if err != nil || w*h != 4032*3024 {
		t.Fatalf("size %dx%d %v", w, h, err)
	}
}

func TestQualityPerKind(t *testing.T) {
	q := findPhotoLevel(defaultPhotoLevel).Qualities
	for name, want := range map[string]int{"IMG_1.HEIC": q.HEIC, "a.heif": q.HEIC, "DSC_1.JPG": q.JPEG, "x.jpeg": q.JPEG, "Shot.PNG": q.PNG, "y.webp": q.Other} {
		if got := q.For(name); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}
	if findPhotoLevel("nonsense").ID != defaultPhotoLevel || findVideoLevel("").ID != defaultVideoLevel {
		t.Fatal("unknown levels don't fall back to the defaults")
	}
}

// Configs from before levels (raw "quality"/"qualities"/"video_crf") get the default levels.
func TestConfigMigratesToLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"dest":"x","quality":50,"qualities":{"heic":70},"video_crf":34,"compress":true}`), 0o644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Compression()
	if c.PhotoLevel != defaultPhotoLevel || c.VideoLevel != defaultVideoLevel || !c.Enabled {
		t.Fatalf("settings = %+v", c)
	}
}

// A copy that wouldn't save 10% isn't kept: the original stays as it is, counted apart.
func TestNotSmallerCopyIsNotKept(t *testing.T) {
	e := newTestEnv(t)
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	seed := uint32(1)
	for i := range img.Pix { // noise: nothing to gain at visually lossless quality
		seed = seed*1664525 + 1013904223
		img.Pix[i] = uint8(seed >> 24)
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 40})
	writeFile(t, filepath.Join(e.root, "IMG_9.JPG"), b.String())
	d, release := e.srv.use()
	release()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	e.srv.mu.Lock()
	e.srv.compress = CompressSettings{Enabled: true, Format: "avif", PhotoLevel: "lossless"}
	e.srv.mu.Unlock()
	d.StartConverter(e.srv.compressSettings, &e.srv.events)
	var st ConversionStats
	waitFor(t, "handled", func() bool { st, _ = d.store.conversionStats(true, false); return st.Remaining == 0 })
	if st.NotSmaller != 1 || st.Converted != 0 || st.Failed != 0 {
		t.Fatalf("stats %+v, want 1 kept as it was", st)
	}
	if _, err := os.Stat(filepath.Join(e.root, "IMG_9.avif")); err == nil {
		t.Fatal("the copy that didn't save space was left on disk")
	}
}
