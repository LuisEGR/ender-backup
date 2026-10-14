package main

import (
	"path/filepath"
	"testing"
)

func TestLibrarySummary(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "index.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, f := range []FileRecord{
		{DeviceID: "p", AssetID: "a", Resource: "photo", OriginalName: "IMG_1.HEIC", RelPath: "IMG_1.HEIC", Size: 100, SHA256: "x1"},
		{DeviceID: "p", AssetID: "b", Resource: "video", OriginalName: "IMG_2.MOV", RelPath: "IMG_2.MOV", Size: 1000, SHA256: "x2"},
		{DeviceID: "p", AssetID: "c", Resource: "photo", OriginalName: "IMG_3.avif", RelPath: "IMG_3.avif", Size: 5, SHA256: "x3"},
	} {
		if err := st.PutFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`INSERT INTO conversions (src_rel, src_name, src_size, out_rel, out_size, format, converted_at)
		VALUES ('IMG_1.HEIC', 'IMG_1.HEIC', 100, 'IMG_1.avif', 40, 'avif', 1), ('IMG_2.MOV', 'IMG_2.MOV', 1000, 'IMG_2.av1.mp4', 300, 'av1', 1)`); err != nil {
		t.Fatal(err)
	}
	s, err := st.LibrarySummary()
	if err != nil {
		t.Fatal(err)
	}
	if s.Photos != (MediaCount{1, 100, 40}) || s.Videos != (MediaCount{1, 1000, 300}) {
		t.Errorf("originals: %+v", s)
	}
	if s.PhotoCopies.Before != 100 || s.PhotoCopies.After != 40 || s.VideoCopies.After != 300 {
		t.Errorf("copies: %+v", s)
	}

	// Originals deleted after compressing count once, by their copy: one still listed
	// with the app's uploads, one only known from its conversion.
	if _, err := st.db.Exec(`UPDATE conversions SET src_removed = 1 WHERE src_rel = 'IMG_1.HEIC';
		INSERT INTO conversions (src_rel, src_name, src_size, out_rel, out_size, format, converted_at, src_removed)
		VALUES ('OLD_9.JPG', 'OLD_9.JPG', 70, 'OLD_9.avif', 20, 'avif', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	s, _ = st.LibrarySummary()
	if s.Photos != (MediaCount{2, 0, 60}) || s.PhotoCopies.Removed != 2 {
		t.Errorf("after deleting originals: %+v", s)
	}
}
