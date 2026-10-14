package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// deviceActivity is what each phone is doing right now, for the settings page's
// overview. It lives in memory only: after a restart it fills in as phones reconnect.
type deviceActivity struct {
	lastRequest  time.Time
	lastUpload   time.Time
	sessionStart time.Time
	current      string // file name of the last upload
	sessionItems int
	sessionBytes int64
	// The phone's own count of its library (X-Library: "done/total"), from app
	// versions that send it.
	libDone, libTotal int
	libAt             time.Time
}

// A pause longer than this between uploads starts a new backup session.
const sessionGap = 10 * time.Minute

type activityLog struct {
	mu sync.Mutex
	m  map[string]*deviceActivity
}

func (a *activityLog) get(device string) *deviceActivity {
	if a.m == nil {
		a.m = map[string]*deviceActivity{}
	}
	d := a.m[device]
	if d == nil {
		d = &deviceActivity{}
		a.m[device] = d
	}
	return d
}

// request records any authenticated request from a phone.
func (a *activityLog) request(r *http.Request, now time.Time) {
	device := r.Header.Get("X-Device-Id")
	if device == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.get(device)
	d.lastRequest = now
	if done, total, ok := parseLibrary(r.Header.Get("X-Library")); ok {
		d.libDone, d.libTotal, d.libAt = done, total, now
	}
}

func parseLibrary(v string) (done, total int, ok bool) {
	a, b, found := strings.Cut(v, "/")
	if !found {
		return 0, 0, false
	}
	done, err1 := strconv.Atoi(strings.TrimSpace(a))
	total, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || total < 0 || done < 0 || done > total {
		return 0, 0, false
	}
	return done, total, true
}

// upload records a received file.
func (a *activityLog) upload(device, name string, size int64, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.get(device)
	if now.Sub(d.lastUpload) > sessionGap {
		d.sessionStart, d.sessionItems, d.sessionBytes = now, 0, 0
	}
	d.lastUpload, d.lastRequest, d.current = now, now, name
	d.sessionItems++
	d.sessionBytes += size
}

// deviceStatus is one phone's live status as the settings page shows it.
type deviceStatus struct {
	State        string `json:"state"` // "uploading", "connected" or "away"
	Current      string `json:"current,omitempty"`
	SessionItems int    `json:"session_items,omitempty"`
	SessionBytes int64  `json:"session_bytes,omitempty"`
	SessionStart int64  `json:"session_start,omitempty"`
	LibraryDone  int    `json:"library_done,omitempty"`
	LibraryTotal int    `json:"library_total,omitempty"`
	LibraryAt    int64  `json:"library_at,omitempty"`
}

func (a *activityLog) status(device string, now time.Time) deviceStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.m[device]
	if d == nil {
		return deviceStatus{State: "away"}
	}
	st := deviceStatus{State: "away"}
	switch {
	case now.Sub(d.lastUpload) < 30*time.Second:
		st.State, st.Current = "uploading", d.current
	case now.Sub(d.lastRequest) < 90*time.Second:
		st.State = "connected"
	}
	if now.Sub(d.lastUpload) < sessionGap*6 && d.sessionItems > 0 {
		st.SessionItems, st.SessionBytes, st.SessionStart = d.sessionItems, d.sessionBytes, d.sessionStart.Unix()
	}
	if d.libTotal > 0 {
		st.LibraryDone, st.LibraryTotal, st.LibraryAt = d.libDone, d.libTotal, d.libAt.Unix()
	}
	return st
}
