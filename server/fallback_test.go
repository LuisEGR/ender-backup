package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// missingDrivePath is a folder on a drive that isn't connected.
func missingDrivePath(t *testing.T) string {
	if runtime.GOOS == "windows" {
		for c := 'Z'; c > 'D'; c-- {
			if _, err := os.Stat(string(c) + `:\`); err != nil {
				return string(c) + `:\Photos`
			}
		}
		t.Skip("no free drive letter")
	}
	return "/Volumes/NotConnected-photo-backup-test/Photos"
}

func TestFallsBackWhenDriveMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	missing := missingDrivePath(t)

	d, warning, err := openOrFallBack(missing)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !samePath(d.root, defaultDest()) {
		t.Fatalf("using %s, want the default folder %s", d.root, defaultDest())
	}
	if !strings.Contains(warning, missing) {
		t.Fatalf("warning %q doesn't name the missing folder", warning)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("created the folder although its drive is missing")
	}

	// A folder that's simply new (its parent exists) is still created as before.
	fresh := filepath.Join(t.TempDir(), "PhotoBackup")
	d2, warning, err := openOrFallBack(fresh)
	if err != nil || warning != "" {
		t.Fatalf("new folder: %v %q", err, warning)
	}
	d2.Close()
}

func TestMountMissing(t *testing.T) {
	if mountMissing(t.TempDir()) || mountMissing(filepath.Join(t.TempDir(), "new", "deeper")) {
		t.Fatal("existing or creatable folder reported as missing drive")
	}
	if runtime.GOOS == "darwin" && !mountMissing("/Volumes/NotConnected-photo-backup-test/Photos") {
		t.Fatal("missing volume not detected")
	}
}

// Deleting a backed-up file on the computer changes the state token phones cache against.
func TestStateTokenChangesWhenFilesDisappear(t *testing.T) {
	e := newTestEnv(t)
	e.uploadAs("phoneA", "A-1", "IMG_0001.JPG", "1700000000", []byte("photo-one"))
	e.uploadAs("phoneA", "A-2", "IMG_0002.JPG", "1700000000", []byte("photo-two"))
	d, release := e.srv.use()
	release()
	d.StartWatching(time.Hour) // first sweep runs right away
	defer d.stopWatching()
	time.Sleep(200 * time.Millisecond)
	before := d.StateToken()
	d.sweep()
	if d.StateToken() != before {
		t.Fatal("token changed without any change on disk")
	}
	os.Remove(filepath.Join(e.root, "IMG_0002.JPG"))
	d.sweep()
	if d.StateToken() == before {
		t.Fatal("token didn't change after a backed-up file was deleted")
	}
	var info map[string]any
	e.do("GET", "/api/info", nil, &info)
	if info["state"] != d.StateToken() {
		t.Fatalf("info state %v, want %v", info["state"], d.StateToken())
	}
}
