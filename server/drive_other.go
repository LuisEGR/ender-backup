//go:build !darwin && !windows && !linux

package main

func describeDrive(path string) (name, connection string) { return "", "" }
