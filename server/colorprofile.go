package main

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Colour profiles: iPhone photos are Display P3, a wider colour space than sRGB. The
// converted pixels keep P3 values, so the AVIF must say so, or viewers assume sRGB and
// colours look duller.

// jpegICC returns the ICC colour profile of a JPEG, which is split across APP2
// "ICC_PROFILE" segments numbered 1..n.
func jpegICC(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errors.New("not a JPEG")
	}
	chunks := map[int][]byte{}
	total := 0
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xFF {
			break
		}
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 { // image data starts
			break
		}
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		if pos+2+size > len(data) {
			break
		}
		seg := data[pos+4 : pos+2+size]
		if marker == 0xE2 && len(seg) > 14 && string(seg[:12]) == "ICC_PROFILE\x00" {
			chunks[int(seg[12])] = seg[14:]
			total = int(seg[13])
		}
		pos += 2 + size
	}
	if total == 0 {
		return nil, errors.New("no ICC profile")
	}
	var icc []byte
	for i := 1; i <= total; i++ {
		c, ok := chunks[i]
		if !ok {
			return nil, errors.New("incomplete ICC profile")
		}
		icc = append(icc, c...)
	}
	return icc, nil
}

// heifMeta is a HEIF file's meta box and its child boxes.
type heifMeta struct {
	meta     bmffBox
	base     int // offset of meta's children in the file
	children []bmffBox
}

func parseHEIFMeta(data []byte) (*heifMeta, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return nil, err
	}
	meta, ok := findBox(top, "meta")
	if !ok {
		return nil, errors.New("no meta box")
	}
	base := meta.payloadStart + 4 // after version/flags
	children, err := parseBoxes(data[base:meta.end])
	if err != nil {
		return nil, err
	}
	return &heifMeta{meta: meta, base: base, children: children}, nil
}

func (h *heifMeta) child(data []byte, typ string) ([]byte, bool) {
	b, ok := findBox(h.children, typ)
	if !ok {
		return nil, false
	}
	return data[h.base+b.payloadStart : h.base+b.end], true
}

// heicICC returns the ICC colour profile of a HEIF file.
func heicICC(data []byte) ([]byte, error) {
	h, err := parseHEIFMeta(data)
	if err != nil {
		return nil, err
	}
	iprp, ok := h.child(data, "iprp")
	if !ok {
		return nil, errors.New("no iprp")
	}
	parts, err := parseBoxes(iprp)
	if err != nil {
		return nil, err
	}
	ipco, ok := findBox(parts, "ipco")
	if !ok {
		return nil, errors.New("no ipco")
	}
	ipcoData := iprp[ipco.payloadStart:ipco.end]
	props, err := parseBoxes(ipcoData)
	if err != nil {
		return nil, err
	}
	for _, p := range props {
		body := ipcoData[p.payloadStart:p.end]
		if p.typ == "colr" && len(body) > 4 && (string(body[:4]) == "prof" || string(body[:4]) == "rICC") {
			return body[4:], nil
		}
	}
	return nil, errors.New("no ICC profile")
}

// addICCToHEIF attaches an ICC colour profile to the primary image of an AVIF/HEIF file.
func addICCToHEIF(data, icc []byte) ([]byte, error) {
	h, err := parseHEIFMeta(data)
	if err != nil {
		return nil, err
	}
	pitm, ok1 := h.child(data, "pitm")
	iprp, ok2 := h.child(data, "iprp")
	ilocData, ok3 := h.child(data, "iloc")
	if !ok1 || !ok2 || !ok3 {
		return nil, errors.New("missing pitm/iprp/iloc")
	}
	pr := &byteReader{b: pitm}
	pv := byte(pr.n(1))
	pr.n(3)
	var primary uint32
	if pv == 0 {
		primary = uint32(pr.n(2))
	} else {
		primary = uint32(pr.n(4))
	}

	parts, err := parseBoxes(iprp)
	if err != nil {
		return nil, err
	}
	ipco, ok := findBox(parts, "ipco")
	if !ok {
		return nil, errors.New("no ipco")
	}
	ipcoData := iprp[ipco.payloadStart:ipco.end]
	props, err := parseBoxes(ipcoData)
	if err != nil {
		return nil, err
	}
	for _, p := range props {
		body := ipcoData[p.payloadStart:p.end]
		if p.typ == "colr" && len(body) >= 4 && string(body[:4]) != "nclx" {
			return nil, errors.New("already has a colour profile")
		}
	}
	newIndex := len(props) + 1 // property indexes are 1-based
	colr := wrapBox("colr", append([]byte("prof"), icc...))
	newIpco := wrapBox("ipco", append(append([]byte{}, ipcoData...), colr...))

	// ipma: add the new property to the primary item's associations.
	var newIpma []byte
	for _, p := range parts {
		if p.typ != "ipma" {
			continue
		}
		body := iprp[p.payloadStart:p.end]
		r := &byteReader{b: body}
		version := byte(r.n(1))
		wide := r.n(3)&1 == 1
		count := r.n(4)
		if r.err != nil {
			return nil, errors.New("bad ipma")
		}
		size := 1
		if wide {
			size = 2
		} else if newIndex > 127 {
			return nil, errors.New("too many properties")
		}
		var out bytes.Buffer
		out.Write(body[:8])
		found := false
		for i := uint64(0); i < count; i++ {
			idSize := 2
			if version >= 1 {
				idSize = 4
			}
			id := uint32(r.n(idSize))
			n := int(r.n(1))
			if r.err != nil || r.pos+n*size > len(body) {
				return nil, errors.New("bad ipma")
			}
			assoc := body[r.pos : r.pos+n*size]
			r.pos += n * size
			putN(&out, uint64(id), idSize)
			if id == primary {
				found = true
				out.WriteByte(byte(n + 1))
				out.Write(assoc)
				putN(&out, uint64(newIndex), size) // not marked essential
			} else {
				out.WriteByte(byte(n))
				out.Write(assoc)
			}
		}
		if !found {
			return nil, errors.New("primary item has no properties")
		}
		newIpma = wrapBox("ipma", out.Bytes())
	}
	if newIpma == nil {
		return nil, errors.New("no ipma")
	}
	var iprpOut bytes.Buffer
	for _, p := range parts {
		switch p.typ {
		case "ipco":
			iprpOut.Write(newIpco)
		case "ipma":
			iprpOut.Write(newIpma)
		default:
			iprpOut.Write(iprp[p.start:p.end])
		}
	}
	newIprp := wrapBox("iprp", iprpOut.Bytes())

	iloc, err := parseIloc(ilocData)
	if err != nil {
		return nil, err
	}
	for _, e := range iloc.entries {
		for _, x := range e.extents {
			if e.constructionMethod == 0 && e.baseOffset+x.offset < uint64(h.meta.end) {
				return nil, errors.New("item data before meta box (unsupported layout)")
			}
		}
	}
	// Image data after the meta box moves by however much meta grows.
	build := func(delta int64) []byte {
		loc := *iloc
		loc.entries = nil
		for _, e := range iloc.entries {
			if e.constructionMethod == 0 {
				e.extents = append([]ilocExtent{}, e.extents...)
				if e.baseOffset > 0 {
					e.baseOffset = uint64(int64(e.baseOffset) + delta)
				} else {
					for i := range e.extents {
						e.extents[i].offset = uint64(int64(e.extents[i].offset) + delta)
					}
				}
			}
			loc.entries = append(loc.entries, e)
		}
		var m bytes.Buffer
		m.Write(data[h.meta.payloadStart:h.base]) // version/flags
		for _, c := range h.children {
			switch c.typ {
			case "iprp":
				m.Write(newIprp)
			case "iloc":
				m.Write(loc.bytes())
			default:
				m.Write(data[h.base+c.start : h.base+c.end])
			}
		}
		return wrapBox("meta", m.Bytes())
	}
	oldLen := int64(h.meta.end - h.meta.start)
	delta := int64(len(build(0))) - oldLen
	newMeta := build(delta)
	if int64(len(newMeta))-oldLen != delta {
		return nil, errors.New("meta size changed unexpectedly")
	}
	out := make([]byte, 0, len(data)+int(delta))
	out = append(out, data[:h.meta.start]...)
	out = append(out, newMeta...)
	return append(out, data[h.meta.end:]...), nil
}
