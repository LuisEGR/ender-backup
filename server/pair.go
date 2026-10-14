package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// Pairing without a QR code: the phone finds the server over Bonjour, asks to
// pair, and gets the token only after someone approves on the settings page.
//
//   GET  /pair/info              {id, name, version}          — unauthenticated
//   POST /pair/request           {device_id, device_name} -> {request_id}
//   GET  /pair/status?id=<rid>   {status: pending|approved|denied|expired, token?...}

const (
	bonjourService  = "_photobackup._tcp"
	pairRequestTTL  = 3 * time.Minute
	maxPendingPairs = 10
)

type pairRequest struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"-"`
	DeviceName string    `json:"device_name"`
	Created    time.Time `json:"-"`
	Status     string    `json:"-"`

	closeDialog context.CancelFunc // closes the native dialog once answered elsewhere
}

type pairRequests struct {
	mu    sync.Mutex
	items map[string]*pairRequest
}

func (p *pairRequests) purgeLocked() {
	for id, r := range p.items {
		if time.Since(r.Created) > pairRequestTTL {
			delete(p.items, id)
		}
	}
}

func (p *pairRequests) pending() []pairRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purgeLocked()
	out := []pairRequest{}
	for _, r := range p.items {
		if r.Status == "pending" {
			out = append(out, *r)
		}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Server) pairHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pair/info", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		writeJSON(w, map[string]any{"id": s.id, "name": s.name, "version": apiVersion, "os": runtime.GOOS})
	})
	mux.HandleFunc("POST /pair/request", s.handlePairRequest)
	mux.HandleFunc("GET /pair/status", s.handlePairStatus)
	mux.HandleFunc("GET /pair/peers", s.handlePeers) // other servers on the tailnet (tailnet.go)
	return mux
}

func (s *Server) handlePairRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.DeviceName)
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "A phone"
	}
	s.pairs.mu.Lock()
	s.pairs.purgeLocked()
	if s.pairs.items == nil {
		s.pairs.items = map[string]*pairRequest{}
	}
	if len(s.pairs.items) >= maxPendingPairs {
		s.pairs.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too many pairing requests; try again in a few minutes")
		return
	}
	pr := &pairRequest{ID: randomHex(16), DeviceID: req.DeviceID, DeviceName: name, Created: time.Now(), Status: "pending"}
	s.pairs.items[pr.ID] = pr
	s.pairs.mu.Unlock()

	s.events.add("%s wants to pair — approve it on this computer", name)
	if s.nativeDialogs {
		ctx, cancel := context.WithTimeout(context.Background(), pairRequestTTL)
		pr.closeDialog = cancel
		go s.askNatively(ctx, pr.ID, name)
	}
	writeJSON(w, map[string]any{"request_id": pr.ID, "expires_in": int(pairRequestTTL.Seconds())})
}

// askNatively shows an Approve/Deny dialog on the desktop. The settings page shows
// the same request, so if the dialog can't be shown (e.g. running over SSH) or is
// closed, the request can still be answered there.
func (s *Server) askNatively(ctx context.Context, id, deviceName string) {
	lang := s.language()
	msg := fmt.Sprintf(tr(lang, "“%s” wants to pair with this computer.\n\nOnly approve if you just tapped this computer in the Ender app."), deviceName)
	approve, err := askApproval(ctx, msg, lang)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("pairing dialog unavailable (%v) — use the settings page", err)
		}
		return
	}
	s.decidePair(id, approve)
}

func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.pairs.mu.Lock()
	s.pairs.purgeLocked()
	pr, ok := s.pairs.items[id]
	status := "expired"
	if ok {
		status = pr.Status
		if status != "pending" {
			delete(s.pairs.items, id) // the answer (and token) is handed out once
		}
	}
	s.pairs.mu.Unlock()

	resp := map[string]any{"status": status}
	if status == "approved" {
		s.mu.RLock()
		resp["token"], resp["name"], resp["id"] = s.token, s.name, s.id
		resp["hosts"], resp["port"] = s.hosts, s.port
		s.mu.RUnlock()
	}
	writeJSON(w, resp)
}

// decidePair is called from the settings page.
func (s *Server) decidePair(id string, approve bool) bool {
	s.pairs.mu.Lock()
	defer s.pairs.mu.Unlock()
	pr, ok := s.pairs.items[id]
	if !ok || pr.Status != "pending" {
		return false
	}
	if pr.closeDialog != nil {
		pr.closeDialog()
	}
	if approve {
		pr.Status = "approved"
		s.events.add("Paired with %s", pr.DeviceName)
	} else {
		pr.Status = "denied"
		s.events.add("Denied pairing request from %s", pr.DeviceName)
	}
	return true
}

// Bonjour advertisement.

type advertiser interface {
	SetText(txt []string)
	Shutdown()
}

var errNoSystemBonjour = errors.New("no system Bonjour")

func (s *Server) bonjourText() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return []string{
		"v=" + strconv.Itoa(apiVersion),
		"os=" + runtime.GOOS, // darwin, windows, linux: the app picks an icon
		"id=" + s.id,
		"n=" + s.name,
		"p=" + strconv.Itoa(s.port),
		"h=" + strings.Join(s.hosts, ","),
	}
}

// StartBonjour announces the server on the local network. Failure is not fatal:
// the QR code still works.
func (s *Server) StartBonjour() {
	s.mu.RLock()
	instance, port, ips := s.name, s.port, s.hosts
	s.mu.RUnlock()
	adv, err := registerSystemBonjour(instance, port, s.bonjourText())
	if errors.Is(err, errNoSystemBonjour) {
		host, _ := os.Hostname()
		host = strings.Split(host, ".")[0] // zeroconf appends ".local."
		var zc *zeroconf.Server
		zc, err = zeroconf.RegisterProxy(bonjourInstance(instance), bonjourService, "local.", port, host, ips, s.bonjourText(), nil)
		if err == nil {
			// Short lifetime: if we crash without a goodbye, phones forget us within minutes.
			zc.TTL(120)
			adv = zc
		}
	}
	if err != nil {
		log.Printf("Bonjour disabled: %v", err)
		return
	}
	s.bonjourMu.Lock()
	s.bonjour = adv
	s.bonjourMu.Unlock()
}

// bonjourInstance makes a name safe for zeroconf, which doesn't escape dots or
// backslashes (they would split the DNS label). Phones display the TXT "n" instead.
func bonjourInstance(name string) string {
	name = strings.NewReplacer(".", "-", "\\", "-").Replace(strings.TrimSpace(name))
	if len(name) > 63 {
		name = name[:63]
	}
	if name == "" {
		name = "photo-backup"
	}
	return name
}

func (s *Server) refreshBonjour() {
	s.bonjourMu.Lock()
	defer s.bonjourMu.Unlock()
	if s.bonjour != nil {
		s.bonjour.SetText(s.bonjourText())
	}
}

func (s *Server) StopBonjour() {
	s.bonjourMu.Lock()
	defer s.bonjourMu.Unlock()
	if s.bonjour != nil {
		s.bonjour.Shutdown() // sends goodbye packets so phones drop us from their list
		s.bonjour = nil
	}
}
