//go:build darwin && cgo

package main

/*
#include <dns_sd.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// On macOS, register through the system's mDNSResponder instead of running a
// second responder on port 5353: the system answers queries, resolves name
// conflicts, and withdraws the service if this process dies.
type systemAdvertiser struct {
	ref C.DNSServiceRef
}

func txtRecord(txt []string) []byte {
	var b []byte
	for _, t := range txt {
		if len(t) > 255 {
			t = t[:255]
		}
		b = append(b, byte(len(t)))
		b = append(b, t...)
	}
	return b
}

func registerSystemBonjour(instance string, port int, txt []string) (advertiser, error) {
	name := C.CString(instance)
	defer C.free(unsafe.Pointer(name))
	regType := C.CString(bonjourService)
	defer C.free(unsafe.Pointer(regType))

	rec := txtRecord(txt)
	p := uint16(port)
	netPort := C.uint16_t(p>>8 | p<<8) // network byte order

	var ref C.DNSServiceRef
	// A nil callback is allowed: we don't need to hear back, and the daemon keeps
	// the registration for as long as our connection (this process) is alive.
	if code := C.DNSServiceRegister(&ref, 0, 0, name, regType, nil, nil, netPort,
		C.uint16_t(len(rec)), unsafe.Pointer(&rec[0]), nil, nil); code != C.kDNSServiceErr_NoError {
		return nil, fmt.Errorf("DNSServiceRegister error %d", int(code))
	}
	return &systemAdvertiser{ref: ref}, nil
}

func (a *systemAdvertiser) SetText(txt []string) {
	rec := txtRecord(txt)
	C.DNSServiceUpdateRecord(a.ref, nil, 0, C.uint16_t(len(rec)), unsafe.Pointer(&rec[0]), 0)
}

func (a *systemAdvertiser) Shutdown() {
	C.DNSServiceRefDeallocate(a.ref)
}
