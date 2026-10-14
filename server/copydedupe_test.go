package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDupKey(t *testing.T) {
	same := [][2]string{{"KGJS9586.MP4", "KGJS9586 1.MP4"}, {"a/IMG_1.HEIC", "a/IMG_1 (2).heic"}, {"X 1.JPG", "X 2.JPG"}}
	for _, p := range same {
		if dupKey(p[0], 10) != dupKey(p[1], 10) {
			t.Errorf("%s and %s should match", p[0], p[1])
		}
	}
	diff := [][2]string{{"IMG_0001.HEIC", "IMG_0002.HEIC"}, {"a/X.MP4", "b/X.MP4"}, {"X.JPG", "X.HEIC"}}
	for _, p := range diff {
		if dupKey(p[0], 10) == dupKey(p[1], 10) {
			t.Errorf("%s and %s shouldn't match", p[0], p[1])
		}
	}
	if dupKey("X.JPG", 10) == dupKey("X 1.JPG", 11) {
		t.Error("different sizes shouldn't match")
	}
}

func TestDuplicateSharesCopy(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDestination(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	content := []byte("same original bytes")
	for _, n := range []string{"V.MOV", "V 1.MOV"} {
		os.WriteFile(filepath.Join(dir, n), content, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "V.av1.mp4"), []byte("copy"), 0o644)
	d.store.recordConversion(convertCandidate{rel: "V.MOV", name: "V.MOV", size: int64(len(content))}, "V.av1.mp4", 4, "av1", 38, nil, convDetail{})

	var events eventLog
	d.convertOne(context.Background(), convertCandidate{rel: "V 1.MOV", name: "V 1.MOV", size: int64(len(content))},
		CompressSettings{Enabled: true, Videos: true, VideoLevel: defaultVideoLevel}, &events)
	var out string
	d.store.db.QueryRow(`SELECT out_rel FROM conversions WHERE src_rel = 'V 1.MOV'`).Scan(&out)
	if out != "V.av1.mp4" {
		t.Fatalf("duplicate got copy %q, want the shared V.av1.mp4", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "V 1.av1.mp4")); err == nil {
		t.Fatal("a second copy was made")
	}
	// Same name and size but different content: not a duplicate.
	os.WriteFile(filepath.Join(dir, "W.MOV"), []byte("aaaa"), 0o644)
	os.WriteFile(filepath.Join(dir, "W 1.MOV"), []byte("bbbb"), 0o644)
	os.WriteFile(filepath.Join(dir, "W.av1.mp4"), []byte("c"), 0o644)
	d.store.recordConversion(convertCandidate{rel: "W.MOV", name: "W.MOV", size: 4}, "W.av1.mp4", 1, "av1", 38, nil, convDetail{})
	if _, _, _, _, ok := d.store.existingCopy("W 1.MOV", 4); ok {
		t.Fatal("different content treated as a duplicate")
	}
}

func TestDedupeCopies(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDestination(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Two copies made from duplicate originals (both deleted after compressing), identical.
	for _, n := range []string{"P.avif", "P 1.avif"} {
		os.WriteFile(filepath.Join(dir, n), []byte("identical copy"), 0o644)
	}
	d.store.db.Exec(`INSERT INTO conversions (src_rel, src_name, src_size, out_rel, out_size, format, converted_at, src_removed)
		VALUES ('P.HEIC', 'P.HEIC', 100, 'P.avif', 14, 'avif', 1, 1), ('P 1.HEIC', 'P 1.HEIC', 100, 'P 1.avif', 14, 'avif', 2, 1)`)
	var events eventLog
	d.dedupeCopies(&events, make(chan struct{}))
	if _, err := os.Stat(filepath.Join(dir, "P 1.avif")); err == nil {
		t.Fatal("the duplicate copy is still there")
	}
	var out string
	d.store.db.QueryRow(`SELECT out_rel FROM conversions WHERE src_rel = 'P 1.HEIC'`).Scan(&out)
	if out != "P.avif" || !d.store.present("P 1.HEIC", 100) {
		t.Fatalf("P 1.HEIC now points at %q (present %v)", out, d.store.present("P 1.HEIC", 100))
	}
	if s, _ := d.store.LibrarySummary(); s.PhotoCopies.Count != 1 || s.Photos.CopyBytes != 14 {
		t.Errorf("summary counts the shared copy twice: %+v", s)
	}
}
