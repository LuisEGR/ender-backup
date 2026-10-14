//go:build darwin

package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"syscall"

	"howett.net/plist"
)

type diskutilInfo struct {
	MediaName           string `plist:"MediaName"`
	IORegistryEntryName string `plist:"IORegistryEntryName"`
	BusProtocol         string `plist:"BusProtocol"`
	Internal            bool   `plist:"Internal"`
	ParentWholeDisk     string `plist:"ParentWholeDisk"`
}

func diskutil(dev string) (diskutilInfo, bool) {
	var info diskutilInfo
	out, err := exec.Command("diskutil", "info", "-plist", dev).Output()
	if err != nil {
		return info, false
	}
	_, err = plist.Unmarshal(out, &info)
	return info, err == nil
}

// describeDrive returns the drive's name and how it is connected, using diskutil and,
// for USB drives, the negotiated link speed from system_profiler.
func describeDrive(path string) (name, connection string) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return "", ""
	}
	vol, ok := diskutil(cString(st.Mntfromname[:]))
	if !ok {
		return "", ""
	}
	whole := vol
	if vol.ParentWholeDisk != "" {
		if w, ok := diskutil(vol.ParentWholeDisk); ok {
			whole = w
		}
	}
	media := whole.MediaName
	if media == "" {
		media = vol.MediaName
	}
	name = strings.TrimSuffix(whole.IORegistryEntryName, " Media")
	if name == "" || strings.HasPrefix(name, "AppleAPFS") {
		name = media
	}

	switch bus := vol.BusProtocol; {
	case bus == "USB":
		connection = "USB"
		if link := usbLinkSpeed(media); link != "" {
			connection = usbGeneration(link)
		}
	case vol.Internal && (bus == "Apple Fabric" || bus == "PCI-Express" || bus == "PCI" || bus == "NVMe"):
		connection = "Internal SSD"
	case vol.Internal:
		connection = "Internal (" + bus + ")"
	default:
		connection = bus
	}
	return name, connection
}

// usbLinkSpeed finds the USB device with the given product name and returns its
// link speed, e.g. "10 Gb/s".
func usbLinkSpeed(product string) string {
	if product == "" {
		return ""
	}
	out, err := exec.Command("system_profiler", "SPUSBHostDataType", "-json").Output()
	if err != nil {
		return ""
	}
	var root any
	if json.Unmarshal(out, &root) != nil {
		return ""
	}
	var find func(any) string
	find = func(x any) string {
		switch v := x.(type) {
		case map[string]any:
			if n, _ := v["_name"].(string); n == product {
				if s, _ := v["USBDeviceKeyLinkSpeed"].(string); s != "" {
					return s
				}
			}
			for _, c := range v {
				if s := find(c); s != "" {
					return s
				}
			}
		case []any:
			for _, c := range v {
				if s := find(c); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return find(root)
}

func cString(b []int8) string {
	var s strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		s.WriteByte(byte(c))
	}
	return s.String()
}
