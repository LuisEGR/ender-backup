package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeviceActivity(t *testing.T) {
	var a activityLog
	t0 := time.Unix(1_800_000_000, 0)
	if st := a.status("p", t0); st.State != "away" {
		t.Fatalf("unknown phone: %+v", st)
	}
	r := httptest.NewRequest("GET", "/api/info", nil)
	r.Header.Set("X-Device-Id", "p")
	r.Header.Set("X-Library", "90/120")
	a.request(r, t0)
	if st := a.status("p", t0.Add(10*time.Second)); st.State != "connected" || st.LibraryDone != 90 || st.LibraryTotal != 120 {
		t.Fatalf("after a request: %+v", st)
	}
	a.upload("p", "IMG_1.HEIC", 1000, t0.Add(20*time.Second))
	a.upload("p", "IMG_2.HEIC", 500, t0.Add(25*time.Second))
	st := a.status("p", t0.Add(30*time.Second))
	if st.State != "uploading" || st.Current != "IMG_2.HEIC" || st.SessionItems != 2 || st.SessionBytes != 1500 {
		t.Fatalf("uploading: %+v", st)
	}
	if st := a.status("p", t0.Add(5*time.Minute)); st.State != "away" || st.SessionItems != 2 {
		t.Fatalf("later: %+v", st)
	}
	// A long pause starts a new session.
	a.upload("p", "IMG_3.HEIC", 7, t0.Add(time.Hour))
	if st := a.status("p", t0.Add(time.Hour)); st.SessionItems != 1 || st.SessionBytes != 7 {
		t.Fatalf("new session: %+v", st)
	}
	for _, bad := range []string{"", "5", "a/b", "9/5", "-1/3"} {
		if _, _, ok := parseLibrary(bad); ok {
			t.Errorf("parseLibrary(%q) accepted", bad)
		}
	}
}
