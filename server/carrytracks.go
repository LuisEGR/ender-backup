package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Copying what ffmpeg can't into an AV1 MP4: the original's timed metadata tracks (DJI
// flight telemetry with the drone's model and firmware, Apple's metadata tracks) and its
// user data block (DJI tags, cover image). MP4 holds them fine; only ffmpeg's muxer
// refuses them. The tracks' samples are copied byte for byte into a new mdat after the
// AV1 file's, their chunk offsets rewritten, track IDs renumbered and references to the
// video/audio tracks pointed at the new ones.

// readTopBox finds a top-level box by type and returns its bytes and offset.
func readTopBox(f *os.File, typ string) ([]byte, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	var off int64
	hdr := make([]byte, 16)
	for off+8 <= st.Size() {
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return nil, 0, err
		}
		size := int64(binary.BigEndian.Uint32(hdr))
		name := string(hdr[4:8])
		if size == 1 {
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return nil, 0, err
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
		} else if size == 0 {
			size = st.Size() - off
		}
		if size < 8 {
			return nil, 0, errors.New("bad box size")
		}
		if name == typ {
			if size > 256<<20 {
				return nil, 0, fmt.Errorf("%s box too large", typ)
			}
			b := make([]byte, size)
			if _, err := f.ReadAt(b, off); err != nil {
				return nil, 0, err
			}
			return b, off, nil
		}
		off += size
	}
	return nil, 0, fmt.Errorf("no %s box", typ)
}

// children returns the child boxes of a container box (b is the whole box).
func children(b []byte, skip int) ([]bmffBox, int, error) {
	start := 8 + skip
	if binary.BigEndian.Uint32(b) == 1 {
		start = 16 + skip
	}
	kids, err := parseBoxes(b[start:])
	return kids, start, err
}

func childBytes(b []byte, typ string) []byte {
	kids, start, err := children(b, 0)
	if err != nil {
		return nil
	}
	for _, k := range kids {
		if k.typ == typ {
			return b[start+k.start : start+k.end]
		}
	}
	return nil
}

// rebuild replaces the child of type typ (or appends it) in container box b.
func rebuild(b []byte, typ string, newChild []byte) []byte {
	kids, start, err := children(b, 0)
	if err != nil {
		return b
	}
	var payload bytes.Buffer
	done := false
	for _, k := range kids {
		if k.typ == typ && !done {
			payload.Write(newChild)
			done = true
		} else {
			payload.Write(b[start+k.start : start+k.end])
		}
	}
	if !done {
		payload.Write(newChild)
	}
	return wrapBox(string(b[4:8]), payload.Bytes())
}

func handlerType(trak []byte) string {
	hdlr := childBytes(childBytes(trak, "mdia"), "hdlr")
	if len(hdlr) < 20 {
		return ""
	}
	return string(hdlr[16:20])
}

func trackID(trak []byte) uint32 {
	tkhd := childBytes(trak, "tkhd")
	if len(tkhd) < 32 {
		return 0
	}
	if tkhd[8] == 0 {
		return binary.BigEndian.Uint32(tkhd[20:])
	}
	return binary.BigEndian.Uint32(tkhd[28:])
}

func movieTimescale(moov []byte) uint32 {
	mvhd := childBytes(moov, "mvhd")
	if len(mvhd) < 32 {
		return 0
	}
	if mvhd[8] == 0 {
		return binary.BigEndian.Uint32(mvhd[20:])
	}
	return binary.BigEndian.Uint32(mvhd[28:])
}

// chunks returns the byte ranges (offset, size) of a track's chunks.
func chunks(stbl []byte) ([][2]int64, error) {
	stsz, stsc := childBytes(stbl, "stsz"), childBytes(stbl, "stsc")
	var offsets []int64
	if stco := childBytes(stbl, "stco"); stco != nil {
		n := int(binary.BigEndian.Uint32(stco[12:]))
		for i := 0; i < n && 16+4*i+4 <= len(stco); i++ {
			offsets = append(offsets, int64(binary.BigEndian.Uint32(stco[16+4*i:])))
		}
	} else if co64 := childBytes(stbl, "co64"); co64 != nil {
		n := int(binary.BigEndian.Uint32(co64[12:]))
		for i := 0; i < n && 16+8*i+8 <= len(co64); i++ {
			offsets = append(offsets, int64(binary.BigEndian.Uint64(co64[16+8*i:])))
		}
	}
	if stsz == nil || stsc == nil || offsets == nil {
		return nil, errors.New("incomplete sample table")
	}
	constSize := int64(binary.BigEndian.Uint32(stsz[12:]))
	nSamples := int(binary.BigEndian.Uint32(stsz[16:]))
	size := func(i int) int64 {
		if constSize != 0 {
			return constSize
		}
		return int64(binary.BigEndian.Uint32(stsz[20+4*i:]))
	}
	type run struct{ first, perChunk int }
	var runs []run
	n := int(binary.BigEndian.Uint32(stsc[12:]))
	for i := 0; i < n; i++ {
		e := stsc[16+12*i:]
		runs = append(runs, run{int(binary.BigEndian.Uint32(e)), int(binary.BigEndian.Uint32(e[4:]))})
	}
	out := make([][2]int64, len(offsets))
	sample := 0
	for c := range offsets {
		per := 0
		for _, r := range runs {
			if c+1 >= r.first {
				per = r.perChunk
			}
		}
		var sz int64
		for j := 0; j < per && sample < nSamples; j++ {
			sz += size(sample)
			sample++
		}
		out[c] = [2]int64{offsets[c], sz}
	}
	return out, nil
}

// patchTrack renumbers a copied track, re-points its references, rescales its
// movie-timescale durations and gives it new chunk offsets (co64).
func patchTrack(trak []byte, newID uint32, idMap map[uint32]uint32, scale func(uint64) uint64, newOffsets []int64) []byte {
	// tkhd: track ID and duration.
	tkhd := append([]byte{}, childBytes(trak, "tkhd")...)
	if tkhd[8] == 0 {
		binary.BigEndian.PutUint32(tkhd[20:], newID)
		binary.BigEndian.PutUint32(tkhd[28:], uint32(scale(uint64(binary.BigEndian.Uint32(tkhd[28:])))))
	} else {
		binary.BigEndian.PutUint32(tkhd[28:], newID)
		binary.BigEndian.PutUint64(tkhd[36:], scale(binary.BigEndian.Uint64(tkhd[36:])))
	}
	trak = rebuild(trak, "tkhd", tkhd)
	// tref: references to other tracks (Apple's point at the video).
	if tref := childBytes(trak, "tref"); tref != nil {
		tref = append([]byte{}, tref...)
		kids, start, _ := children(tref, 0)
		for _, k := range kids {
			for p := start + k.payloadStart; p+4 <= start+k.end; p += 4 {
				if id, ok := idMap[binary.BigEndian.Uint32(tref[p:])]; ok {
					binary.BigEndian.PutUint32(tref[p:], id)
				}
			}
		}
		trak = rebuild(trak, "tref", tref)
	}
	// edts/elst: segment durations are in the movie timescale.
	if edts := childBytes(trak, "edts"); edts != nil {
		if elst := childBytes(edts, "elst"); elst != nil {
			elst = append([]byte{}, elst...)
			v, n := elst[8], int(binary.BigEndian.Uint32(elst[12:]))
			for i := 0; i < n; i++ {
				if v == 0 {
					p := 16 + 12*i
					binary.BigEndian.PutUint32(elst[p:], uint32(scale(uint64(binary.BigEndian.Uint32(elst[p:])))))
				} else {
					p := 16 + 20*i
					binary.BigEndian.PutUint64(elst[p:], scale(binary.BigEndian.Uint64(elst[p:])))
				}
			}
			trak = rebuild(trak, "edts", rebuild(edts, "elst", elst))
		}
	}
	// Chunk offsets: a co64 box replaces stco/co64.
	var co bytes.Buffer
	co.Write([]byte{0, 0, 0, 0})
	binary.Write(&co, binary.BigEndian, uint32(len(newOffsets)))
	for _, o := range newOffsets {
		binary.Write(&co, binary.BigEndian, uint64(o))
	}
	co64 := wrapBox("co64", co.Bytes())
	mdia := childBytes(trak, "mdia")
	minf := childBytes(mdia, "minf")
	stbl := childBytes(minf, "stbl")
	kids, start, _ := children(stbl, 0)
	var payload bytes.Buffer
	for _, k := range kids {
		switch k.typ {
		case "stco", "co64":
			payload.Write(co64)
		default:
			payload.Write(stbl[start+k.start : start+k.end])
		}
	}
	stbl = wrapBox("stbl", payload.Bytes())
	return rebuild(trak, "mdia", rebuild(mdia, "minf", rebuild(minf, "stbl", stbl)))
}

// carryTracks copies the original's metadata tracks and user data into the AV1 MP4 at
// out (whose moov must be its last box) and lists what it carried.
func carryTracks(origPath, out string) ([]string, error) {
	orig, err := os.Open(origPath)
	if err != nil {
		return nil, err
	}
	defer orig.Close()
	oMoov, _, err := readTopBox(orig, "moov")
	if err != nil {
		return nil, err
	}
	dst, err := os.OpenFile(out, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer dst.Close()
	nMoov, nOff, err := readTopBox(dst, "moov")
	if err != nil {
		return nil, err
	}
	if st, _ := dst.Stat(); nOff+int64(len(nMoov)) != st.Size() {
		return nil, errors.New("the copy's moov isn't at the end")
	}

	oKids, oStart, err := children(oMoov, 0)
	if err != nil {
		return nil, err
	}
	nKids, nStart, err := children(nMoov, 0)
	if err != nil {
		return nil, err
	}
	// Map the original's video/audio track IDs to the copy's.
	firstOf := func(moov []byte, kids []bmffBox, start int, handler string) uint32 {
		for _, k := range kids {
			if k.typ == "trak" && handlerType(moov[start+k.start:start+k.end]) == handler {
				return trackID(moov[start+k.start : start+k.end])
			}
		}
		return 0
	}
	idMap := map[uint32]uint32{}
	for _, h := range []string{"vide", "soun"} {
		if o, n := firstOf(oMoov, oKids, oStart, h), firstOf(nMoov, nKids, nStart, h); o != 0 && n != 0 {
			idMap[o] = n
		}
	}
	var nextID uint32 = 1
	for _, k := range nKids {
		if k.typ == "trak" {
			nextID = max(nextID, trackID(nMoov[nStart+k.start:nStart+k.end])+1)
		}
	}
	oScale, nScale := movieTimescale(oMoov), movieTimescale(nMoov)
	scale := func(v uint64) uint64 {
		if oScale == 0 || nScale == 0 || v == 0xFFFFFFFF || v == 0xFFFFFFFFFFFFFFFF {
			return v
		}
		return v * uint64(nScale) / uint64(oScale)
	}

	// The metadata tracks and their chunks.
	type carried struct {
		trak   []byte
		chunks [][2]int64
	}
	var tracks []carried
	var dataSize int64
	for _, k := range oKids {
		if k.typ != "trak" {
			continue
		}
		trak := oMoov[oStart+k.start : oStart+k.end]
		if handlerType(trak) != "meta" {
			continue
		}
		stbl := childBytes(childBytes(childBytes(trak, "mdia"), "minf"), "stbl")
		cs, err := chunks(stbl)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			dataSize += c[1]
		}
		tracks = append(tracks, carried{trak, cs})
	}
	oUdta := childBytes(oMoov, "udta")
	if len(tracks) == 0 && oUdta == nil {
		return nil, nil
	}

	// Rewrite the file's end: [everything before moov][mdat with the tracks' data][moov].
	if err := dst.Truncate(nOff); err != nil {
		return nil, err
	}
	pos := nOff
	var hdr bytes.Buffer
	binary.Write(&hdr, binary.BigEndian, uint32(1))
	hdr.WriteString("mdat")
	binary.Write(&hdr, binary.BigEndian, uint64(16+dataSize))
	if _, err := dst.WriteAt(hdr.Bytes(), pos); err != nil {
		return nil, err
	}
	pos += 16
	var newTraks [][]byte
	var what []string
	for i, tr := range tracks {
		offsets := make([]int64, len(tr.chunks))
		for j, c := range tr.chunks {
			offsets[j] = pos
			if _, err := io.Copy(io.NewOffsetWriter(dst, pos), io.NewSectionReader(orig, c[0], c[1])); err != nil {
				return nil, err
			}
			pos += c[1]
		}
		newTraks = append(newTraks, patchTrack(tr.trak, nextID+uint32(i), idMap, scale, offsets))
		what = append(what, describeTrack(tr.trak))
	}

	// New moov: the copy's boxes, the carried tracks, next_track_ID, and the original's
	// user data (its entries win; the copy's others are kept).
	lastTrak := -1
	for i, k := range nKids {
		if k.typ == "trak" {
			lastTrak = i
		}
	}
	var payload bytes.Buffer
	for i, k := range nKids {
		b := nMoov[nStart+k.start : nStart+k.end]
		switch k.typ {
		case "mvhd": // next_track_ID is its last field in both versions
			b = append([]byte{}, b...)
			binary.BigEndian.PutUint32(b[len(b)-4:], nextID+uint32(len(newTraks)))
		case "udta":
			continue // merged below
		}
		payload.Write(b)
		if i == lastTrak {
			for _, t := range newTraks {
				payload.Write(t)
			}
		}
	}
	udta := mergeUdta(oUdta, childBytes(nMoov, "udta"))
	if udta != nil {
		payload.Write(udta)
		if oUdta != nil {
			what = append(what, "user data (tags, cover)")
		}
	}
	if _, err := dst.WriteAt(wrapBox("moov", payload.Bytes()), pos); err != nil {
		return nil, err
	}
	return what, dst.Sync()
}

// mergeUdta combines two udta boxes: the original's entries, then the copy's others.
func mergeUdta(orig, copy []byte) []byte {
	if orig == nil {
		return copy
	}
	var payload bytes.Buffer
	seen := map[string]bool{}
	okids, ostart, err := children(orig, 0)
	if err != nil {
		return copy
	}
	for _, k := range okids {
		payload.Write(orig[ostart+k.start : ostart+k.end])
		seen[k.typ] = true
	}
	if copy != nil {
		if ckids, cstart, err := children(copy, 0); err == nil {
			for _, k := range ckids {
				if !seen[k.typ] {
					payload.Write(copy[cstart+k.start : cstart+k.end])
				}
			}
		}
	}
	return wrapBox("udta", payload.Bytes())
}

func describeTrack(trak []byte) string {
	stsd := childBytes(childBytes(childBytes(childBytes(trak, "mdia"), "minf"), "stbl"), "stsd")
	format := ""
	if len(stsd) >= 24 {
		format = string(stsd[20:24])
	}
	switch format {
	case "djmd":
		return "DJI flight telemetry"
	case "dbgi":
		return "DJI debug data"
	case "mebx":
		return "Apple timed metadata"
	}
	return "metadata track " + format
}
