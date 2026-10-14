package main

import "golang.org/x/sys/windows"

// setLowPriority lets other programs go first when the CPU is busy (photo conversion).
func setLowPriority() {
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
}
