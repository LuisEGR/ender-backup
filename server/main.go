// photo-backup receives photos and videos from the iPhone app over the local
// network and keeps a SQLite index of what has been safely stored.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func usage() {
	fmt.Fprintf(os.Stderr, `photo-backup â€” wireless iPhone photo/video backup

Usage:
  photo-backup                  start the server and open the settings page in the browser
  photo-backup serve  [flags]   start the server (headless; settings page still at http://localhost:<port>)
  photo-backup status [flags]   show what has been backed up per device
  photo-backup verify [flags]   check that every indexed file is still on disk
  photo-backup approve [-yes]   approve phones waiting to pair (for computers without a screen, e.g. over SSH)

Settings are saved in the config file (see "photo-backup serve -h"); flags override them for one run.
`)
}

func main() {
	if os.Getenv(convertWorkerEnv) == "1" { // the photo conversion helper (convertproc.go)
		runConvertWorker()
		return
	}
	cmd, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		err = runServe(append([]string{"-open"}, args...))
	case "serve":
		err = runServe(args)
	case "status":
		err = runStatus(args)
	case "approve":
		err = runApprove(args)
	case "verify":
		err = runVerify(args)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// loadConfig reads the config file and registers -dest with its value as default.
func loadConfig(fs *flag.FlagSet) (cfg *Config, cfgPath string, dest *string, err error) {
	cfgPath, err = configPath()
	if err != nil {
		return nil, "", nil, err
	}
	cfg, err = LoadConfig(cfgPath)
	if err != nil {
		return nil, "", nil, fmt.Errorf("config %s: %w", cfgPath, err)
	}
	dest = fs.String("dest", cfg.Dest, "destination folder for photos and videos")
	return cfg, cfgPath, dest, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, cfgPath, dest, err := loadConfig(fs)
	if err != nil {
		return err
	}
	port := fs.Int("port", cfg.Port, "TCP port to listen on")
	hostsFlag := fs.String("host", "", "comma-separated address(es) to put in the QR (default: auto-detect)")
	newToken := fs.Bool("new-token", false, "rotate the pairing token (already-paired phones must re-scan)")
	name := fs.String("name", cfg.Name, "name shown in the app")
	deviceFolders := fs.Bool("device-folders", cfg.DeviceFolders, "store each phone's files in its own subfolder")
	open := fs.Bool("open", false, "open the settings page in the browser")
	desktop := fs.Bool("desktop", false, "run inside the desktop app: print the settings URL as PHOTO_BACKUP_URL=â€¦ (and only that if one is already running)")
	noDialogs := fs.Bool("no-dialogs", false, "approve pairing only on the settings page, without desktop dialogs")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: photo-backup serve [flags]\n\nConfig file: %s\n\n", cfgPath)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if *newToken {
		cfg.Token = randomToken()
		if err := cfg.Save(cfgPath); err != nil {
			return err
		}
	}
	settingsURL := fmt.Sprintf("http://localhost:%d/", *port)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		// Launched again while already running: just show the existing instance.
		if *desktop && alreadyRunning(*port) {
			// The desktop app shows the server that's already running (e.g. started at login).
			fmt.Printf("PHOTO_BACKUP_URL=%s\nPHOTO_BACKUP_ALREADY_RUNNING=1\n", settingsURL)
			return nil
		}
		if *open && alreadyRunning(*port) {
			fmt.Printf("photo-backup is already running â€” opening %s\n", settingsURL)
			return openBrowser(settingsURL)
		}
		return err
	}

	d, warning, err := openOrFallBack(*dest)
	if err != nil {
		return err
	}
	srv := NewServer(d, cfg.Token, *name)
	if warning != "" {
		srv.fallBack(*dest, warning) // logged to the terminal and the settings page
	}
	go srv.watchPreferredDest(10 * time.Second)
	go srv.watchPeers()
	defer srv.Close()
	srv.id = cfg.ID
	srv.deviceFolders.Store(*deviceFolders)
	srv.albumFolders.Store(cfg.AlbumFolders == nil || *cfg.AlbumFolders)
	srv.lang = cfg.Language
	d.StartIndexing(&srv.events)
	d.ProbeDrive(&srv.events)
	srv.compress = cfg.Compression()
	d.StartConverter(srv.compressSettings, &srv.events)
	d.StartWatching(30 * time.Second)
	srv.nativeDialogs = !*noDialogs
	srv.cfgPath, srv.port = cfgPath, *port
	srv.hosts = localAddresses()
	if *hostsFlag != "" {
		srv.hosts = strings.Split(*hostsFlag, ",")
	}
	if len(srv.hosts) == 0 {
		return fmt.Errorf("no network address found; pass -host")
	}
	srv.StartBonjour()
	defer srv.StopBonjour()
	go func() {
		// Ctrl+C: withdraw the Bonjour announcement before exiting.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		srv.StopBonjour()
		os.Exit(0)
	}()
	if *desktop {
		// The desktop app holds our stdin open; when it ends (even killed) stdin closes and
		// the server goes with it instead of lingering without its window and tray icon.
		go func() {
			io.Copy(io.Discard, os.Stdin)
			srv.StopBonjour()
			os.Exit(0)
		}()
	}

	fmt.Printf("\n  Ender Backup is running as %q â€” pair from the app on your iPhone.\n\n", *name)
	fmt.Printf("  Saving to:  %s\n", d.root)
	for _, h := range srv.hosts {
		fmt.Printf("  Listening:  http://%s\n", net.JoinHostPort(h, strconv.Itoa(*port)))
	}
	fmt.Printf("  Settings:   %s\n\n", settingsURL)
	if *desktop {
		fmt.Printf("PHOTO_BACKUP_URL=%s\n", settingsURL) // read by the desktop app
	}

	if *open {
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := openBrowser(settingsURL); err != nil {
				log.Printf("open browser: %v", err)
			}
		}()
	}

	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return hs.Serve(ln)
}

func alreadyRunning(port int) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/ui/state", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// pairingURL is what the QR code encodes; the app tries each host in order.
func pairingURL(hosts []string, port int, token, name, id string) string {
	v := url.Values{}
	v.Set("i", id)
	v.Set("h", strings.Join(hosts, ","))
	v.Set("p", strconv.Itoa(port))
	v.Set("t", token)
	v.Set("n", name)
	return "photobackup://pair?" + v.Encode()
}

// localAddresses returns IPv4 addresses of up interfaces, private LAN ranges
// first (192.168/10/172.16), then others such as Tailscale's 100.64/10.
// virtualInterface reports container/VM bridges (Docker, libvirt, WSL/Hyper-V) whose
// addresses a phone on the Wi-Fi can never reach.
func virtualInterface(name string) bool {
	n := strings.ToLower(name)
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "vethernet", "cni", "flannel", "podman"} {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func localAddresses() []string {
	var lan, other []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualInterface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.IsPrivate() {
				lan = append(lan, ip.String())
			} else {
				other = append(other, ip.String())
			}
		}
	}
	return append(lan, other...)
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	_, _, dest, err := loadConfig(fs)
	if err != nil {
		return err
	}
	fs.Parse(args)
	d, err := OpenDestination(*dest)
	if err != nil {
		return err
	}
	defer d.Close()

	stats, err := d.store.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("Destination: %s\n\n", d.root)
	if len(stats) == 0 {
		fmt.Println("No devices have connected yet.")
		return nil
	}
	for _, s := range stats {
		label := s.Name
		if label == "" {
			label = s.ID
		}
		fmt.Printf("%s  (folder: %s)\n  backed up:  %d assets (%d files, %s)\n  deleted on phone: %d\n  last seen:  %s\n\n",
			label, s.Folder, s.Assets, s.Files, humanBytes(s.Bytes), s.Deleted, s.LastSeen.Format("2006-01-02 15:04"))
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	_, _, dest, err := loadConfig(fs)
	if err != nil {
		return err
	}
	fs.Parse(args)
	d, err := OpenDestination(*dest)
	if err != nil {
		return err
	}
	defer d.Close()

	missing, err := d.store.MissingFiles()
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		fmt.Println("All indexed files are present.")
		return nil
	}
	for _, f := range missing {
		fmt.Printf("MISSING  %s  (asset %s, %s)\n", f.RelPath, f.AssetID, f.Resource)
	}
	log.Printf("%d files missing â€” those assets will not be reported as safe to delete", len(missing))
	return nil
}
