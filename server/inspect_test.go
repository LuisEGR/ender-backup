package main

import (
	"os"
	"testing"
)

// With PHOTO_BACKUP_INSPECT=<folder>: which recorded files are missing from it, and which of
// those still have a compressed copy (and so still count as backed up).
func TestInspectFolder(t *testing.T) {
	root := os.Getenv("PHOTO_BACKUP_INSPECT")
	if root == "" {
		t.Skip("set PHOTO_BACKUP_INSPECT to run")
	}
	d, err := OpenDestination(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	type rec struct {
		rel  string
		size int64
	}
	var recs []rec
	rows, _ := d.store.db.Query(`SELECT rel_path, size FROM files`)
	for rows.Next() {
		var r rec
		rows.Scan(&r.rel, &r.size)
		recs = append(recs, r)
	}
	rows.Close()
	gone, compressedOnly := 0, 0
	for _, r := range recs {
		if d.store.fileOnDisk(r.rel, r.size) {
			continue
		}
		if _, ok := d.store.converted(r.rel); ok {
			compressedOnly++
		} else {
			gone++
			if gone <= 5 {
				t.Logf("gone: %s", r.rel)
			}
		}
	}
	t.Logf("%d uploaded files recorded; original missing but compressed copy there: %d; missing entirely: %d",
		len(recs), compressedOnly, gone)
	var scanned, scannedGone, avifs, avifGone int
	rows, _ = d.store.db.Query(`SELECT rel_path, size FROM scanned`)
	var srecs []rec
	for rows.Next() {
		var r rec
		rows.Scan(&r.rel, &r.size)
		srecs = append(srecs, r)
	}
	rows.Close()
	for _, r := range srecs {
		scanned++
		if !d.store.fileOnDisk(r.rel, r.size) {
			scannedGone++
			if scannedGone <= 5 {
				t.Logf("indexed file gone: %s", r.rel)
			}
		}
	}
	rows, _ = d.store.db.Query(`SELECT out_rel, out_size FROM conversions WHERE out_rel <> ''`)
	var crecs []rec
	for rows.Next() {
		var r rec
		rows.Scan(&r.rel, &r.size)
		crecs = append(crecs, r)
	}
	rows.Close()
	for _, r := range crecs {
		avifs++
		if !d.store.fileOnDisk(r.rel, r.size) {
			avifGone++
			if avifGone <= 5 {
				t.Logf("compressed copy gone: %s", r.rel)
			}
		}
	}
	t.Logf("indexed files: %d, gone: %d; compressed copies: %d, gone: %d", scanned, scannedGone, avifs, avifGone)
}
