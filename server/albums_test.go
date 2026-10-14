package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func (e *testEnv) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(rel)))
	return err == nil
}

func (e *testEnv) uploadToAlbums(asset, filename string, data []byte, albums ...string) string {
	q := url.Values{"device_id": {"dev1"}, "asset_id": {asset}, "resource": {"photo"},
		"filename": {filename}, "created": {"1700000000"}, "album": albums}
	var out map[string]any
	if code := e.do("POST", "/api/upload?"+q.Encode(), data, &out); code != 200 {
		e.t.Fatalf("upload: %d %v", code, out)
	}
	body, _ := json.Marshal(commitRequest{DeviceID: "dev1", AssetID: asset, MediaType: "image",
		Resources: []string{"photo"}, Albums: albums})
	if code := e.do("POST", "/api/commit", body, nil); code != 200 {
		e.t.Fatalf("commit: %d", code)
	}
	return out["rel_path"].(string)
}

func (e *testEnv) checkWithAlbums(id, name string, size int64, albums ...string) []any {
	a := photo(id, name, size, 1700000000)
	a.Albums = albums
	if a.Albums == nil {
		a.Albums = []string{}
	}
	return e.checkAssets("dev1", a)
}

func TestAlbumFolders(t *testing.T) {
	e := newTestEnv(t)
	e.srv.albumFolders.Store(true)
	data := []byte("whatsapp-photo")
	n := int64(len(data))

	// In two albums: stored in the first (A–Z), copied into the other.
	rel := e.uploadToAlbums("A1", "IMG_0001.JPG", data, "WhatsApp", "Trip/2024")
	if rel != "Trip_2024/IMG_0001.JPG" {
		t.Fatalf("stored at %q", rel)
	}
	if !e.exists("WhatsApp/IMG_0001.JPG") {
		t.Fatal("no copy in WhatsApp/")
	}

	// Taken out of the trip album: moves to WhatsApp/, replacing the copy; still backed up.
	if got := e.checkWithAlbums("A1", "IMG_0001.JPG", n, "WhatsApp"); len(got) != 1 {
		t.Fatalf("not backed up after move: %v", got)
	}
	if e.exists("Trip_2024/IMG_0001.JPG") || !e.exists("WhatsApp/IMG_0001.JPG") || e.exists("WhatsApp/IMG_0001_1.JPG") {
		t.Fatal("not moved cleanly to WhatsApp/")
	}

	// In no album any more: stays where it is.
	e.checkWithAlbums("A1", "IMG_0001.JPG", n)
	if !e.exists("WhatsApp/IMG_0001.JPG") {
		t.Fatal("photo in no album was moved")
	}

	// A file copied in before (Image Capture) that matches a photo in an album moves too.
	writeFile(t, filepath.Join(e.root, "IMG_0500.HEIC"), "image-capture")
	d, release := e.srv.use()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	release()
	if got := e.checkWithAlbums("B1", "IMG_0500.HEIC", int64(len("image-capture")), "WhatsApp"); len(got) != 1 {
		t.Fatalf("image capture file not matched: %v", got)
	}
	if !e.exists("WhatsApp/IMG_0500.HEIC") || e.exists("IMG_0500.HEIC") {
		t.Fatal("image capture file not moved into WhatsApp/")
	}
	if got := e.checkWithAlbums("B1", "IMG_0500.HEIC", int64(len("image-capture")), "WhatsApp"); len(got) != 1 {
		t.Fatalf("not backed up after moving: %v", got)
	}

	// Old app versions (no album info) change nothing.
	e.checkAssets("dev1", photo("A1", "IMG_0001.JPG", n, 1700000000))
	if !e.exists("WhatsApp/IMG_0001.JPG") {
		t.Fatal("moved without album info")
	}
}

// With album folders off, uploads go to the main folder as before.
func TestAlbumFoldersOff(t *testing.T) {
	e := newTestEnv(t)
	if rel := e.uploadToAlbums("A1", "IMG_0002.JPG", []byte("x"), "WhatsApp"); rel != "IMG_0002.JPG" {
		t.Fatalf("stored at %q", rel)
	}
}

// A compressed photo whose original was deleted moves with its AVIF and stays backed up.
func TestAlbumMoveKeepsCompressedCopy(t *testing.T) {
	e := newTestEnv(t)
	data := []byte("original-bytes")
	rel := e.uploadToAlbums("A1", "IMG_0003.JPG", data)
	d, release := e.srv.use()
	release()
	writeFile(t, filepath.Join(e.root, "IMG_0003.avif"), "avif")
	c := convertCandidate{rel: rel, name: "IMG_0003.JPG", size: int64(len(data))}
	if err := d.store.recordConversion(c, "IMG_0003.avif", 4, "avif", 50, nil, convDetail{}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.root, rel)) // "delete originals"

	e.srv.albumFolders.Store(true)
	if got := e.checkWithAlbums("A1", "IMG_0003.JPG", int64(len(data)), "Family"); len(got) != 1 {
		t.Fatalf("not backed up: %v", got)
	}
	if !e.exists("Family/IMG_0003.avif") || e.exists("IMG_0003.avif") {
		t.Fatal("compressed copy not moved into Family/")
	}
	if got := e.checkWithAlbums("A1", "IMG_0003.JPG", int64(len(data)), "Family"); len(got) != 1 {
		t.Fatalf("not backed up after move: %v", got)
	}
}

// The info reports how much the backup folder itself uses (counted in the background).
func TestFolderBytes(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.root, "a", "IMG_1.JPG"), "12345")
	writeFile(t, filepath.Join(e.root, "IMG_2.JPG"), "1234567890")
	var info map[string]any
	waitFor(t, "folder size", func() bool {
		e.do("GET", "/api/info", nil, &info)
		n, ok := info["folder_bytes"].(float64)
		return ok && n >= 15 // plus the index database in .photo-backup
	})
}
