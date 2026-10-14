package main

import "testing"

func TestUSBGeneration(t *testing.T) {
	for link, want := range map[string]string{
		"480 Mb/s": "USB 2.0 (480 Mb/s)",
		"5 Gb/s":   "USB 3.2 Gen 1 (5 Gb/s)",
		"10 Gb/s":  "USB 3.2 Gen 2 (10 Gb/s)",
		"20 Gb/s":  "USB 3.2 Gen 2x2 (20 Gb/s)",
		"40 Gb/s":  "USB4 (40 Gb/s)",
		"weird":    "USB",
	} {
		if got := usbGeneration(link); got != want {
			t.Errorf("usbGeneration(%q) = %q, want %q", link, got, want)
		}
	}
}

// Logs what the platform reports for the temp dir's drive (no assertion: it depends on the machine).
func TestDescribeDrive(t *testing.T) {
	name, conn := describeDrive(t.TempDir())
	t.Logf("drive: %q, connection: %q", name, conn)
}

func TestHumanBytesDecimal(t *testing.T) {
	for n, want := range map[int64]string{999: "999 B", 1500: "1.5 KB", 552_300_000: "552.3 MB", 1_000_000_000_000: "1.0 TB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
