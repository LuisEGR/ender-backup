//go:build !windows

package main

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

// lowPriorityChild: on macOS and Linux the command is started through `nice` instead.
func lowPriorityChild(cmd *exec.Cmd) {}
