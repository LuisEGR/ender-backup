//go:build darwin

package main

import (
	"errors"
	"strings"

	"golang.org/x/sys/unix"
	"howett.net/plist"
)

const (
	tagsAttr    = "com.apple.metadata:_kMDItemUserTags"
	favoriteTag = "Favorite"
	tagRed      = "6" // Finder tag colours: 1 gray, 2 green, 3 purple, 4 blue, 5 yellow, 6 red, 7 orange
)

// setFavoriteTag adds or removes a red "Favorite" Finder tag, keeping any other tags.
func setFavoriteTag(path string, favorite bool) error {
	var tags []string
	buf := make([]byte, 64<<10)
	if n, err := unix.Getxattr(path, tagsAttr, buf); err == nil {
		if _, err := plist.Unmarshal(buf[:n], &tags); err != nil {
			tags = nil
		}
	}
	// Tags are stored as "Name\nColour"; drop any existing Favorite tag, whatever its colour.
	kept := tags[:0]
	for _, t := range tags {
		if name, _, _ := strings.Cut(t, "\n"); name != favoriteTag {
			kept = append(kept, t)
		}
	}
	if favorite {
		kept = append(kept, favoriteTag+"\n"+tagRed)
	}
	if len(kept) == 0 {
		if err := unix.Removexattr(path, tagsAttr); err != nil && !errors.Is(err, unix.ENOATTR) {
			return err
		}
		return nil
	}
	data, err := plist.Marshal(kept, plist.BinaryFormat)
	if err != nil {
		return err
	}
	return unix.Setxattr(path, tagsAttr, data, 0)
}

// favoriteTagged reports whether a file carries the Favorite tag (for tests).
func favoriteTagged(path string) bool {
	buf := make([]byte, 64<<10)
	n, err := unix.Getxattr(path, tagsAttr, buf)
	if err != nil {
		return false
	}
	var tags []string
	plist.Unmarshal(buf[:n], &tags)
	for _, t := range tags {
		if name, _, _ := strings.Cut(t, "\n"); name == favoriteTag {
			return true
		}
	}
	return false
}
