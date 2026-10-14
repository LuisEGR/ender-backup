package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type testEnv struct {
	t    *testing.T
	ts   *httptest.Server
	root string
	srv  *Server
}

func newTestEnv(t *testing.T) *testEnv {
	root := t.TempDir()
	d, err := OpenDestination(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(d, "secret", "test")
	srv.port, srv.hosts, srv.id = 8765, []string{"127.0.0.1"}, "srv123"
	srv.cfgPath = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, ts: ts, root: root, srv: srv}
}

func (e *testEnv) do(method, path string, body []byte, out any) int {
	req, _ := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Device-Id", "dev1")
	req.Header.Set("X-Device-Name", "Test iPhone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (e *testEnv) upload(asset, resource, filename string, data []byte) map[string]any {
	q := url.Values{"device_id": {"dev1"}, "asset_id": {asset}, "resource": {resource},
		"filename": {filename}, "created": {"1600000000"}}
	var out map[string]any
	if code := e.do("POST", "/api/upload?"+q.Encode(), data, &out); code != 200 {
		e.t.Fatalf("upload %s: %d %v", filename, code, out)
	}
	return out
}

func (e *testEnv) check(ids ...string) []any {
	body, _ := json.Marshal(checkRequest{DeviceID: "dev1", AssetIDs: ids})
	var out struct {
		BackedUp []any `json:"backed_up"`
	}
	e.do("POST", "/api/check", body, &out)
	return out.BackedUp
}

func (e *testEnv) commit(asset string, resources ...string) int {
	body, _ := json.Marshal(commitRequest{DeviceID: "dev1", AssetID: asset, MediaType: "image", Resources: resources})
	return e.do("POST", "/api/commit", body, nil)
}

func TestAuthRequired(t *testing.T) {
	e := newTestEnv(t)
	resp, err := http.Get(e.ts.URL + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

func TestUploadCommitCheck(t *testing.T) {
	e := newTestEnv(t)
	const asset = "ABC-123/L0/001"

	out := e.upload(asset, "photo", "IMG_0001.HEIC", []byte("photo-bytes"))
	if got := out["rel_path"]; got != "IMG_0001.HEIC" {
		t.Fatalf("rel_path = %v", got)
	}
	// Not safe to delete until committed, and commit fails while a resource is missing.
	if got := e.check(asset); len(got) != 0 {
		t.Fatalf("uncommitted asset reported backed up: %v", got)
	}
	if code := e.commit(asset, "photo", "live_video"); code != http.StatusConflict {
		t.Fatalf("commit with missing resource: %d", code)
	}
	e.upload(asset, "live_video", "IMG_0001.MOV", []byte("video-bytes"))
	if code := e.commit(asset, "photo", "live_video"); code != 200 {
		t.Fatalf("commit: %d", code)
	}
	if got := e.check(asset, "other"); len(got) != 1 || got[0] != asset {
		t.Fatalf("check = %v", got)
	}

	// If a file disappears from disk, the asset is no longer safe to delete.
	os.Remove(filepath.Join(e.root, "IMG_0001.MOV"))
	if got := e.check(asset); len(got) != 0 {
		t.Fatalf("asset with missing file reported backed up: %v", got)
	}
}

func TestNameCollisionAndDedupe(t *testing.T) {
	e := newTestEnv(t)
	a := e.upload("a1", "photo", "IMG_0001.JPG", []byte("one"))
	b := e.upload("a2", "photo", "IMG_0001.JPG", []byte("two"))
	c := e.upload("a3", "photo", "IMG_9999.JPG", []byte("one"))
	if a["rel_path"] != "IMG_0001.JPG" || b["rel_path"] != "IMG_0001_1.JPG" {
		t.Fatalf("collision naming: %v, %v", a["rel_path"], b["rel_path"])
	}
	if c["rel_path"] != a["rel_path"] {
		t.Fatalf("identical content not deduplicated: %v", c["rel_path"])
	}
}

func TestDeviceFolders(t *testing.T) {
	root := t.TempDir()
	st, err := OpenStore(filepath.Join(root, "index.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, c := range []struct{ id, name, want string }{
		{"d1", "iPhone", "iPhone"},
		{"d2", "iphone", "iphone (2)"},    // folders compare case-insensitively (Windows/macOS)
		{"d1", "Renamed Phone", "iPhone"}, // stays fixed after first use
		{"d3", "", "device-d3"},
	} {
		got, err := st.DeviceFolder(c.id, c.name)
		if err != nil || got != c.want {
			t.Errorf("DeviceFolder(%q, %q) = %q, %v; want %q", c.id, c.name, got, err, c.want)
		}
	}
}

func TestSizeMismatchRejected(t *testing.T) {
	e := newTestEnv(t)
	q := url.Values{"device_id": {"dev1"}, "asset_id": {"x"}, "resource": {"photo"},
		"filename": {"a.jpg"}, "size": {"999"}}
	if code := e.do("POST", "/api/upload?"+q.Encode(), []byte("short"), nil); code != 400 {
		t.Fatalf("got %d, want 400", code)
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"IMG_1.HEIC":       "IMG_1.HEIC",
		"../../etc/passwd": "passwd",
		`C:\x\y.jpg`:       "y.jpg",
		"a:b?.jpg":         "a_b_.jpg",
		"..":               "",
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpeedTestEndpoint(t *testing.T) {
	e := newTestEnv(t)
	var out map[string]any
	if code := e.do("POST", "/api/speedtest", make([]byte, 1<<20), &out); code != 200 || out["bytes"] != float64(1<<20) {
		t.Fatalf("speedtest: %d %v", code, out)
	}
}

func TestStoredFilesAreReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	e := newTestEnv(t)
	e.upload("p1", "photo", "IMG_0700.JPG", []byte("x"))
	st, err := os.Stat(filepath.Join(e.root, "IMG_0700.JPG"))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, %v; want 0644", st.Mode().Perm(), err)
	}
}
