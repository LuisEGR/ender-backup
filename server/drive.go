package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DriveInfo describes the disk holding the destination folder.
type DriveInfo struct {
	Name       string  `json:"name,omitempty"`        // e.g. "Kingston XS2000"
	Connection string  `json:"connection,omitempty"`  // e.g. "USB 3.2 Gen 2 (10 Gb/s)", "Internal SSD"
	WriteSpeed float64 `json:"write_speed,omitempty"` // bytes/s from the last speed test
	TestedAt   int64   `json:"tested_at,omitempty"`
	Testing    bool    `json:"testing"`
}

type driveState struct {
	mu   sync.Mutex
	info DriveInfo
}

func (d *Destination) Drive() DriveInfo {
	d.drive.mu.Lock()
	defer d.drive.mu.Unlock()
	return d.drive.info
}

// ProbeDrive identifies the drive and measures its write speed in the background,
// after any running index scan (so the two don't slow each other down).
func (d *Destination) ProbeDrive(events *eventLog) {
	d.drive.mu.Lock()
	if d.drive.info.Testing {
		d.drive.mu.Unlock()
		return
	}
	d.drive.info.Testing = true
	d.drive.mu.Unlock()

	go func() {
		name, conn := describeDrive(d.root)
		d.drive.mu.Lock()
		d.drive.info.Name, d.drive.info.Connection = name, conn
		d.drive.mu.Unlock()

		for running, _ := d.Indexing(); running; running, _ = d.Indexing() {
			time.Sleep(500 * time.Millisecond)
		}
		speed, err := measureWriteSpeed(d.tmpDir)
		d.drive.mu.Lock()
		d.drive.info.Testing = false
		if err == nil {
			d.drive.info.WriteSpeed, d.drive.info.TestedAt = speed, time.Now().Unix()
		}
		d.drive.mu.Unlock()
		if err != nil {
			events.add("Disk speed test failed: %v", err)
			return
		}
		var label []string
		for _, s := range []string{name, conn} {
			if s != "" {
				label = append(label, s)
			}
		}
		events.add("Disk write speed: %s/s  %s", humanBytes(int64(speed)), strings.Join(label, " · "))
	}()
}

// measureWriteSpeed writes up to 256 MB of random data (stopping after ~8 s on slow
// disks), forces it to the device, and deletes it.
func measureWriteSpeed(dir string) (float64, error) {
	const chunk, limit = 4 << 20, 256 << 20
	buf := make([]byte, chunk)
	if _, err := rand.Read(buf); err != nil { // incompressible, so the drive can't cheat
		return 0, err
	}
	f, err := os.CreateTemp(dir, "speedtest-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	start := time.Now()
	written := 0
	for written < limit && (written < 32<<20 || time.Since(start) < 8*time.Second) {
		n, err := f.Write(buf)
		if err != nil {
			return 0, err
		}
		written += n
	}
	if err := f.Sync(); err != nil { // F_FULLFSYNC on macOS: really on the device
		return 0, err
	}
	return float64(written) / time.Since(start).Seconds(), nil
}

// usbGeneration names a USB link speed as reported by macOS ("10 Gb/s", "480 Mb/s").
func usbGeneration(link string) string {
	fields := strings.Fields(link)
	if len(fields) != 2 {
		return "USB"
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "USB"
	}
	mbps := v
	if strings.HasPrefix(fields[1], "Gb") {
		mbps = v * 1000
	}
	var gen string
	switch {
	case mbps <= 12:
		gen = "USB 1.1"
	case mbps <= 480:
		gen = "USB 2.0"
	case mbps <= 5000:
		gen = "USB 3.2 Gen 1"
	case mbps <= 10000:
		gen = "USB 3.2 Gen 2"
	case mbps <= 20000:
		gen = "USB 3.2 Gen 2x2"
	default:
		gen = "USB4"
	}
	return fmt.Sprintf("%s (%s)", gen, link)
}
