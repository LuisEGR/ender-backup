package main

import (
	"net/http"
	"testing"
)

func (e *testEnv) pairStatus(id string) map[string]any {
	resp, out := e.ui("GET", "/pair/status?id="+id, "", nil, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("status: %d", resp.StatusCode)
	}
	return out
}

func (e *testEnv) requestPair(name string) string {
	resp, out := e.ui("POST", "/pair/request", "application/json",
		map[string]string{"device_id": "dev9", "device_name": name}, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("request: %d %v", resp.StatusCode, out)
	}
	return out["request_id"].(string)
}

func TestPairInfoIsPublic(t *testing.T) {
	e := newTestEnv(t)
	resp, out := e.ui("GET", "/pair/info", "", nil, nil)
	if resp.StatusCode != 200 || out["id"] != "srv123" || out["token"] != nil {
		t.Fatalf("pair/info = %d %v", resp.StatusCode, out)
	}
}

func TestPairApproveHandsOutTokenOnce(t *testing.T) {
	e := newTestEnv(t)
	rid := e.requestPair("Luis's iPhone")
	if got := e.pairStatus(rid); got["status"] != "pending" || got["token"] != nil {
		t.Fatalf("before approval: %v", got)
	}
	_, state := e.ui("GET", "/ui/state", "", nil, nil)
	if reqs := state["pair_requests"].([]any); len(reqs) != 1 {
		t.Fatalf("pending requests on settings page: %v", reqs)
	}
	if resp, _ := e.ui("POST", "/ui/pair/approve", "application/json", map[string]string{"id": rid}, nil); resp.StatusCode != 200 {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	got := e.pairStatus(rid)
	if got["status"] != "approved" || got["token"] != "secret" || got["id"] != "srv123" {
		t.Fatalf("after approval: %v", got)
	}
	if again := e.pairStatus(rid); again["status"] != "expired" || again["token"] != nil {
		t.Fatalf("token handed out twice: %v", again)
	}
}

func TestPairDeny(t *testing.T) {
	e := newTestEnv(t)
	rid := e.requestPair("Stranger")
	e.ui("POST", "/ui/pair/deny", "application/json", map[string]string{"id": rid}, nil)
	if got := e.pairStatus(rid); got["status"] != "denied" || got["token"] != nil {
		t.Fatalf("after deny: %v", got)
	}
}

func TestPairApproveOnlyFromThisComputer(t *testing.T) {
	e := newTestEnv(t)
	rid := e.requestPair("Stranger")
	resp, _ := e.ui("POST", "/ui/pair/approve", "application/json", map[string]string{"id": rid},
		func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin approve: %d", resp.StatusCode)
	}
}

func TestBonjourInstance(t *testing.T) {
	for in, want := range map[string]string{
		"MacBook Air de Luis":         "MacBook Air de Luis",
		"MacBook-Air-de-Luis-2.local": "MacBook-Air-de-Luis-2-local",
		`a\b`:                         "a-b",
		"":                            "photo-backup",
	} {
		if got := bonjourInstance(in); got != want {
			t.Errorf("bonjourInstance(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPairRequestLimit(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < maxPendingPairs; i++ {
		e.requestPair("spam")
	}
	resp, _ := e.ui("POST", "/pair/request", "application/json", map[string]string{"device_name": "one more"}, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", resp.StatusCode)
	}
}
