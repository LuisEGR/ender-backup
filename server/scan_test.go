package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Photos copied into the folder before (e.g. with Image Capture) are recognised.
func TestScanRecognisesExistingFiles(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.root, "IMG_0100.HEIC"), "heic-bytes")
	writeFile(t, filepath.Join(e.root, "2023", "IMG_0101.MOV"), "mov-bytes")
	writeFile(t, filepath.Join(e.root, "._IMG_0100.HEIC"), "resource fork")             // hidden: ignored
	writeFile(t, filepath.Join(e.root, ".Spotlight-V100", "IMG_0102.JPG"), "spotlight") // system: ignored

	d, release := e.srv.use()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	_, count := d.Indexing()
	release()
	if count != 2 {
		t.Fatalf("indexed %d files, want 2", count)
	}

	got := e.checkAssets("phone",
		photo("a", "IMG_0100.HEIC", int64(len("heic-bytes")), 1700000000), // capture time unknown for scanned files
		photo("b", "img_0101.mov", int64(len("mov-bytes")), 0),            // names compare case-insensitively
		photo("c", "IMG_0100.HEIC", 999, 0),                               // different size
		photo("d", "IMG_0102.JPG", int64(len("spotlight")), 0),            // was in a skipped folder
	)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("backed_up = %v, want [a b]", got)
	}
}

// Uploading a photo that is already in the folder doesn't store it again; a different
// photo that happens to have the same name is kept alongside.
func TestUploadDedupesAgainstExistingFiles(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.root, "IMG_0200.JPG"), "original")
	d, release := e.srv.use()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	release()

	same := e.uploadAs("phone", "p1", "IMG_0200.JPG", "0", []byte("original"))
	if same["rel_path"] != "IMG_0200.JPG" {
		t.Fatalf("identical upload stored at %v, want the existing IMG_0200.JPG", same["rel_path"])
	}
	other := e.uploadAs("phone", "p2", "IMG_0200.JPG", "0", []byte("somethin")) // same size, other bytes
	if other["rel_path"] != "IMG_0200_1.JPG" {
		t.Fatalf("different photo stored at %v, want IMG_0200_1.JPG", other["rel_path"])
	}
}

// A rescan forgets files that were deleted from the folder.
func TestRescanDropsDeletedFiles(t *testing.T) {
	e := newTestEnv(t)
	path := filepath.Join(e.root, "IMG_0300.JPG")
	writeFile(t, path, "bytes")
	d, release := e.srv.use()
	defer release()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	os.Remove(path)
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	var n int
	d.store.db.QueryRow(`SELECT COUNT(*) FROM scanned`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d scanned rows after deleting the file, want 0", n)
	}
}
