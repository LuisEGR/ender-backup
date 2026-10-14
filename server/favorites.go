package main

import (
	"log"
	"path/filepath"
	"time"
)

// Favorites marked on the phone are mirrored onto the backed-up files: recorded in
// the index and, on macOS, shown as a "Favorite" Finder tag. The phone sends the
// flag with every status check, so changes on the phone (and photos that were in
// the folder before, e.g. from Image Capture) are synced automatically.

const favoritesSchema = `
CREATE TABLE IF NOT EXISTS favorites (
	rel_path   TEXT PRIMARY KEY,
	updated_at INTEGER NOT NULL
);
`

// SetFavorite records whether a file is a favorite and reports whether that changed.
func (s *Store) SetFavorite(rel string, favorite bool) (bool, error) {
	var q string
	var args []any
	if favorite {
		q, args = `INSERT OR IGNORE INTO favorites (rel_path, updated_at) VALUES (?, ?)`, []any{rel, time.Now().Unix()}
	} else {
		q, args = `DELETE FROM favorites WHERE rel_path = ?`, []any{rel}
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) FavoriteCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM favorites`).Scan(&n)
	return n, err
}

// syncFavorite applies the phone's favorite flag to the files of one asset.
func (s *Server) syncFavorite(d *Destination, paths []string, favorite bool) {
	for _, rel := range paths {
		changed, err := d.store.SetFavorite(rel, favorite)
		if err != nil {
			log.Printf("favorite %s: %v", rel, err)
			continue
		}
		if !changed {
			continue
		}
		// Tag the file that's actually there (the converted copy if the original was removed).
		if err := setFavoriteTag(filepath.Join(d.root, filepath.FromSlash(d.store.resolve(rel))), favorite); err != nil {
			log.Printf("Finder tag %s: %v", rel, err)
		}
		if favorite {
			s.events.add("Marked %s as favorite", rel)
		} else {
			s.events.add("Removed favorite from %s", rel)
		}
	}
}
