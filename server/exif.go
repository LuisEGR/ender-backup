package main

import (
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
)

// jpegExif returns the TIFF-format EXIF block from a JPEG's APP1 segment.
func jpegExif(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errors.New("not a JPEG")
	}
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xFF {
			return nil, errors.New("bad JPEG segment")
		}
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 { // image data starts: no EXIF before it
			break
		}
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		if pos+2+size > len(data) {
			break
		}
		seg := data[pos+4 : pos+2+size]
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return seg[6:], nil
		}
		pos += 2 + size
	}
	return nil, errors.New("no EXIF")
}

// tiffOrder returns the byte order of a TIFF block and the offset of IFD0.
func tiffOrder(tiff []byte) (binary.ByteOrder, int, bool) {
	if len(tiff) < 8 {
		return nil, 0, false
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil, 0, false
	}
	return bo, int(bo.Uint32(tiff[4:])), true
}

// orientationEntry finds the Orientation tag (0x0112) in IFD0 and returns the offset of
// its value, or -1.
func orientationEntry(tiff []byte) (binary.ByteOrder, int) {
	bo, ifd, ok := tiffOrder(tiff)
	if !ok || ifd+2 > len(tiff) {
		return nil, -1
	}
	n := int(bo.Uint16(tiff[ifd:]))
	for i := 0; i < n; i++ {
		e := ifd + 2 + i*12
		if e+12 > len(tiff) {
			break
		}
		if bo.Uint16(tiff[e:]) == 0x0112 {
			return bo, e + 8
		}
	}
	return bo, -1
}

// exifOrientation returns the EXIF orientation (1–8), or 1 if absent.
func exifOrientation(tiff []byte) int {
	bo, at := orientationEntry(tiff)
	if at < 0 {
		return 1
	}
	if v := int(bo.Uint16(tiff[at:])); v >= 1 && v <= 8 {
		return v
	}
	return 1
}

// withOrientationUpright returns a copy of the EXIF block with Orientation set to 1:
// the converted pixels are already the right way up.
func withOrientationUpright(tiff []byte) []byte {
	out := append([]byte{}, tiff...)
	if bo, at := orientationEntry(out); at >= 0 {
		bo.PutUint16(out[at:], 1)
	}
	return out
}

// applyOrientation rotates/flips an image as EXIF orientation o says it should be shown.
func applyOrientation(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	in := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(in, in.Bounds(), src, b.Min, draw.Src)
	dw, dh := w, h
	if o >= 5 { // 5–8 swap width and height
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // rotate 90 clockwise
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 90 counter-clockwise
				dx, dy = y, w-1-x
			}
			si := in.PixOffset(x, y)
			di := out.PixOffset(dx, dy)
			copy(out.Pix[di:di+4], in.Pix[si:si+4])
		}
	}
	return out
}
