package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Finding other photo-backup servers on the Tailscale network. Bonjour doesn't cross
// Tailscale (no multicast), so the phone, away from home, asks any server it can reach
// for this list (/pair/peers). Each server reads its tailnet's devices from the local
// Tailscale app and checks which answer on photo-backup's usual ports.

// peerPorts are the ports a photo-backup server is looked for on.
var peerPorts = []int{8765, 8766, 8767, 8768, 8769, 8770}

type tailnetPeer struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	OS    string   `json:"os,omitempty"`
	Hosts []string `json:"hosts"`
	Port  int      `json:"port"`
}

type peerList struct {
	mu    sync.Mutex
	peers []tailnetPeer
}

// tailscaleCLI returns the path of the Tailscale command-line tool, or "".
func tailscaleCLI() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{"/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/opt/homebrew/bin/tailscale", "/usr/local/bin/tailscale"}
	case "windows":
		candidates = []string{os.Getenv("ProgramFiles") + `\Tailscale\tailscale.exe`}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// tailnetDevices returns the IPv4 Tailscale addresses of the other online devices.
func tailnetDevices(ctx context.Context) ([]string, error) {
	cli := tailscaleCLI()
	if cli == "" {
		return nil, nil // Tailscale isn't installed
	}
	cmd := exec.CommandContext(ctx, cli, "status", "--json")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var st struct {
		Peer map[string]struct {
			TailscaleIPs []string
			Online       bool
		}
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, err
	}
	var ips []string
	for _, p := range st.Peer {
		if !p.Online {
			continue
		}
		for _, ip := range p.TailscaleIPs {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips, nil
}

// probePeer returns the photo-backup server answering at ip on one of peerPorts.
func probePeer(ctx context.Context, ip string) (tailnetPeer, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	type hit struct {
		peer tailnetPeer
		ok   bool
	}
	results := make(chan hit, len(peerPorts))
	for _, port := range peerPorts {
		go func(port int) {
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort(ip, strconv.Itoa(port))+"/pair/info", nil)
			resp, err := client.Do(req)
			if err != nil {
				results <- hit{}
				return
			}
			defer resp.Body.Close()
			var info struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				OS   string `json:"os"`
			}
			if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&info) != nil || info.ID == "" {
				results <- hit{}
				return
			}
			results <- hit{tailnetPeer{ID: info.ID, Name: info.Name, OS: info.OS, Hosts: []string{ip}, Port: port}, true}
		}(port)
	}
	for range peerPorts {
		if r := <-results; r.ok {
			return r.peer, true
		}
	}
	return tailnetPeer{}, false
}

// findPeers lists the photo-backup servers on the tailnet other than this one.
func (s *Server) findPeers(ctx context.Context) []tailnetPeer {
	ips, err := tailnetDevices(ctx)
	if err != nil || len(ips) == 0 {
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var found []tailnetPeer
	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			if p, ok := probePeer(ctx, ip); ok && p.ID != s.id {
				mu.Lock()
				found = append(found, p)
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// watchPeers refreshes the list of tailnet servers every couple of minutes.
func (s *Server) watchPeers() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		peers := s.findPeers(ctx)
		cancel()
		s.peers.mu.Lock()
		s.peers.peers = peers
		s.peers.mu.Unlock()
		time.Sleep(2 * time.Minute)
	}
}

// handlePeers lists the other photo-backup servers this one found on the tailnet.
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	s.peers.mu.Lock()
	peers := append([]tailnetPeer{}, s.peers.peers...)
	s.peers.mu.Unlock()
	writeJSON(w, map[string]any{"peers": peers})
}
