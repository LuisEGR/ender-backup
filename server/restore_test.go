package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWantOriginalsBack(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "index.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A photo whose original was deleted after compressing: only the copy is on disk.
	if err := os.WriteFile(filepath.Join(dir, "IMG_1.avif"), []byte("copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO conversions (src_rel, src_name, src_size, out_rel, out_size, format, converted_at, src_removed)
		VALUES ('IMG_1.HEIC', 'IMG_1.HEIC', 10, 'IMG_1.avif', 4, 'avif', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if !st.present("IMG_1.HEIC", 10) {
		t.Fatal("the copy should count before asking for the original back")
	}
	if n, bytes, err := st.WantOriginalsBack("", 0); err != nil || n != 1 || bytes != 10 {
		t.Fatalf("WantOriginalsBack = %d %d %v", n, bytes, err)
	}
	if st.present("IMG_1.HEIC", 10) {
		t.Fatal("the copy still counts as the backup")
	}
	// Videos only, with a size limit: the photo above is already flagged, nothing else fits.
	st.db.Exec(`INSERT INTO conversions (src_rel, src_name, src_size, out_rel, out_size, format, converted_at, src_removed)
		VALUES ('V1.MOV', 'V1.MOV', 500, 'V1.av1.mp4', 100, 'av1', 2, 1), ('V2.MOV', 'V2.MOV', 300, 'V2.av1.mp4', 100, 'av1', 3, 1)`)
	if n, bytes, _ := st.WantOriginalsBack("av1", 400); n != 1 || bytes != 300 {
		t.Fatalf("videos within 400 bytes: %d %d (want the newer, 300-byte one)", n, bytes)
	}
	if n, _ := st.originalsWanted(); n != 2 {
		t.Fatalf("wanted %d", n)
	}
	// The phone sends the original again.
	if err := os.WriteFile(filepath.Join(dir, "IMG_1.HEIC"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.originalBack("IMG_1.HEIC"); err != nil {
		t.Fatal(err)
	}
	if !st.present("IMG_1.HEIC", 10) {
		t.Fatal("original back but not present")
	}
	var removed, restore int
	st.db.QueryRow(`SELECT src_removed, restore FROM conversions WHERE src_rel = 'IMG_1.HEIC'`).Scan(&removed, &restore)
	if removed != 0 || restore != 0 {
		t.Fatalf("flags after the original came back: removed %d restore %d", removed, restore)
	}
}
