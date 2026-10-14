package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Album folders: photos in an album on the phone (including albums apps create, like
// "WhatsApp") are saved in a folder named after it. A photo in several albums is stored
// in the first one (A–Z) and copied into the others — as a hard link where the disk
// supports it, so the copies take no extra space. This applies to every file matched to
// the phone's photo, including ones copied into the folder before (Image Capture…).

const albumCopiesSchema = `
CREATE TABLE IF NOT EXISTS album_copies (
	rel_path TEXT PRIMARY KEY,
	src_rel  TEXT NOT NULL, -- the file it's a copy of
	album    TEXT NOT NULL, -- folder name
	size     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS album_copies_src ON album_copies (src_rel);
`

// albumDirs turns album names into folder names: cleaned up, without duplicates, A–Z.
func albumDirs(albums []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range albums {
		// "Trip/2024" is one album, not a folder inside another.
		a = strings.NewReplacer("/", "_", "\\", "_").Replace(strings.TrimSpace(a))
		dir := strings.TrimRight(sanitizeFilename(a), ". ")
		if dir == "" || seen[strings.ToLower(dir)] {
			continue
		}
		seen[strings.ToLower(dir)] = true
		out = append(out, dir)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// albumFolder is where a new upload goes: <base>/<first album>, or <base> without albums.
func albumFolder(base string, albums []string) string {
	if dirs := albumDirs(albums); len(dirs) > 0 {
		return path.Join(base, dirs[0])
	}
	return base
}

// freeName returns <dir>/<name>, or <dir>/<name>_N.<ext> if that is taken.
func (s *Store) freeName(dir, name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s_%d%s", base, i, ext)
		}
		rel := path.Join(dir, n)
		if _, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(rel))); errors.Is(err, os.ErrNotExist) {
			return rel
		}
	}
}

// relocate moves a stored file (and its compressed copy) into dir, updating every
// table that refers to it. It returns the new path.
func (s *Store) relocate(rel, dir string) (string, error) {
	if path.Dir(rel) == path.Clean(dir) || (path.Dir(rel) == "." && dir == "") {
		return rel, nil
	}
	if err := os.MkdirAll(filepath.Join(s.root, filepath.FromSlash(dir)), 0o755); err != nil {
		return "", err
	}
	newRel := s.freeName(dir, path.Base(rel))
	src := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.Rename(src, filepath.Join(s.root, filepath.FromSlash(newRel))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err // a missing original is fine if it was compressed and removed
	}
	var outRel string
	err := s.db.QueryRow(`SELECT out_rel FROM conversions WHERE src_rel = ? AND out_rel <> ''`, rel).Scan(&outRel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	newOut := ""
	if outRel != "" {
		newOut = s.freeName(dir, path.Base(outRel))
		if err := os.Rename(filepath.Join(s.root, filepath.FromSlash(outRel)),
			filepath.Join(s.root, filepath.FromSlash(newOut))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	for _, q := range []string{
		`UPDATE files SET rel_path = ? WHERE rel_path = ?`,
		`UPDATE OR REPLACE favorites SET rel_path = ? WHERE rel_path = ?`,
		`UPDATE OR REPLACE scanned SET rel_path = ? WHERE rel_path = ?`,
		`UPDATE OR REPLACE conversions SET src_rel = ? WHERE src_rel = ?`,
		`UPDATE album_copies SET src_rel = ? WHERE src_rel = ?`,
	} {
		if _, err := s.db.Exec(q, newRel, rel); err != nil {
			return "", err
		}
	}
	if newOut != "" {
		if _, err := s.db.Exec(`UPDATE conversions SET out_rel = ? WHERE src_rel = ?`, newOut, newRel); err != nil {
			return "", err
		}
		s.db.Exec(`UPDATE OR REPLACE scanned SET rel_path = ? WHERE rel_path = ?`, newOut, outRel)
	}
	return newRel, nil
}

// linkOrCopy makes dst the same file as src: a hard link where the disk supports it
// (no extra space), otherwise a copy.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if st, err := os.Stat(src); err == nil {
		os.Chtimes(tmp, st.ModTime(), st.ModTime())
	}
	return os.Rename(tmp, dst)
}

// removeCopy deletes an album copy, its compressed version and its records.
func (s *Store) removeCopy(rel string) error {
	var outRel string
	s.db.QueryRow(`SELECT out_rel FROM conversions WHERE src_rel = ?`, rel).Scan(&outRel)
	for _, p := range []string{rel, outRel} {
		if p == "" {
			continue
		}
		if err := os.Remove(filepath.Join(s.root, filepath.FromSlash(p))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.db.Exec(`DELETE FROM scanned WHERE rel_path = ?`, p)
	}
	for _, q := range []string{`DELETE FROM conversions WHERE src_rel = ?`, `DELETE FROM favorites WHERE rel_path = ?`,
		`DELETE FROM album_copies WHERE rel_path = ?`} {
		if _, err := s.db.Exec(q, rel); err != nil {
			return err
		}
	}
	return nil
}

type albumCopy struct {
	rel, album string
	size       int64
}

func (s *Store) copiesOf(src string) ([]albumCopy, error) {
	rows, err := s.db.Query(`SELECT rel_path, album, size FROM album_copies WHERE src_rel = ?`, src)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []albumCopy
	for rows.Next() {
		var c albumCopy
		if err := rows.Scan(&c.rel, &c.album, &c.size); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// syncAlbums puts an asset's files (paths) where its albums say: in base/<first album>
// (or base when it's in none), with a copy in base/<each other album>. Copies for albums
// the photo was taken out of are removed. It reports how many files moved or were copied.
func (s *Store) syncAlbums(paths []string, base string, albums []string) (moved, copied int, err error) {
	dirs := albumDirs(albums)
	primary := base
	if len(dirs) > 0 {
		primary = path.Join(base, dirs[0])
	}
	inDir := func(rel, dir string) bool { return path.Dir(rel) == dir || (path.Dir(rel) == "." && dir == "") }
	for _, rel := range paths {
		// A photo in no album stays where it is (Image Capture subfolders, or the album
		// folder it was in before).
		if len(dirs) > 0 && !inDir(rel, primary) {
			// A copy already in the new folder is replaced by the file itself.
			if copies, err := s.copiesOf(rel); err == nil {
				for _, c := range copies {
					if c.album == dirs[0] {
						s.removeCopy(c.rel)
					}
				}
			}
			if rel, err = s.relocate(rel, primary); err != nil {
				return moved, copied, err
			}
			moved++
		}
		existing, err := s.copiesOf(rel)
		if err != nil {
			return moved, copied, err
		}
		want := map[string]bool{}
		for _, album := range dirs[min(1, len(dirs)):] {
			want[album] = true
			have := false
			for _, c := range existing {
				if c.album == album && s.present(c.rel, c.size) {
					have = true
				}
			}
			if have {
				continue
			}
			// Copy whatever is on disk: the original, or its compressed copy if the
			// original was removed.
			src := rel
			if _, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel))); err != nil {
				out, ok := s.converted(rel)
				if !ok {
					continue
				}
				src = out
			}
			st, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(src)))
			if err != nil {
				continue
			}
			dir := path.Join(base, album)
			if err := os.MkdirAll(filepath.Join(s.root, filepath.FromSlash(dir)), 0o755); err != nil {
				return moved, copied, err
			}
			dst := s.freeName(dir, path.Base(src))
			if err := linkOrCopy(filepath.Join(s.root, filepath.FromSlash(src)), filepath.Join(s.root, filepath.FromSlash(dst))); err != nil {
				return moved, copied, err
			}
			if _, err := s.db.Exec(`INSERT OR REPLACE INTO album_copies (rel_path, src_rel, album, size) VALUES (?, ?, ?, ?)`,
				dst, rel, album, st.Size()); err != nil {
				return moved, copied, err
			}
			if s.isFavorite(rel) {
				_ = setFavoriteTag(filepath.Join(s.root, filepath.FromSlash(dst)), true)
			}
			copied++
		}
		for _, c := range existing {
			if !want[c.album] {
				if err := s.removeCopy(c.rel); err != nil {
					return moved, copied, err
				}
			}
		}
	}
	return moved, copied, nil
}

// syncAlbums applies an asset's albums to its files if album folders are on.
func (s *Server) syncAlbums(d *Destination, deviceID, deviceName string, paths, albums []string) {
	if !s.albumFolders.Load() || albums == nil || len(paths) == 0 {
		return
	}
	base := ""
	if s.deviceFolders.Load() {
		var err error
		if base, err = d.store.DeviceFolder(deviceID, deviceName); err != nil {
			return
		}
	}
	moved, copied, err := d.store.syncAlbums(paths, base, albums)
	switch {
	case err != nil:
		s.events.add("Album folders: %v", err)
	case moved > 0 || copied > 0:
		where := "no album"
		if dirs := albumDirs(albums); len(dirs) > 0 {
			where = strings.Join(dirs, ", ")
		}
		s.events.add("Album folders (%s): moved %d, copied %d", where, moved, copied)
	}
}
