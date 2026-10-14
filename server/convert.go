package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
)

// Converting photos to a smaller format (AVIF). Decoding/encoding use libheif/libavif,
// either the system libraries when installed (fast) or copies compiled to WebAssembly
// bundled in this binary (no install needed, slower).

const defaultQuality = 50

var useSystemCodecs sync.Once

// convertible reports whether a file is a photo we can convert.
func convertible(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".heic", ".heif":
		return true
	}
	return false
}

// convertToAVIF encodes src as AVIF into dst, keeping the photo's EXIF (date, place,
// camera) and making its orientation explicit in the pixels. It returns the output size.
func convertToAVIF(src, dst string, quality int) (int64, error) {
	useSystemCodecs.Do(func() {
		_ = avif.Dynamic() // use installed libavif/libheif if present; else WebAssembly
		_ = heic.Dynamic()
	})
	if quality <= 0 || quality > 100 {
		quality = defaultQuality
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}

	var img image.Image
	var exif, icc []byte
	switch strings.ToLower(filepath.Ext(src)) {
	case ".heic", ".heif":
		// The HEIC decoder already applies the image's rotation.
		if img, err = heic.Decode(bytes.NewReader(data)); err != nil {
			return 0, fmt.Errorf("decode HEIC: %w", err)
		}
		exif, _ = heicExif(data)
		icc, _ = heicICC(data)
	case ".jpg", ".jpeg":
		if img, _, err = image.Decode(bytes.NewReader(data)); err != nil {
			return 0, fmt.Errorf("decode JPEG: %w", err)
		}
		icc, _ = jpegICC(data)
		if exif, err = jpegExif(data); err == nil {
			img = applyOrientation(img, exifOrientation(exif))
		}
	default:
		if img, _, err = image.Decode(bytes.NewReader(data)); err != nil {
			return 0, fmt.Errorf("decode: %w", err)
		}
	}

	var out bytes.Buffer
	if err := avif.Encode(&out, toYCbCr420(img), avif.Options{
		Quality: quality, Speed: 8, ChromaSubsampling: image.YCbCrSubsampleRatio420,
	}); err != nil {
		return 0, fmt.Errorf("encode AVIF: %w", err)
	}
	result := out.Bytes()
	// Keep the colour space (iPhone photos are Display P3; see colorprofile.go).
	if len(icc) > 0 {
		if withICC, err := addICCToHEIF(result, icc); err == nil {
			result = withICC
		}
	}
	if len(exif) > 0 {
		if withExif, err := addExifToHEIF(result, withOrientationUpright(exif)); err == nil {
			result = withExif
		}
	}

	// Check the result decodes and has the right dimensions before keeping it.
	cfg, err := avif.DecodeConfig(bytes.NewReader(result))
	if err != nil {
		return 0, fmt.Errorf("verify AVIF: %w", err)
	}
	if b := img.Bounds(); cfg.Width != b.Dx() || cfg.Height != b.Dy() {
		return 0, errors.New("verify AVIF: size mismatch")
	}

	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, result, 0o644); err != nil {
		return 0, err
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chtimes(tmp, st.ModTime(), st.ModTime())
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return int64(len(result)), nil
}

// toYCbCr420 converts to 8-bit 4:2:0 without alpha, the usual layout for photos: smaller
// and faster to encode than RGBA (which would also store an alpha plane).
func toYCbCr420(src image.Image) image.Image {
	if y, ok := src.(*image.YCbCr); ok && y.SubsampleRatio == image.YCbCrSubsampleRatio420 {
		return y
	}
	b := src.Bounds()
	dst := image.NewYCbCr(b, image.YCbCrSubsampleRatio420)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			yy, cb, cr := color.RGBToYCbCr(uint8(r>>8), uint8(g>>8), uint8(bl>>8))
			dst.Y[dst.YOffset(x, y)] = yy
			if (x-b.Min.X)%2 == 0 && (y-b.Min.Y)%2 == 0 {
				ci := dst.COffset(x, y)
				dst.Cb[ci], dst.Cr[ci] = cb, cr
			}
		}
	}
	return dst
}
