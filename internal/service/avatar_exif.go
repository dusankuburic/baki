package service

import (
	"bytes"
	"encoding/binary"
	"image"
)

// The EXIF Orientation of an uploaded JPEG. Cameras store the sensor's pixels as they were read and record how to
// turn them in the Orientation tag (0x0112 in IFD0); viewers apply it. The re-encode stores no metadata, so it has to
// turn the pixels itself or the stored avatar shows on its side. This is a deliberately small reader, not an EXIF
// library: it walks the marker segments before the first scan, reads IFD0 of an APP1 "Exif" segment and nothing
// else, and bounds every offset by that segment (at most 64 KB), so a hostile header costs one pass over its bytes.

// exifOrientationTag is the Orientation tag; exifTypeShort its required TIFF type (SHORT, one value).
const (
	exifOrientationTag = 0x0112
	exifTypeShort      = 3
)

// jpegOrientation returns the EXIF Orientation (1-8) of the JPEG data, or 1 (as stored) when it carries none or the
// segment cannot be read: an unreadable tag never refuses an otherwise valid image.
func jpegOrientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			// A stray byte between segments: skip to the next marker, as libjpeg (with a warning) and image/jpeg
			// do. Every step moves on, so this is still one pass.
			i++
			continue
		}
		marker := data[i+1]
		switch {
		case marker == 0xff: // fill byte before a marker
			i++
			continue
		case marker == 0x00: // FF 00 is no marker, only a stuffed byte: stray data too
			i += 2
			continue
		case marker == 0x01 || (marker >= 0xd0 && marker <= 0xd8): // TEM, RSTn, SOI: no length field
			i += 2
			continue
		case marker == 0xda || marker == 0xd9: // the scan (or the end) starts: the metadata segments are all before it
			return 1
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return 1
		}
		if seg := data[i+4 : i+2+n]; marker == 0xe1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return exifOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

// exifOrientation reads the Orientation entry of IFD0 from a TIFF structure (an Exif segment's payload).
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return 1
	}
	// Compared as len-2, not ifd+2: an offset near the top of a 32-bit int would wrap past the check.
	ifd := int(order.Uint32(tiff[4:]))
	if ifd < 8 || ifd > len(tiff)-2 {
		return 1
	}
	entries := int(order.Uint16(tiff[ifd:]))
	for k := 0; k < entries; k++ {
		e := ifd + 2 + 12*k
		if e+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[e:]) != exifOrientationTag {
			continue
		}
		if order.Uint16(tiff[e+2:]) != exifTypeShort || order.Uint32(tiff[e+4:]) != 1 {
			return 1
		}
		if o := int(order.Uint16(tiff[e+8:])); o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}

// orientImage returns src as a viewer shows it under EXIF orientation o: 2 mirrors it left to right, 3 turns it
// 180°, 4 mirrors it top to bottom, 5 mirrors it across the main diagonal, 6 turns it 90° clockwise, 7 mirrors it
// across the other diagonal, 8 turns it 90° counter-clockwise; 5 to 8 swap the sides. Any other o returns src.
func orientImage(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if o >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
