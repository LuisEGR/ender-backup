//go:build darwin || linux

package main

import (
	"os"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// setLowPriority lets other programs go first when the CPU is busy (photo conversion).
func setLowPriority() {
	_ = unix.Setpriority(unix.PRIO_PROCESS, 0, 10)
	if runtime.GOOS == "linux" {
		// On Linux the nice value is per thread: lower every existing thread (new ones
		// inherit it from the thread that creates them).
		if tasks, err := os.ReadDir("/proc/self/task"); err == nil {
			for _, t := range tasks {
				if tid, err := strconv.Atoi(t.Name()); err == nil {
					_ = unix.Setpriority(unix.PRIO_PROCESS, tid, 10)
				}
			}
		}
	}
}
