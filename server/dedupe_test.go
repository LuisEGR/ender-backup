package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// uploadAs uploads one resource as the given phone.
func (e *testEnv) uploadAs(device, asset, filename string, created string, data []byte) map[string]any {
	q := url.Values{"device_id": {device}, "asset_id": {asset}, "resource": {"photo"},
		"filename": {filename}, "created": {created}}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/upload?"+q.Encode(), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Device-Id", device)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		e.t.Fatalf("upload: %d %v", resp.StatusCode, out)
	}
	return out
}

func (e *testEnv) checkAssets(device string, assets ...checkAsset) []any {
	body, _ := json.Marshal(checkRequest{DeviceID: device, Assets: assets})
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/check", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		BackedUp []any `json:"backed_up"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.BackedUp
}

func photo(id, name string, size, created int64) checkAsset {
	return checkAsset{ID: id, CreatedAt: created, Resources: []ResourceFingerprint{{Name: name, Size: size}}}
}

// A photo sent by one phone counts as backed up for another phone (or a reinstall of
// the app, which gets a new device ID) when name, size and capture time match.
func TestCheckMatchesContentAcrossPhones(t *testing.T) {
	e := newTestEnv(t)
	data := []byte("shared-photo-bytes")
	e.uploadAs("phoneA", "A-1", "IMG_0042.HEIC", "1700000000", data)
	n := int64(len(data))

	got := e.checkAssets("phoneB",
		photo("B-1", "IMG_0042.HEIC", n, 1700000000),   // same photo on phone B
		photo("B-2", "IMG_0042.HEIC", n+1, 1700000000), // different size
		photo("B-3", "IMG_0042.HEIC", n, 1700000999),   // different capture time
		photo("B-4", "IMG_9999.HEIC", n, 1700000000),   // different name
	)
	if len(got) != 1 || got[0] != "B-1" {
		t.Fatalf("backed_up = %v, want [B-1]", got)
	}

	// A Live Photo only counts when both parts are stored.
	live := checkAsset{ID: "B-5", CreatedAt: 1700000000, Resources: []ResourceFingerprint{
		{Name: "IMG_0042.HEIC", Size: n}, {Name: "IMG_0042.MOV", Size: 10},
	}}
	if got := e.checkAssets("phoneB", live); len(got) != 0 {
		t.Fatalf("live photo with missing video reported backed up: %v", got)
	}

	// Deleting the file on the computer means it's no longer backed up.
	os.Remove(filepath.Join(e.root, "IMG_0042.HEIC"))
	if got := e.checkAssets("phoneB", photo("B-1", "IMG_0042.HEIC", n, 1700000000)); len(got) != 0 {
		t.Fatalf("missing file reported backed up: %v", got)
	}
}

// The same bytes uploaded by two phones are stored once.
func TestUploadDedupesAcrossPhones(t *testing.T) {
	e := newTestEnv(t)
	data := []byte("same-bytes")
	a := e.uploadAs("phoneA", "A-1", "IMG_0001.JPG", "1700000000", data)
	b := e.uploadAs("phoneB", "B-7", "IMG_0001.JPG", "1700000000", data)
	if a["rel_path"] != b["rel_path"] {
		t.Fatalf("stored twice: %v and %v", a["rel_path"], b["rel_path"])
	}
	entries, _ := os.ReadDir(e.root)
	files := 0
	for _, en := range entries {
		if !en.IsDir() {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("%d files on disk, want 1", files)
	}
}

// Databases created before created_at existed are upgraded in place.
func TestMigratesOldDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "index.db")
	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE files (device_id TEXT NOT NULL, asset_id TEXT NOT NULL,
		resource TEXT NOT NULL, original_name TEXT NOT NULL, rel_path TEXT NOT NULL, size INTEGER NOT NULL,
		sha256 TEXT NOT NULL, uploaded_at INTEGER NOT NULL, PRIMARY KEY (device_id, asset_id, resource));
		INSERT INTO files VALUES ('d', 'a', 'photo', 'IMG_1.JPG', 'IMG_1.JPG', 3, 'x', 1);`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	os.WriteFile(filepath.Join(root, "IMG_1.JPG"), []byte("abc"), 0o644)

	st, err := OpenStore(dbPath, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Old rows have no capture time, so they match on name + size alone.
	ok, err := st.MatchesContent([]ResourceFingerprint{{Name: "IMG_1.JPG", Size: 3}}, 1700000000)
	if err != nil || !ok {
		t.Fatalf("MatchesContent on migrated db = %v, %v", ok, err)
	}
}
