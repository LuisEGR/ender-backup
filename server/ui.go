package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"rsc.io/qr"
)

//go:embed ui.html
var uiHTML []byte

// uiBuild identifies this page's contents: an open page reloads itself when the server it
// talks to (restarted with a new build) serves a different one.
var uiBuild = func() string { h := sha256.Sum256(uiHTML); return hex.EncodeToString(h[:6]) }()

// localOnly restricts the settings page to this computer: the connection must come
// from loopback, the Host header must name loopback (blocks DNS rebinding), and
// state-changing requests must be same-origin JSON (blocks other websites).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !isLoopbackHost(host) || !isLoopbackHost(hostOnly(r.Host)) {
			http.Error(w, "The settings page is only available on the computer running photo-backup.", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !isLoopbackHost(u.Hostname()) {
					writeError(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) uiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // the desktop window must get a new build's page
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /ui/state", s.handleUIState)
	mux.HandleFunc("GET /ui/qr.png", s.handleQR)
	mux.HandleFunc("POST /ui/settings", s.handleUISettings)
	mux.HandleFunc("POST /ui/new-token", s.handleNewToken)
	mux.HandleFunc("POST /ui/pick-folder", s.handlePickFolder)
	mux.HandleFunc("POST /ui/open-folder", s.handleOpenFolder)
	mux.HandleFunc("POST /ui/speedtest", func(w http.ResponseWriter, r *http.Request) {
		d, release := s.use()
		d.ProbeDrive(&s.events)
		release()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /ui/rescan", func(w http.ResponseWriter, r *http.Request) {
		d, release := s.use()
		d.StartIndexing(&s.events)
		release()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /ui/restore-originals", s.handleRestoreOriginals)
	mux.HandleFunc("POST /ui/language", s.handleLanguage)
	mux.HandleFunc("POST /ui/pair/approve", s.handleDecidePair(true))
	mux.HandleFunc("POST /ui/pair/deny", s.handleDecidePair(false))
	return mux
}

func (s *Server) pairURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return pairingURL(s.hosts, s.port, s.token, s.name, s.id)
}

func (s *Server) handleUIState(w http.ResponseWriter, r *http.Request) {
	d, release := s.use()
	stats, err := d.store.Stats()
	root := d.root
	indexing, indexed := d.Indexing()
	favorites, _ := d.store.FavoriteCount()
	conversion, _ := d.store.conversionStats(s.compressSettings().kinds())
	conversion.Current = d.converting()
	conversion.Progress = d.compressionProgress(s.compressSettings().kinds())
	drive := d.Drive()
	folderBytes, _ := d.FolderBytes()
	library := s.library.get(d)
	wanted, wantedBytes := d.store.originalsWanted()
	recent, _ := d.store.RecentFiles(8)
	release()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type device struct {
		Name         string `json:"name"`
		Folder       string `json:"folder"`
		Assets       int64  `json:"assets"`
		Files        int64  `json:"files"`
		Size         string `json:"size"`
		Bytes        int64  `json:"bytes"`
		Deleted      int64  `json:"deleted"`
		LastSeen     string `json:"last_seen"`
		LastSeenUnix int64  `json:"last_seen_unix"`
		LastBackup   int64  `json:"last_backup,omitempty"`
		deviceStatus
	}
	devices := []device{}
	now := time.Now()
	for _, st := range stats {
		devices = append(devices, device{st.Name, st.Folder, st.Assets, st.Files, humanBytes(st.Bytes), st.Bytes,
			st.Deleted, st.LastSeen.Format("2006-01-02 15:04"), st.LastSeen.Unix(), st.LastBackup,
			s.activity.status(st.ID, now)})
	}
	var diskTotal, diskFree uint64
	if t, f, err := diskUsage(root); err == nil {
		diskTotal, diskFree = t, f
	}
	s.mu.RLock()
	name, port, hosts := s.name, s.port, s.hosts
	s.mu.RUnlock()
	writeJSON(w, map[string]any{
		"name": name, "dest": root, "port": port, "hosts": hosts, "device_folders": s.deviceFolders.Load(),
		"album_folders": s.albumFolders.Load(), "folder_bytes": folderBytes, "disk_total": diskTotal, "disk_free": diskFree,
		"library": library, "originals_wanted": wanted, "originals_wanted_bytes": wantedBytes,
		"version": serverVersion, "os": runtime.GOOS, "ui": uiBuild, "recent": recent,
		"language": s.language(), "language_setting": s.languageSetting(), "languages": languages,
		"system_language": systemLanguage(),
		"warning":         s.destWarningText(),
		"indexing":        indexing, "indexed": indexed, "favorites": favorites, "drive": drive,
		"compress": s.compressSettings(), "conversion": conversion, "video_reason": findVideoTools().reason,
		"photo_levels": photoLevels, "video_levels": videoLevels,
		"pair_url": s.pairURL(), "config_path": s.cfgPath,
		"devices": devices, "events": s.events.recent(), "pair_requests": s.pairs.pending(),
	})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	code, err := qr.Encode(s.pairURL(), qr.M)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// In the page's colours: dark on cream by day, and after dark too (a light-on-dark code
	// would match better but many scanners, the iPhone's included, read it unreliably).
	fg, bg := color.RGBA{0x36, 0x2A, 0x23, 0xFF}, color.RGBA{0xFF, 0xFC, 0xF4, 0xFF}
	if r.URL.Query().Get("theme") == "dark" {
		fg, bg = color.RGBA{0x17, 0x1F, 0x20, 0xFF}, color.RGBA{0xE8, 0xE0, 0xD0, 0xFF}
	}
	const scale, quiet = 8, 4 // quiet: the blank margin scanners need, in modules
	side := (code.Size + 2*quiet) * scale
	img := image.NewPaletted(image.Rect(0, 0, side, side), color.Palette{bg, fg})
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.SetColorIndex((x+quiet)*scale+dx, (y+quiet)*scale+dy, 1)
					}
				}
			}
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	png.Encode(w, img)
}

// updateConfig applies fn to the saved config file.
func (s *Server) updateConfig(fn func(*Config)) error {
	if s.cfgPath == "" {
		return nil
	}
	c, err := LoadConfig(s.cfgPath)
	if err != nil {
		return err
	}
	fn(c)
	return c.Save(s.cfgPath)
}

func (s *Server) handleUISettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dest          string `json:"dest"`
		Name          string `json:"name"`
		DeviceFolders *bool  `json:"device_folders"`
		AlbumFolders  *bool  `json:"album_folders"`
		compressUpdate
	}
	if !readJSON(w, r, &req) {
		return
	}
	dest, err := expandHome(strings.TrimSpace(req.Dest))
	if err != nil || dest == "" {
		writeError(w, http.StatusBadRequest, "choose a destination folder")
		return
	}
	if dest, err = filepath.Abs(dest); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, release := s.use()
	current := d.root
	release()
	preferred, _ := s.fallbackState()
	if !samePath(dest, current) {
		if err := s.SetDestination(dest); err != nil {
			writeError(w, http.StatusBadRequest, "can't use that folder: "+err.Error())
			return
		}
		s.clearFallback() // a folder was chosen: stop waiting for the missing drive
		preferred = ""
	}
	if preferred != "" {
		dest = preferred // keep the drive's folder in the settings file while it's away
	}
	name := strings.TrimSpace(req.Name)
	if name != "" {
		s.mu.Lock()
		s.name = name
		s.mu.Unlock()
		s.refreshBonjour()
	}
	if req.DeviceFolders != nil {
		s.deviceFolders.Store(*req.DeviceFolders)
	}
	if req.AlbumFolders != nil {
		s.albumFolders.Store(*req.AlbumFolders)
	}
	compress := s.applyCompression(req.compressUpdate)
	if err := s.updateConfig(func(c *Config) {
		c.Dest = dest
		if name != "" {
			c.Name = name
		}
		if req.DeviceFolders != nil {
			c.DeviceFolders = *req.DeviceFolders
		}
		if req.AlbumFolders != nil {
			c.AlbumFolders = req.AlbumFolders
		}
		c.Compress, c.Format, c.PhotoLevel, c.RemoveOriginals = compress.Enabled, compress.Format, compress.PhotoLevel, compress.RemoveOriginals
		c.CompressVideos, c.VideoLevel = compress.Videos, compress.VideoLevel
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "saved for this session, but writing the config failed: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleNewToken(w http.ResponseWriter, r *http.Request) {
	token := randomToken()
	s.mu.Lock()
	s.token = token
	s.mu.Unlock()
	if err := s.updateConfig(func(c *Config) { c.Token = token }); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.events.add("New pairing code — phones must scan the QR again")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	d, release := s.use()
	start := d.root
	release()
	path, err := pickFolder(start, s.language())
	if errors.Is(err, errCancelled) {
		writeJSON(w, map[string]any{"cancelled": true})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": path})
}

func (s *Server) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	d, release := s.use()
	root := d.root
	release()
	if err := openInFileManager(root); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDecidePair(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if !s.decidePair(req.ID, approve) {
			writeError(w, http.StatusNotFound, "that request has expired")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, p[1:]), nil
	}
	return p, nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
