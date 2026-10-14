package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

var errCancelled = errors.New("cancelled")

// friendlyName is the computer's user-facing name ("MacBook Air de Luis"), falling
// back to the hostname without its domain.
func friendlyName() string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				return name
			}
		}
	}
	host, _ := os.Hostname()
	if host = strings.Split(host, ".")[0]; host == "" {
		host = "Computer"
	}
	return host
}

// pickFolder shows the operating system's native folder chooser, starting at start.
func pickFolder(start, lang string) (string, error) {
	if st, err := os.Stat(start); err != nil || !st.IsDir() {
		start, _ = os.UserHomeDir()
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// A TopMost owner window keeps the dialog in front of the browser.
		const script = `Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true; ShowInTaskbar = $false}
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = 'Choose where to save photos and videos'
$d.ShowNewFolderButton = $true
$d.SelectedPath = $env:PB_START
if ($d.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { $d.SelectedPath } else { exit 1 }`
		cmd = exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script)
	case "darwin":
		cmd = exec.Command("osascript",
			"-e", `activate`,
			"-e", `POSIX path of (choose folder with prompt (system attribute "PB_TITLE") default location (POSIX file (system attribute "PB_START")))`)
	default:
		if _, err := exec.LookPath("zenity"); err != nil {
			return "", errors.New("no folder picker available (install zenity) — type the path instead")
		}
		cmd = exec.Command("zenity", "--file-selection", "--directory",
			"--title="+tr(lang, "Choose where to save photos and videos"), "--filename="+start+"/")
	}
	cmd.Env = append(os.Environ(), "PB_START="+start, "PB_TITLE="+tr(lang, "Choose where to save photos and videos"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		// Cancelling exits with status 1 on all three (osascript reports error -128).
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", errCancelled
		}
		return "", fmt.Errorf("folder picker failed: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	path := strings.TrimSpace(stdout.String())
	if len(path) > 1 {
		path = strings.TrimRight(path, `/`)
	}
	if path == "" {
		return "", errCancelled
	}
	return path, nil
}

// askApproval shows a native Approve/Deny dialog and reports the choice. It returns an
// error if no answer was given: the dialog couldn't be shown, or ctx ended (timeout, or
// the request was answered on the settings page), which also closes the dialog.
func askApproval(ctx context.Context, message, lang string) (bool, error) {
	approveLabel, denyLabel := tr(lang, "Approve"), tr(lang, "Deny")
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "osascript",
			"-e", `on run argv`,
			"-e", `activate`,
			"-e", `set r to display dialog (item 1 of argv) with title "Ender Backup" buttons {item 3 of argv, item 2 of argv} default button (item 2 of argv) with icon caution`,
			"-e", `if button returned of r is (item 2 of argv) then return "Approve"`,
			"-e", `return "Deny"`,
			"-e", `end run`,
			message, approveLabel, denyLabel)
	case "windows":
		const script = `Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true; ShowInTaskbar = $false}
$msg = $env:PB_MSG + [Environment]::NewLine + [Environment]::NewLine + $env:PB_HINT
$r = [System.Windows.Forms.MessageBox]::Show($owner, $msg, 'Ender Backup', 'YesNo', 'Question')
if ($r -eq 'Yes') { 'Approve' } else { 'Deny' }`
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", script)
	default:
		if _, err := exec.LookPath("zenity"); err != nil {
			return false, errors.New("zenity not installed")
		}
		cmd = exec.CommandContext(ctx, "zenity", "--question", "--title=Ender Backup",
			"--text="+message, "--ok-label="+approveLabel, "--cancel-label="+denyLabel)
	}
	cmd.Env = append(os.Environ(), "PB_MSG="+message, "PB_HINT="+tr(lang, "Yes = Approve, No = Deny"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		// zenity exits 1 for Deny.
		if runtime.GOOS == "linux" && errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("%v %s", err, strings.TrimSpace(stderr.String()))
	}
	if runtime.GOOS == "linux" {
		return true, nil // zenity exits 0 for Approve
	}
	return strings.TrimSpace(stdout.String()) == "Approve", nil
}

// openInFileManager shows a folder in Explorer / Finder / the desktop's file manager.
func openInFileManager(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer.exe", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
