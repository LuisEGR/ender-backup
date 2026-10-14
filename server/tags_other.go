//go:build !darwin

package main

// Finder tags only exist on macOS; elsewhere favorites are only recorded in the index.
func setFavoriteTag(path string, favorite bool) error { return nil }

func favoriteTagged(path string) bool { return false }
