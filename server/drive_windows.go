//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
)

// describeDrive returns the disk's name and bus type (USB, NVMe, SATA…). Windows
// doesn't expose the negotiated USB version.
func describeDrive(path string) (name, connection string) {
	const script = `$l = (Get-Item -LiteralPath $env:PB_PATH).PSDrive.Name
$d = Get-Partition -DriveLetter $l | Get-Disk
"$($d.FriendlyName)|$($d.BusType)"`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "PB_PATH="+path)
	out, err := cmd.Output()
	if err != nil {
		return "", ""
	}
	name, bus, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	return strings.TrimSpace(name), strings.TrimSpace(bus)
}
