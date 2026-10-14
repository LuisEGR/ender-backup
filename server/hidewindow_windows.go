package main

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a helper process from opening a console window (the server itself may
// run without one, started hidden at login).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// lowPriorityChild also runs it below normal priority (photo/video compression).
func lowPriorityChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00004000} // + BELOW_NORMAL_PRIORITY_CLASS
}
