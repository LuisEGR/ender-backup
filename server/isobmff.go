package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Minimal ISO-BMFF (HEIF/AVIF container) handling: enough to read the Exif item of a
// HEIC and to add one to an AVIF, which the AVIF encoder can't do itself.

type bmffBox struct {
	typ          string
	start, end   int // whole box, in the parent buffer
	payloadStart int // after the size/type header
}

// parseBoxes splits data into its top-level boxes.
func parseBoxes(data []byte) ([]bmffBox, error) {
	var boxes []bmffBox
	for pos := 0; pos < len(data); {
		if pos+8 > len(data) {
			return nil, errors.New("truncated box header")
		}
		size := int(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		header := 8
		switch size {
		case 1:
			if pos+16 > len(data) {
				return nil, errors.New("truncated large box")
			}
			size = int(binary.BigEndian.Uint64(data[pos+8:]))
			header = 16
		case 0:
			size = len(data) - pos
		}
		if size < header || pos+size > len(data) {
			return nil, fmt.Errorf("bad size for box %q", typ)
		}
		boxes = append(boxes, bmffBox{typ: typ, start: pos, end: pos + size, payloadStart: pos + header})
		pos += size
	}
	return boxes, nil
}

func findBox(boxes []bmffBox, typ string) (bmffBox, bool) {
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return bmffBox{}, false
}

// ilocEntry is one item's location. Offsets are absolute file offsets when
// construction method is 0, or relative to the meta box's idat when it is 1.
type ilocEntry struct {
	itemID             uint32
	constructionMethod uint16
	dataRefIndex       uint16
	baseOffset         uint64
	extents            []ilocExtent
}

type ilocExtent struct {
	index, offset, length uint64
}

type ilocBox struct {
	version                                         byte
	offsetSize, lengthSize, baseOffsetSize, idxSize int
	entries                                         []ilocEntry
}

type byteReader struct {
	b   []byte
	pos int
	err error
}

func (r *byteReader) n(size int) uint64 {
	if r.err != nil {
		return 0
	}
	if size == 0 {
		return 0
	}
	if r.pos+size > len(r.b) {
		r.err = errors.New("truncated")
		return 0
	}
	var v uint64
	for i := 0; i < size; i++ {
		v = v<<8 | uint64(r.b[r.pos+i])
	}
	r.pos += size
	return v
}

func parseIloc(payload []byte) (*ilocBox, error) {
	r := &byteReader{b: payload}
	version := byte(r.n(1))
	r.n(3) // flags
	sizes := r.n(2)
	box := &ilocBox{
		version:        version,
		offsetSize:     int(sizes >> 12 & 0xF),
		lengthSize:     int(sizes >> 8 & 0xF),
		baseOffsetSize: int(sizes >> 4 & 0xF),
	}
	if version == 1 || version == 2 {
		box.idxSize = int(sizes & 0xF)
	}
	var count uint64
	if version < 2 {
		count = r.n(2)
	} else {
		count = r.n(4)
	}
	for i := uint64(0); i < count && r.err == nil; i++ {
		var e ilocEntry
		if version < 2 {
			e.itemID = uint32(r.n(2))
		} else {
			e.itemID = uint32(r.n(4))
		}
		if version == 1 || version == 2 {
			e.constructionMethod = uint16(r.n(2) & 0xF)
		}
		e.dataRefIndex = uint16(r.n(2))
		e.baseOffset = r.n(box.baseOffsetSize)
		extents := r.n(2)
		for j := uint64(0); j < extents && r.err == nil; j++ {
			var x ilocExtent
			if box.idxSize > 0 {
				x.index = r.n(box.idxSize)
			}
			x.offset = r.n(box.offsetSize)
			x.length = r.n(box.lengthSize)
			e.extents = append(e.extents, x)
		}
		box.entries = append(box.entries, e)
	}
	return box, r.err
}

func putN(buf *bytes.Buffer, v uint64, size int) {
	for i := size - 1; i >= 0; i-- {
		buf.WriteByte(byte(v >> (8 * uint(i))))
	}
}

// bytes serializes the iloc box, widening offset/length fields to 4 bytes if needed.
func (b *ilocBox) bytes() []byte {
	if b.offsetSize < 4 {
		b.offsetSize = 4
	}
	if b.lengthSize < 4 {
		b.lengthSize = 4
	}
	var p bytes.Buffer
	p.WriteByte(b.version)
	p.Write([]byte{0, 0, 0})
	putN(&p, uint64(b.offsetSize<<12|b.lengthSize<<8|b.baseOffsetSize<<4|b.idxSize), 2)
	if b.version < 2 {
		putN(&p, uint64(len(b.entries)), 2)
	} else {
		putN(&p, uint64(len(b.entries)), 4)
	}
	for _, e := range b.entries {
		if b.version < 2 {
			putN(&p, uint64(e.itemID), 2)
		} else {
			putN(&p, uint64(e.itemID), 4)
		}
		if b.version == 1 || b.version == 2 {
			putN(&p, uint64(e.constructionMethod), 2)
		}
		putN(&p, uint64(e.dataRefIndex), 2)
		putN(&p, e.baseOffset, b.baseOffsetSize)
		putN(&p, uint64(len(e.extents)), 2)
		for _, x := range e.extents {
			if b.idxSize > 0 {
				putN(&p, x.index, b.idxSize)
			}
			putN(&p, x.offset, b.offsetSize)
			putN(&p, x.length, b.lengthSize)
		}
	}
	return wrapBox("iloc", p.Bytes())
}

func wrapBox(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

// infeItem is an item declared in iinf: its ID and 4-character type.
type infeItem struct {
	id  uint32
	typ string
}

// parseIinf returns the declared items and the raw infe boxes.
func parseIinf(payload []byte) ([]infeItem, [][]byte, error) {
	if len(payload) < 4 {
		return nil, nil, errors.New("short iinf")
	}
	version := payload[0]
	pos := 4
	if version == 0 {
		pos += 2
	} else {
		pos += 4
	}
	if pos > len(payload) {
		return nil, nil, errors.New("short iinf")
	}
	boxes, err := parseBoxes(payload[pos:])
	if err != nil {
		return nil, nil, err
	}
	var items []infeItem
	var raw [][]byte
	for _, b := range boxes {
		body := payload[pos+b.payloadStart : pos+b.end]
		raw = append(raw, payload[pos+b.start:pos+b.end])
		if b.typ != "infe" || len(body) < 4 {
			continue
		}
		r := &byteReader{b: body}
		v := byte(r.n(1))
		r.n(3)
		var it infeItem
		switch {
		case v >= 2:
			if v == 2 {
				it.id = uint32(r.n(2))
			} else {
				it.id = uint32(r.n(4))
			}
			r.n(2) // protection index
			if r.pos+4 <= len(body) {
				it.typ = string(body[r.pos : r.pos+4])
			}
		default:
			it.id = uint32(r.n(2))
		}
		items = append(items, it)
	}
	return items, raw, nil
}

// heicExif returns the TIFF-format EXIF block stored as an "Exif" item in a HEIF file.
func heicExif(data []byte) ([]byte, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return nil, err
	}
	meta, ok := findBox(top, "meta")
	if !ok {
		return nil, errors.New("no meta box")
	}
	metaChildren, err := parseBoxes(data[meta.payloadStart+4 : meta.end])
	if err != nil {
		return nil, err
	}
	base := meta.payloadStart + 4
	iinf, ok1 := findBox(metaChildren, "iinf")
	ilocB, ok2 := findBox(metaChildren, "iloc")
	if !ok1 || !ok2 {
		return nil, errors.New("no iinf/iloc")
	}
	items, _, err := parseIinf(data[base+iinf.payloadStart : base+iinf.end])
	if err != nil {
		return nil, err
	}
	var exifID uint32
	for _, it := range items {
		if it.typ == "Exif" {
			exifID = it.id
		}
	}
	if exifID == 0 {
		return nil, errors.New("no Exif item")
	}
	iloc, err := parseIloc(data[base+ilocB.payloadStart : base+ilocB.end])
	if err != nil {
		return nil, err
	}
	var idat []byte
	if b, ok := findBox(metaChildren, "idat"); ok {
		idat = data[base+b.payloadStart : base+b.end]
	}
	for _, e := range iloc.entries {
		if e.itemID != exifID {
			continue
		}
		src := data
		if e.constructionMethod == 1 {
			src = idat
		}
		var item []byte
		for _, x := range e.extents {
			start := e.baseOffset + x.offset
			if start+x.length > uint64(len(src)) {
				return nil, errors.New("Exif item out of range")
			}
			item = append(item, src[start:start+x.length]...)
		}
		if len(item) < 4 {
			return nil, errors.New("short Exif item")
		}
		// The item starts with the offset of the TIFF header within the rest.
		skip := int(binary.BigEndian.Uint32(item)) + 4
		if skip > len(item) {
			return nil, errors.New("bad Exif header offset")
		}
		return item[skip:], nil
	}
	return nil, errors.New("Exif item has no location")
}

// addExifToHEIF adds a TIFF-format EXIF block to an AVIF/HEIF file as an "Exif" item
// describing the primary image. The payload goes in a new mdat at the end of the file;
// offsets of existing items after the meta box are shifted by how much meta grew.
func addExifToHEIF(data, tiff []byte) ([]byte, error) {
	top, err := parseBoxes(data)
	if err != nil {
		return nil, err
	}
	meta, ok := findBox(top, "meta")
	if !ok {
		return nil, errors.New("no meta box")
	}
	base := meta.payloadStart + 4
	children, err := parseBoxes(data[base:meta.end])
	if err != nil {
		return nil, err
	}
	pitm, ok := findBox(children, "pitm")
	if !ok {
		return nil, errors.New("no primary item")
	}
	pr := &byteReader{b: data[base+pitm.payloadStart : base+pitm.end]}
	pv := byte(pr.n(1))
	pr.n(3)
	primary := uint32(pr.n(2))
	if pv != 0 {
		pr.pos -= 2
		primary = uint32(pr.n(4))
	}
	iinfB, ok1 := findBox(children, "iinf")
	ilocB, ok2 := findBox(children, "iloc")
	if !ok1 || !ok2 {
		return nil, errors.New("no iinf/iloc")
	}
	iinfPayload := data[base+iinfB.payloadStart : base+iinfB.end]
	items, rawInfes, err := parseIinf(iinfPayload)
	if err != nil {
		return nil, err
	}
	var newID uint32 = 1
	for _, it := range items {
		if it.id >= newID {
			newID = it.id + 1
		}
		if it.typ == "Exif" {
			return nil, errors.New("already has Exif")
		}
	}
	if newID > 0xFFFF {
		return nil, errors.New("too many items")
	}
	iloc, err := parseIloc(data[base+ilocB.payloadStart : base+ilocB.end])
	if err != nil {
		return nil, err
	}

	// New iinf: same version, one more infe (version 2, 16-bit ID, type "Exif").
	var iinf bytes.Buffer
	iinf.Write(iinfPayload[:4])
	if iinfPayload[0] == 0 {
		putN(&iinf, uint64(len(rawInfes)+1), 2)
	} else {
		putN(&iinf, uint64(len(rawInfes)+1), 4)
	}
	for _, b := range rawInfes {
		iinf.Write(b)
	}
	var infe bytes.Buffer
	infe.Write([]byte{2, 0, 0, 0})
	putN(&infe, uint64(newID), 2)
	putN(&infe, 0, 2)
	infe.WriteString("Exif")
	infe.WriteByte(0) // empty item name
	iinf.Write(wrapBox("infe", infe.Bytes()))
	newIinf := wrapBox("iinf", iinf.Bytes())

	// iref: "the Exif item describes (cdsc) the primary image".
	var ref bytes.Buffer
	putN(&ref, uint64(newID), 2)
	putN(&ref, 1, 2)
	putN(&ref, uint64(primary), 2)
	cdsc := wrapBox("cdsc", ref.Bytes())
	var newIref []byte
	if irefB, ok := findBox(children, "iref"); ok {
		old := data[base+irefB.payloadStart : base+irefB.end]
		if len(old) < 4 || old[0] != 0 {
			return nil, errors.New("unsupported iref version")
		}
		newIref = wrapBox("iref", append(append([]byte{}, old...), cdsc...))
	} else {
		newIref = wrapBox("iref", append([]byte{0, 0, 0, 0}, cdsc...))
	}

	payload := make([]byte, 4, 4+len(tiff)) // exif_tiff_header_offset = 0
	payload = append(payload, tiff...)

	build := func(exifOffset uint64, delta int64) []byte {
		loc := *iloc
		loc.entries = nil
		for _, e := range iloc.entries {
			if e.constructionMethod == 0 && delta != 0 {
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
		loc.entries = append(loc.entries, ilocEntry{
			itemID:  newID,
			extents: []ilocExtent{{offset: exifOffset, length: uint64(len(payload))}},
		})
		var m bytes.Buffer
		m.Write(data[meta.payloadStart:base]) // meta's version/flags
		wroteIref := false
		for _, c := range children {
			switch c.typ {
			case "iinf":
				m.Write(newIinf)
				if _, has := findBox(children, "iref"); !has {
					m.Write(newIref)
					wroteIref = true
				}
			case "iloc":
				m.Write(loc.bytes())
			case "iref":
				if !wroteIref {
					m.Write(newIref)
					wroteIref = true
				}
			default:
				m.Write(data[base+c.start : base+c.end])
			}
		}
		return wrapBox("meta", m.Bytes())
	}

	// Items stored after meta move by however much meta grows; the Exif payload goes
	// into a new mdat at the very end.
	first := build(0, 0)
	delta := int64(len(first)) - int64(meta.end-meta.start)
	for _, e := range iloc.entries {
		if e.constructionMethod != 0 {
			continue
		}
		for _, x := range e.extents {
			if e.baseOffset+x.offset < uint64(meta.end) {
				return nil, errors.New("item data before meta box (unsupported layout)")
			}
		}
	}
	newLen := int64(len(data)) + delta
	newMeta := build(uint64(newLen+8), delta)

	out := make([]byte, 0, int(newLen)+8+len(payload))
	out = append(out, data[:meta.start]...)
	out = append(out, newMeta...)
	out = append(out, data[meta.end:]...)
	out = append(out, wrapBox("mdat", payload)...)
	return out, nil
}
