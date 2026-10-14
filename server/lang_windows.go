package main

import "syscall"

// osLanguage: Windows' display language, as a short tag ("es", "en", …).
func osLanguage() string {
	id, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage").Call()
	// The low 10 bits are the primary language (LANG_ENGLISH 0x09, LANG_SPANISH 0x0A, …).
	switch id & 0x3FF {
	case 0x0A:
		return "es"
	case 0x09:
		return "en"
	}
	return ""
}
