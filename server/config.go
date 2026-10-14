package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the settings that survive restarts. It lives in the user config
// dir (or $PHOTO_BACKUP_CONFIG), separate from the destination folder, so
// changing the folder doesn't change the pairing token.
type Config struct {
	ID    string `json:"id"` // stable server identity, advertised over Bonjour
	Dest  string `json:"dest"`
	Port  int    `json:"port"`
	Name  string `json:"name"`
	Token string `json:"token"`
	// DeviceFolders stores each phone's files in <dest>/<device name>/ (default: all in <dest>).
	DeviceFolders bool `json:"device_folders"`
	// AlbumFolders saves photos in a folder per album on the phone (nil: on).
	AlbumFolders *bool `json:"album_folders,omitempty"`
	// Compression (see compress.go): off by default; AVIF, quality per kind of photo; originals kept.
	Compress        bool   `json:"compress"`
	Format          string `json:"format"`
	PhotoLevel      string `json:"photo_level"` // see levels.go
	CompressVideos  bool   `json:"compress_videos"`
	VideoLevel      string `json:"video_level"`
	RemoveOriginals bool   `json:"remove_originals"`
	// Language of the settings page and dialogs ("en", "es"); empty: the computer's.
	Language string `json:"language,omitempty"`
}

func configPath() (string, error) {
	if p := os.Getenv("PHOTO_BACKUP_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "photo-backup", "config.json"), nil
}

// LoadConfig reads the config, filling in (and saving) defaults for anything missing.
func LoadConfig(path string) (*Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Editors on Windows (Notepad, PowerShell 5) may save UTF-8 with a byte-order mark.
		data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	changed := false
	if c.Dest == "" {
		c.Dest, changed = defaultDest(), true
	}
	if c.Port == 0 {
		c.Port, changed = 8765, true
	}
	// Older configs defaulted to the raw hostname ("MacBook-Air.local").
	if c.Name == "" || strings.HasSuffix(strings.ToLower(c.Name), ".local") {
		c.Name, changed = friendlyName(), true
	}
	if c.ID == "" {
		c.ID, changed = randomHex(8), true
	}
	if c.AlbumFolders == nil {
		on := true
		c.AlbumFolders, changed = &on, true
	}
	if c.Format == "" {
		c.Format, changed = "avif", true
	}
	// Older versions stored raw qualities ("quality", "qualities", "video_crf"); those
	// are replaced by perceptual levels.
	if !validPhotoLevel(c.PhotoLevel) {
		c.PhotoLevel, changed = defaultPhotoLevel, true
	}
	if !validVideoLevel(c.VideoLevel) {
		c.VideoLevel, changed = defaultVideoLevel, true
	}
	if c.Token == "" {
		c.Token, changed = randomToken(), true
	}
	if changed {
		if err := c.Save(path); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaultDest() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Pictures", "PhotoBackup")
}

func randomToken() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Compression returns the compression settings.
func (c *Config) Compression() CompressSettings {
	return CompressSettings{Enabled: c.Compress, Format: c.Format, PhotoLevel: findPhotoLevel(c.PhotoLevel).ID,
		RemoveOriginals: c.RemoveOriginals, Videos: c.CompressVideos, VideoLevel: findVideoLevel(c.VideoLevel).ID}
}
