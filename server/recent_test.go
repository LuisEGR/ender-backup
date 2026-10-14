package main

import (
	"path/filepath"
	"testing"
)

func TestRecentFiles(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "index.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.TouchDevice("phone", "Ana's iPhone"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO files (device_id, asset_id, resource, original_name, rel_path, size, sha256, uploaded_at)
		VALUES ('phone', 'a', 'photo', 'IMG_1.HEIC', 'IMG_1.HEIC', 10, 'x', 100),
		       ('phone', 'b', 'video', 'IMG_2.MOV', 'IMG_2.MOV', 20, 'y', 300),
		       ('gone', 'c', 'photo', 'IMG_3.HEIC', 'IMG_3.HEIC', 30, 'z', 200)`); err != nil {
		t.Fatal(err)
	}
	got, err := st.RecentFiles(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "IMG_2.MOV" || got[0].Device != "Ana's iPhone" || got[1].Name != "IMG_3.HEIC" || got[1].Device != "" {
		t.Fatalf("RecentFiles(2) = %+v", got)
	}
}
