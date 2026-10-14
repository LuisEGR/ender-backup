//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

// Adding and removing the Favorite tag keeps the user's own tags.
func TestFavoriteTagKeepsOtherTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "IMG.JPG")
	os.WriteFile(path, []byte("x"), 0o644)
	data, _ := plist.Marshal([]string{"Work\n4"}, plist.BinaryFormat)
	if err := unix.Setxattr(path, tagsAttr, data, 0); err != nil {
		t.Fatal(err)
	}
	read := func() []string {
		buf := make([]byte, 4096)
		n, err := unix.Getxattr(path, tagsAttr, buf)
		if err != nil {
			return nil
		}
		var tags []string
		plist.Unmarshal(buf[:n], &tags)
		return tags
	}

	if err := setFavoriteTag(path, true); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 2 || got[0] != "Work\n4" || got[1] != "Favorite\n6" {
		t.Fatalf("tags = %q", got)
	}
	if err := setFavoriteTag(path, false); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 1 || got[0] != "Work\n4" {
		t.Fatalf("tags after removing = %q", got)
	}
}
