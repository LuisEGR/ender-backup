package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// When the chosen folder is on a drive that isn't connected, photo-backup saves to the
// default folder instead, says so, and switches back as soon as the drive is back. The
// settings file keeps the drive's folder.

// errDriveMissing means the folder is on a drive (or volume) that isn't mounted.
var errDriveMissing = errors.New("its drive isn't connected")

// mountMissing reports whether root can't exist right now because the volume it lives on
// isn't mounted: e.g. /Volumes/Kingston/Photos with /Volumes/Kingston missing, or E:\Photos
// with no E: drive. Creating the folder would put it on the internal disk instead.
func mountMissing(root string) bool {
	p := filepath.Clean(root)
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			return true // not even the drive's root exists (Windows drive letter)
		}
		p = parent
	}
	if p == filepath.Clean(root) {
		return false // the folder itself exists
	}
	// p is the deepest part that exists; the next level down would be the volume.
	switch filepath.ToSlash(p) {
	case "/Volumes", "/media", "/mnt", "/run/media":
		return true
	}
	if d := filepath.ToSlash(filepath.Dir(p)); d == "/media" || d == "/run/media" {
		return true // /media/<user>/<volume>
	}
	return false
}

// openOrFallBack opens the chosen folder or, if that fails, the default folder. The
// returned warning explains the fallback ("" when the chosen folder is used).
func openOrFallBack(root string) (*Destination, string, error) {
	d, err := OpenDestination(root)
	if err == nil {
		return d, "", nil
	}
	fallback := defaultDest()
	if samePath(fallback, root) {
		return nil, "", err
	}
	d, ferr := OpenDestination(fallback)
	if ferr != nil {
		return nil, "", fmt.Errorf("%v (and the default folder %s: %v)", err, fallback, ferr)
	}
	return d, fmt.Sprintf("Can't use %s: %v. Saving to %s until it's available again.", root, err, fallback), nil
}

// fallBack records that the server is using the default folder instead of preferred.
func (s *Server) fallBack(preferred, warning string) {
	s.mu.Lock()
	s.preferredDest, s.destWarning = preferred, warning
	s.mu.Unlock()
	s.events.add("Warning: %s", warning)
}

// clearFallback forgets the folder waited for (the user chose another one).
func (s *Server) clearFallback() {
	s.mu.Lock()
	s.preferredDest, s.destWarning = "", ""
	s.mu.Unlock()
}

func (s *Server) fallbackState() (preferred, warning string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.preferredDest, s.destWarning
}

// watchPreferredDest switches back to the chosen folder once its drive is connected.
func (s *Server) watchPreferredDest(every time.Duration) {
	for range time.Tick(every) {
		preferred, _ := s.fallbackState()
		if preferred == "" || mountMissing(preferred) {
			continue
		}
		if err := s.SetDestination(preferred); err != nil {
			continue
		}
		s.clearFallback()
		s.events.add("%s is available again — saving there", preferred)
	}
}

func (s *Server) destWarningText() string {
	_, w := s.fallbackState()
	return w
}
