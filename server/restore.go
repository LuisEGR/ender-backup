package main

import "net/http"

// Getting originals back: after "Delete originals" removed them, a compressed copy still
// counts as the backup, so phones never send those items again. Asking for the originals
// back flags those conversions: their copies stop counting (see present), the phones see
// the items as not backed up and upload them, and when an original lands again the flag
// is cleared (originalBack). The compressed copies stay.

// WantOriginalsBack flags conversions whose original was deleted: format "avif" (photos),
// "av1" (videos) or "" (both), most recently converted first, until maxBytes of originals
// (0 = no limit). It returns how many originals the phones should bring back, and their size.
func (s *Store) WantOriginalsBack(format string, maxBytes int64) (n, bytes int64, err error) {
	rows, err := s.db.Query(`SELECT src_rel, src_size FROM conversions
		WHERE src_removed = 1 AND out_rel <> '' AND restore = 0 AND (? = '' OR format = ?)
		ORDER BY converted_at DESC`, format, format)
	if err != nil {
		return 0, 0, err
	}
	var rels []string
	for rows.Next() {
		var rel string
		var size int64
		if err := rows.Scan(&rel, &size); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if maxBytes > 0 && bytes+size > maxBytes {
			continue // too big for what's left; a smaller one may still fit
		}
		rels = append(rels, rel)
		bytes += size
	}
	rows.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	for _, rel := range rels {
		if _, err := tx.Exec(`UPDATE conversions SET restore = 1 WHERE src_rel = ?`, rel); err != nil {
			tx.Rollback()
			return 0, 0, err
		}
	}
	return int64(len(rels)), bytes, tx.Commit()
}

// originalBack records that the original at rel is on disk again.
func (s *Store) originalBack(rel string) error {
	_, err := s.db.Exec(`UPDATE conversions SET src_removed = 0, restore = 0 WHERE src_rel = ? AND restore = 1`, rel)
	return err
}

// originalsWanted counts originals asked back that haven't arrived yet.
func (s *Store) originalsWanted() (n, bytes int64) {
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(src_size), 0) FROM conversions WHERE restore = 1`).Scan(&n, &bytes)
	return n, bytes
}

func (s *Server) handleRestoreOriginals(w http.ResponseWriter, r *http.Request) {
	if s.compressSettings().RemoveOriginals {
		writeError(w, http.StatusConflict, "Turn off “Delete originals after compressing” first, or they'd be deleted again.")
		return
	}
	var req struct {
		Format   string `json:"format"`    // "avif", "av1" or "" for both
		MaxBytes int64  `json:"max_bytes"` // 0 = all
	}
	if r.ContentLength > 0 && !readJSON(w, r, &req) {
		return
	}
	d, release := s.use()
	defer release()
	n, bytes, err := d.store.WantOriginalsBack(req.Format, req.MaxBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.events.add("Asked for %d deleted originals back (%s): phones will send them again", n, humanBytes(bytes))
	writeJSON(w, map[string]any{"ok": true, "count": n, "bytes": bytes})
}
