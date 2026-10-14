//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// describeDrive returns the disk's model and how it is connected (USB version from
// sysfs, NVMe, SATA SSD/HDD), for the disk holding path.
func describeDrive(path string) (name, connection string) {
	out, err := exec.Command("findmnt", "-no", "SOURCE", "--target", path).Output()
	if err != nil {
		return "", ""
	}
	source := strings.TrimSpace(string(out)) // e.g. /dev/sda2, /dev/nvme0n1p1
	if !strings.HasPrefix(source, "/dev/") {
		return "", "" // tmpfs, network or container filesystems
	}
	disk := filepath.Base(source)
	if out, err := exec.Command("lsblk", "-no", "PKNAME", source).Output(); err == nil {
		if parent := strings.TrimSpace(string(out)); parent != "" {
			disk = strings.Fields(parent)[0]
		}
	}
	sys := "/sys/block/" + disk

	vendor := read(sys + "/device/vendor")
	if vendor == "ATA" { // generic placeholder the kernel reports for SATA disks
		vendor = ""
	}
	name = strings.Join(strings.Fields(vendor+" "+read(sys+"/device/model")), " ")

	transport := ""
	if out, err := exec.Command("lsblk", "-dno", "TRAN", "/dev/"+disk).Output(); err == nil {
		transport = strings.TrimSpace(string(out))
	}
	switch transport {
	case "usb":
		connection = "USB"
		if speed := usbSpeed(sys); speed != "" {
			connection = usbGeneration(speed)
		}
	case "nvme":
		connection = "NVMe"
	case "sata", "ata":
		connection = "SATA SSD"
		if read(sys+"/queue/rotational") == "1" {
			connection = "SATA HDD"
		}
	default:
		connection = strings.ToUpper(transport)
	}
	return name, connection
}

// usbSpeed walks up from the block device to its USB device and returns the link
// speed in the "5 Gb/s" / "480 Mb/s" form used by usbGeneration.
func usbSpeed(sysBlock string) string {
	dir, err := filepath.EvalSymlinks(sysBlock)
	if err != nil {
		return ""
	}
	for ; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if read(dir+"/idVendor") == "" {
			continue
		}
		var mbps float64
		if _, err := fmt.Sscan(read(dir+"/speed"), &mbps); err != nil || mbps <= 0 {
			return ""
		}
		if mbps >= 1000 {
			return fmt.Sprintf("%g Gb/s", mbps/1000)
		}
		return fmt.Sprintf("%g Mb/s", mbps)
	}
	return ""
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
