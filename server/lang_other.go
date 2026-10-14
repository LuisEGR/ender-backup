//go:build !windows && !darwin

package main

// osLanguage: on Linux and the like, the LANG variables (checked by systemLanguage) are it.
func osLanguage() string { return "" }
