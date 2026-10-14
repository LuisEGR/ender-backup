package main

import (
	"crypto/sha256"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Duplicate originals: importing the same photo or video twice (Image Capture, Finder)
// leaves "KGJS9586.MP4" and "KGJS9586 1.MP4" side by side, byte for byte the same. Each
// was compressed on its own, making two identical copies. Now a duplicate shares the copy
// of the original compressed first, and dedupeCopies removes the extra copies made before.

var copySuffix = regexp.MustCompile(`( \d+| \(\d+\))$`)

// dupKey identifies an original up to the " 1" / " (1)" a second import adds: the folder,
// the name without that suffix, the extension, and the size.
func dupKey(rel string, size int64) string {
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	stem := copySuffix.ReplaceAllString(strings.TrimSuffix(base, ext), "")
	return strings.ToLower(dir+stem+ext) + "|" + strconv.FormatInt(size, 10)
}

// sameContent compares two files byte for byte (through SHA-256).
func sameContent(a, b string) bool {
	ha, err1 := fileHash(a)
	hb, err2 := fileHash(b)
	return err1 == nil && err2 == nil && ha == hb
}

func fileHash(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return string(h.Sum(nil)), nil
}

// existingCopy finds a compressed copy already made from a duplicate of rel: an original
// with the same dupKey whose copy is on disk. When both originals are still on disk they
// must also be identical.
func (s *Store) existingCopy(rel string, size int64) (srcRel, outRel string, outSize int64, quality int, ok bool) {
	key := dupKey(rel, size)
	rows, err := s.db.Query(`SELECT src_rel, out_rel, out_size, quality FROM conversions
		WHERE out_rel <> '' AND src_size = ? AND src_rel <> ? ORDER BY converted_at`, size, rel)
	if err != nil {
		return
	}
	type cand struct {
		src, out string
		size     int64
		q        int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.src, &c.out, &c.size, &c.q) == nil && dupKey(c.src, size) == key {
			cands = append(cands, c)
		}
	}
	rows.Close()
	for _, c := range cands {
		if !s.fileOnDisk(c.out, c.size) {
			continue
		}
		if s.fileOnDisk(c.src, size) && !sameContent(s.abs(c.src), s.abs(rel)) {
			continue // same name and size, different content
		}
		return c.src, c.out, c.size, c.q, true
	}
	return
}

func (s *Store) abs(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

// dedupeCopies removes copies made from duplicate originals before existingCopy was used:
// within each group of originals with the same dupKey, a later copy that is byte for byte
// the same as the earliest one is deleted and its conversion points at the earliest copy.
// Runs once per folder (meta "dedupe_copies_v1"); stops early when stop is closed and
// resumes next time.
func (d *Destination) dedupeCopies(events *eventLog, stop <-chan struct{}) {
	s := d.store
	var done string
	if s.db.QueryRow(`SELECT value FROM meta WHERE key = 'dedupe_copies_v1'`).Scan(&done) == nil {
		return
	}
	type conv struct {
		src, out string
		size     int64
		at       int64
	}
	rows, err := s.db.Query(`SELECT src_rel, src_size, out_rel, out_size, converted_at FROM conversions WHERE out_rel <> ''`)
	if err != nil {
		return
	}
	groups := map[string][]conv{}
	for rows.Next() {
		var c conv
		var srcSize int64
		if rows.Scan(&c.src, &srcSize, &c.out, &c.size, &c.at) == nil {
			k := dupKey(c.src, srcSize)
			groups[k] = append(groups[k], c)
		}
	}
	rows.Close()
	start := time.Now()
	var removed int
	var freed int64
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		select {
		case <-stop:
			return // not finished: runs again next time
		default:
		}
		sort.Slice(g, func(i, j int) bool { return g[i].at < g[j].at || (g[i].at == g[j].at && g[i].src < g[j].src) })
		keep := g[0]
		if !s.fileOnDisk(keep.out, keep.size) {
			continue
		}
		for _, x := range g[1:] {
			if strings.EqualFold(x.out, keep.out) || !s.fileOnDisk(x.out, x.size) || x.size != keep.size ||
				!sameContent(s.abs(keep.out), s.abs(x.out)) {
				continue
			}
			if err := os.Remove(s.abs(x.out)); err != nil {
				continue
			}
			s.db.Exec(`UPDATE conversions SET out_rel = ?, out_size = ? WHERE src_rel = ?`, keep.out, keep.size, x.src)
			s.db.Exec(`DELETE FROM scanned WHERE rel_path = ?`, x.out)
			removed++
			freed += x.size
		}
	}
	s.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('dedupe_copies_v1', '1')`)
	if removed > 0 {
		events.add("Removed %d duplicate compressed copies (%s) made from duplicate originals, in %s",
			removed, humanBytes(freed), time.Since(start).Round(time.Second))
	}
}
