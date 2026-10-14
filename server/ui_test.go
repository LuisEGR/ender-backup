package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (e *testEnv) ui(method, path, contentType string, body any, mutate func(*http.Request)) (*http.Response, map[string]any) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, &buf)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestUIPageAndState(t *testing.T) {
	e := newTestEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	_, state := e.ui("GET", "/ui/state", "", nil, nil)
	if state["dest"] != e.root || !strings.HasPrefix(state["pair_url"].(string), "photobackup://pair?") {
		t.Fatalf("state = %v", state)
	}
	if resp, _ := e.ui("GET", "/ui/qr.png", "", nil, nil); resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr content-type %q", resp.Header.Get("Content-Type"))
	}
}

func TestUIRejectsNonLocalRequests(t *testing.T) {
	e := newTestEnv(t)
	// DNS rebinding: a foreign Host header is refused even over loopback.
	if resp, _ := e.ui("GET", "/ui/state", "", nil, func(r *http.Request) { r.Host = "evil.example:8765" }); resp.StatusCode != 403 {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}
	// A form post from another website (no JSON content type) is refused.
	if resp, _ := e.ui("POST", "/ui/new-token", "text/plain", map[string]any{}, nil); resp.StatusCode != 415 {
		t.Fatalf("non-JSON post: %d", resp.StatusCode)
	}
	// Cross-origin JSON is refused.
	resp, _ := e.ui("POST", "/ui/new-token", "application/json", map[string]any{},
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin post: %d", resp.StatusCode)
	}
}

func TestUISettingsSwitchesDestination(t *testing.T) {
	e := newTestEnv(t)
	newRoot := filepath.Join(t.TempDir(), "Elsewhere")
	resp, out := e.ui("POST", "/ui/settings", "application/json",
		map[string]any{"dest": newRoot, "name": "Studio PC", "device_folders": true}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("settings: %d %v", resp.StatusCode, out)
	}
	// New uploads land in the new folder, and the choice is persisted.
	e.upload("a1", "photo", "IMG_0001.JPG", []byte("x"))
	if _, err := os.Stat(filepath.Join(newRoot, "Test iPhone", "IMG_0001.JPG")); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(e.srv.cfgPath)
	if err != nil || cfg.Dest != newRoot || cfg.Name != "Studio PC" || !cfg.DeviceFolders {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	var info map[string]any
	e.do("GET", "/api/info", nil, &info)
	if info["name"] != "Studio PC" {
		t.Fatalf("info name = %v", info["name"])
	}
	// The phone is told where its files go, including its own subfolder.
	if want := filepath.Join(newRoot, "Test iPhone"); info["folder"] != want {
		t.Fatalf("info folder = %v, want %v", info["folder"], want)
	}
}

func TestInfoFolderWithoutDeviceFolders(t *testing.T) {
	e := newTestEnv(t)
	var info map[string]any
	e.do("GET", "/api/info", nil, &info)
	if info["folder"] != e.root {
		t.Fatalf("info folder = %v, want %v", info["folder"], e.root)
	}
	total, _ := info["disk_total"].(float64)
	free, _ := info["disk_free"].(float64)
	if total <= 0 || free <= 0 || free > total {
		t.Fatalf("disk usage total=%v free=%v", info["disk_total"], info["disk_free"])
	}
}

func TestUINewTokenInvalidatesOld(t *testing.T) {
	e := newTestEnv(t)
	if resp, _ := e.ui("POST", "/ui/new-token", "application/json", map[string]any{}, nil); resp.StatusCode != 200 {
		t.Fatalf("new-token: %d", resp.StatusCode)
	}
	if code := e.do("GET", "/api/info", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("old token still accepted: %d", code)
	}
}

func TestConfigWithByteOrderMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("\xEF\xBB\xBF{\"port\": 8770, \"name\": \"WinPc\"}"), 0o600)
	c, err := LoadConfig(path)
	if err != nil || c.Port != 8770 || c.Name != "WinPc" {
		t.Fatalf("LoadConfig with BOM = %+v, %v", c, err)
	}
}
