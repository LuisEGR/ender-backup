package main

import (
	"os/exec"
	"strings"
)

// osLanguage: the first of macOS's preferred languages, e.g. "es-MX".
func osLanguage() string {
	out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return ""
	}
	// (\n    "es-MX",\n    "en-US"\n)
	for _, f := range strings.Fields(string(out)) {
		f = strings.Trim(f, `(),"`)
		if f != "" {
			return f
		}
	}
	return ""
}
