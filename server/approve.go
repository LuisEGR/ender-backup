package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// runApprove approves pairing requests from the terminal, for servers with no screen
// (a Linux box over SSH): it talks to the running server's localhost-only settings API.
func runApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	cfg, _, _, err := loadConfig(fs)
	if err != nil {
		return err
	}
	port := fs.Int("port", cfg.Port, "port of the running server")
	yes := fs.Bool("yes", false, "approve every waiting request without asking")
	wait := fs.Duration("wait", 0, "keep waiting this long for a request (e.g. 2m) if none is pending")
	fs.Parse(args)

	base := fmt.Sprintf("http://127.0.0.1:%d", *port)
	client := http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(*wait)
	in := bufio.NewReader(os.Stdin)

	for {
		var state struct {
			PairRequests []struct {
				ID         string `json:"id"`
				DeviceName string `json:"device_name"`
			} `json:"pair_requests"`
		}
		resp, err := client.Get(base + "/ui/state")
		if err != nil {
			return fmt.Errorf("no photo-backup server running on port %d (%v)", *port, err)
		}
		err = json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if err != nil {
			return err
		}

		if len(state.PairRequests) == 0 {
			if time.Now().Before(deadline) {
				time.Sleep(time.Second)
				continue
			}
			fmt.Println("No phones are waiting to pair. Tap this computer in the app, then run this again (or use -wait 2m).")
			return nil
		}

		for _, r := range state.PairRequests {
			if !*yes {
				fmt.Printf("%q wants to pair with this computer. Approve? [y/N] ", r.DeviceName)
				answer, _ := in.ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
					post(client, base+"/ui/pair/deny", r.ID)
					fmt.Println("Denied.")
					continue
				}
			}
			if err := post(client, base+"/ui/pair/approve", r.ID); err != nil {
				fmt.Printf("Could not approve %q: %v\n", r.DeviceName, err)
				continue
			}
			fmt.Printf("Approved %q.\n", r.DeviceName)
		}
		return nil
	}
}

func post(client http.Client, url, id string) error {
	body, _ := json.Marshal(map[string]string{"id": id})
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server said %s", resp.Status)
	}
	return nil
}
