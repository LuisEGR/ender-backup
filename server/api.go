package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const apiVersion = 1

// serverVersion is shown in the app's About section.
const serverVersion = "0.20.0"

// Destination is one backup folder and its index.
type Destination struct {
	store  *Store
	root   string // destination folder
	tmpDir string // same volume as root so finished uploads can be renamed into place
	idx    indexer
	drive  driveState
	conv   converter
	size   folderSize
	state  folderState
}

// OpenDestination opens (creating if needed) a backup folder and its index at
// <root>/.photo-backup/index.db.
func OpenDestination(root string) (*Destination, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if mountMissing(root) {
		return nil, errDriveMissing
	}
	tmp := filepath.Join(root, ".photo-backup", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(root, ".photo-backup", "index.db"), root)
	if err != nil {
		return nil, err
	}
	return &Destination{store: store, root: root, tmpDir: tmp}, nil
}

func (d *Destination) Close() error {
	d.stopWatching()
	d.stopConverter()
	d.stopIndexing()
	return d.store.Close()
}

// Server exposes the HTTP API the iPhone app talks to, plus the local settings page.
type Server struct {
	mu    sync.RWMutex // guards the fields below; requests hold RLock, so SetDestination waits for uploads in flight
	dest  *Destination
	token string
	name  string
	id    string // stable server ID, advertised over Bonjour so phones can find us again

	// deviceFolders puts each phone's files in its own subfolder instead of the destination root.
	deviceFolders atomic.Bool
	// preferredDest is the chosen folder while its drive is missing and the default folder
	// is used instead (see fallback.go); destWarning explains that to the user.
	preferredDest, destWarning string
	// albumFolders: save photos in a folder per album (see albums.go).
	albumFolders atomic.Bool
	// lang: the language chosen in the settings, "" for the computer's (guarded by mu; i18n.go).
	lang string
	// peers: other photo-backup servers found on the Tailscale network.
	peers peerList
	// compress: convert photos to AVIF in the background (guarded by mu).
	compress CompressSettings
	// nativeDialogs asks to approve pairing requests with a desktop dialog (off in tests).
	nativeDialogs bool

	placeMu sync.Mutex // serializes choosing a free filename + renaming into place
	events  eventLog
	pairs   pairRequests
	// activity: what each phone is doing now (activity.go).
	activity activityLog
	library  libraryCache // the folder's photo/video summary for the settings page

	bonjourMu sync.Mutex
	bonjour   advertiser

	// Used by the settings page (ui.go).
	cfgPath string
	port    int
	hosts   []string
}

func NewServer(dest *Destination, token, name string) *Server {
	return &Server{dest: dest, token: token, name: name}
}

// use returns the current destination, pinned until release is called.
func (s *Server) use() (d *Destination, release func()) {
	s.mu.RLock()
	return s.dest, s.mu.RUnlock
}

// SetDestination switches to another backup folder once in-flight requests finish.
func (s *Server) SetDestination(root string) error {
	d, err := OpenDestination(root)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.dest
	s.dest = d
	s.mu.Unlock()
	s.events.add("Saving to %s", d.root)
	d.StartIndexing(&s.events)
	d.ProbeDrive(&s.events)
	d.StartConverter(s.compressSettings, &s.events)
	d.StartWatching(30 * time.Second)
	return old.Close()
}

func (s *Server) Close() error {
	s.mu.Lock() // waits for requests in flight
	d := s.dest
	s.mu.Unlock()
	// Outside the lock: stopping the converter waits for it, and it reads the settings.
	return d.Close()
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/info", s.handleInfo)
	api.HandleFunc("POST /api/check", s.handleCheck)
	api.HandleFunc("POST /api/upload", s.handleUpload)
	api.HandleFunc("POST /api/commit", s.handleCommit)
	api.HandleFunc("POST /api/deleted", s.handleDeleted)
	api.HandleFunc("POST /api/speedtest", handleSpeedTest)
	api.HandleFunc("POST /api/compression", s.handleCompression)

	mux := http.NewServeMux()
	mux.Handle("/api/", s.auth(api))
	mux.Handle("/pair/", s.pairHandler())
	mux.Handle("/", localOnly(s.uiHandler()))
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		token := s.token
		s.mu.RUnlock()
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token — scan the QR code again")
			return
		}
		s.activity.request(r, time.Now())
		if dev := r.Header.Get("X-Device-Id"); dev != "" {
			d, release := s.use()
			err := d.store.TouchDevice(dev, r.Header.Get("X-Device-Name"))
			release()
			if err != nil {
				log.Printf("touch device: %v", err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	name := s.name
	s.mu.RUnlock()

	// Where this phone's files go, so the app can show it.
	d, release := s.use()
	defer release()
	folder := d.root
	if dev := r.Header.Get("X-Device-Id"); dev != "" && s.deviceFolders.Load() {
		sub, err := d.store.DeviceFolder(dev, r.Header.Get("X-Device-Name"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		folder = filepath.Join(d.root, sub)
	}
	indexing, indexed := d.Indexing()
	resp := map[string]any{"name": name, "id": s.id, "version": apiVersion, "folder": folder,
		"os": runtime.GOOS, "server_version": serverVersion,
		"indexing": indexing, "indexed": indexed, "drive": d.Drive(),
		"compression": s.compressionInfo(d),
		"state":       d.StateToken()} // changes when files disappear: phones recheck
	if total, free, err := diskUsage(d.root); err == nil {
		resp["disk_total"], resp["disk_free"] = total, free
	}
	if _, warning := s.fallbackState(); warning != "" {
		resp["folder_warning"] = warning
	}
	if n, ok := d.FolderBytes(); ok {
		resp["folder_bytes"] = n // the backup folder's share of the used space
	}
	writeJSON(w, resp)
}

type checkRequest struct {
	DeviceID string   `json:"device_id"`
	AssetIDs []string `json:"asset_ids"` // matched by this device's asset IDs only
	// Assets are matched by ID and, failing that, by content (name + size + capture
	// time), so photos already sent by another phone or a previous install count too.
	Assets []checkAsset `json:"assets"`
}

type checkAsset struct {
	ID        string                `json:"id"`
	CreatedAt int64                 `json:"created_at"`
	Resources []ResourceFingerprint `json:"resources"`
	Favorite  *bool                 `json:"favorite"` // nil from app versions that don't send it
	Albums    []string              `json:"albums"`   // nil from app versions that don't send it
}

// handleCheck answers "which of these assets are safely backed up?".
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id required")
		return
	}
	d, release := s.use()
	defer release()
	ids, err := d.store.BackedUp(req.DeviceID, req.AssetIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, a := range req.Assets {
		paths, err := d.store.assetFiles(req.DeviceID, a.ID)
		if err == nil && len(paths) == 0 {
			paths, err = d.store.ContentMatch(a.Resources, a.CreatedAt)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(paths) > 0 {
			ids = append(ids, a.ID)
			if a.Favorite != nil {
				s.syncFavorite(d, paths, *a.Favorite)
			}
			s.syncAlbums(d, req.DeviceID, r.Header.Get("X-Device-Name"), paths, a.Albums)
		}
	}
	indexing, _ := d.Indexing()
	writeJSON(w, map[string]any{"backed_up": ids, "indexing": indexing, "state": d.StateToken()})
}

// handleUpload receives one resource of an asset as the raw request body.
// Metadata travels in the query string (filenames may be non-ASCII).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	device, asset, resource := q.Get("device_id"), q.Get("asset_id"), q.Get("resource")
	filename := sanitizeFilename(q.Get("filename"))
	if device == "" || asset == "" || resource == "" || filename == "" {
		writeError(w, http.StatusBadRequest, "device_id, asset_id, resource and filename are required")
		return
	}
	created, _ := strconv.ParseInt(q.Get("created"), 10, 64)
	expectedSize := int64(-1)
	if v := q.Get("size"); v != "" {
		expectedSize, _ = strconv.ParseInt(v, 10, 64)
	}

	d, release := s.use()
	defer release()
	tmp, err := os.CreateTemp(d.tmpDir, "upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed into place

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), throttled(r.Body))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "receive failed: "+err.Error())
		return
	}
	if expectedSize >= 0 && n != expectedSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size mismatch: got %d bytes, expected %d", n, expectedSize))
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want := strings.ToLower(q.Get("sha256")); want != "" && want != sum {
		writeError(w, http.StatusBadRequest, "sha256 mismatch")
		return
	}

	rel, dup, err := d.store.FindByContent(sum, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !dup {
		// Same photo already in the folder from before (Image Capture, another tool)?
		rel, dup, err = d.store.FindScannedDuplicate(filename, n, sum)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if !dup {
		folder := ""
		if s.deviceFolders.Load() {
			var ferr error
			if folder, ferr = d.store.DeviceFolder(device, r.Header.Get("X-Device-Name")); ferr != nil {
				writeError(w, http.StatusInternalServerError, ferr.Error())
				return
			}
		}
		if albums := q["album"]; s.albumFolders.Load() {
			folder = albumFolder(folder, albums)
		}
		rel, err = s.place(d, tmpPath, folder, filename, created)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := d.store.PutFile(FileRecord{
		DeviceID: device, AssetID: asset, Resource: resource, OriginalName: filename,
		RelPath: rel, Size: n, SHA256: sum, CreatedAt: created,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	note := ""
	if dup {
		note = " (duplicate content, reusing existing file)"
	}
	if err := d.store.originalBack(rel); err != nil {
		log.Printf("original back: %v", err)
	}
	s.events.add("Received %s  %s%s", humanBytes(n), rel, note)
	s.activity.upload(device, filename, n, time.Now())
	d.WakeConverter()
	writeJSON(w, map[string]any{"rel_path": rel, "size": n, "sha256": sum})
}

// place moves a finished upload to <root>/[<device folder>/]<filename>, picking a
// free name if one is taken, and sets the file's mtime to the capture date.
func (s *Server) place(d *Destination, tmpPath, folder, filename string, created int64) (string, error) {
	t := time.Unix(created, 0)
	dirRel := folder
	if err := os.MkdirAll(filepath.Join(d.root, dirRel), 0o755); err != nil {
		return "", err
	}

	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 0; ; i++ {
		name := filename
		if i > 0 {
			name = fmt.Sprintf("%s_%d%s", base, i, ext)
		}
		rel := filepath.Join(dirRel, name)
		dst := filepath.Join(d.root, rel)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			return "", err
		}
		// Temp files are created 0600; make backups readable like any other photo
		// (shared folders, media servers). No-op on Windows.
		_ = os.Chmod(dst, 0o644)
		if created > 0 {
			_ = os.Chtimes(dst, t, t)
		}
		return filepath.ToSlash(rel), nil
	}
}

type commitRequest struct {
	DeviceID  string   `json:"device_id"`
	AssetID   string   `json:"asset_id"`
	MediaType string   `json:"media_type"`
	CreatedAt int64    `json:"created_at"`
	Resources []string `json:"resources"`
	Albums    []string `json:"albums"` // nil from app versions that don't send it
}

// handleCommit is sent after all resources of an asset are uploaded; only
// committed assets are ever reported as safe to delete.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req commitRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.DeviceID == "" || req.AssetID == "" {
		writeError(w, http.StatusBadRequest, "device_id and asset_id required")
		return
	}
	d, release := s.use()
	defer release()
	missing, err := d.store.CommitAsset(req.DeviceID, req.AssetID, req.MediaType, req.CreatedAt, req.Resources)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(missing) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "missing resources", "missing": missing})
		return
	}
	if paths, err := d.store.assetFiles(req.DeviceID, req.AssetID); err == nil {
		s.syncAlbums(d, req.DeviceID, r.Header.Get("X-Device-Name"), paths, req.Albums) // copies into the other albums
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleted(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	if !readJSON(w, r, &req) {
		return
	}
	d, release := s.use()
	defer release()
	if err := d.store.MarkDeleted(req.DeviceID, req.AssetIDs); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.events.add("Phone deleted %d backed-up items", len(req.AssetIDs))
	writeJSON(w, map[string]any{"ok": true})
}

// handleSpeedTest receives and discards data so the phone can time the network
// between them, separately from the disk.
func handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, 1<<30))
	if err != nil {
		writeError(w, http.StatusBadRequest, "receive failed: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"bytes": n, "seconds": time.Since(start).Seconds()})
}

// eventLog keeps recent activity for the settings page and mirrors it to the console.
type eventLog struct {
	mu    sync.Mutex
	seq   int
	items []event
}

type event struct {
	Seq  int    `json:"seq"`
	Time string `json:"time"`
	Text string `json:"text"`
}

func (l *eventLog) add(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	log.Print(text)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.items = append(l.items, event{Seq: l.seq, Time: time.Now().Format("15:04:05"), Text: text})
	if len(l.items) > 100 {
		l.items = l.items[len(l.items)-100:]
	}
}

func (l *eventLog) recent() []event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]event, len(l.items))
	copy(out, l.items)
	return out
}

func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep "&" in pairing URLs readable for scripts
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// humanBytes formats sizes (and, with "/s", speeds) in decimal units — MB, GB — the
// same way the iPhone app and Finder do, so numbers match across them.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
