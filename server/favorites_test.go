package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

func fav(b bool) *bool { return &b }

// The phone's favorite flag is mirrored onto matching files — including files that
// were already in the folder — and follows later changes on the phone.
func TestFavoritesSync(t *testing.T) {
	e := newTestEnv(t)
	writeFile(t, filepath.Join(e.root, "IMG_0500.HEIC"), "existing")
	d, release := e.srv.use()
	d.StartIndexing(&e.srv.events)
	d.waitIndexed()
	release()

	asset := photo("a1", "IMG_0500.HEIC", int64(len("existing")), 0)
	count := func() int {
		d, release := e.srv.use()
		defer release()
		n, _ := d.store.FavoriteCount()
		return n
	}
	tagged := func() bool { return favoriteTagged(filepath.Join(e.root, "IMG_0500.HEIC")) }

	asset.Favorite = fav(true)
	e.checkAssets("phone", asset)
	if count() != 1 {
		t.Fatalf("favorites = %d after favoriting, want 1", count())
	}
	if runtime.GOOS == "darwin" && !tagged() {
		t.Fatal("file not tagged Favorite in Finder")
	}

	asset.Favorite = fav(false)
	e.checkAssets("phone", asset)
	if count() != 0 || tagged() {
		t.Fatalf("favorite not removed: count=%d tagged=%v", count(), tagged())
	}

	// Older apps don't send the flag: nothing changes.
	asset.Favorite = nil
	e.checkAssets("phone", asset)
	if count() != 0 {
		t.Fatalf("favorites = %d without a flag, want 0", count())
	}
}
