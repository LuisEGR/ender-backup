//go:build !(darwin && cgo)

package main

// Elsewhere (Windows, Linux, or a macOS build without cgo) the pure-Go
// zeroconf responder in pair.go is used.
func registerSystemBonjour(instance string, port int, txt []string) (advertiser, error) {
	return nil, errNoSystemBonjour
}
