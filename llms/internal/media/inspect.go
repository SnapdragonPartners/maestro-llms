package media

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/gif"  // register GIF for image.DecodeConfig
	_ "image/jpeg" // register JPEG for image.DecodeConfig
	_ "image/png"  // register PNG for image.DecodeConfig
	"regexp"
	"strconv"
)

// ImageSize returns the pixel dimensions of a PNG, JPEG, GIF or WebP image
// by reading its header only. ok is false for any other format or a header
// that doesn't parse.
func ImageSize(data []byte) (w, h int, ok bool) {
	if w, h, ok = webpSize(data); ok {
		return w, h, true
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// webpSize reads the canvas size from a WebP RIFF header: the VP8X extended
// header, a lossy VP8 key frame, or a lossless VP8L header.
func webpSize(b []byte) (w, h int, ok bool) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8X":
		// 24-bit little-endian canvas width-1 and height-1 at offsets 24, 27.
		w = int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h = int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	case "VP8 ":
		// Frame tag (3 bytes) then the start code 9d 01 2a, then 14-bit sizes.
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		w = int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h = int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	case "VP8L":
		// Signature 0x2f, then width-1 and height-1 as 14-bit fields.
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		w = int(bits&0x3fff) + 1
		h = int((bits>>14)&0x3fff) + 1
	default:
		return 0, 0, false
	}
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

var (
	// pageObject matches a page object's type entry, not /Pages.
	pageObject = regexp.MustCompile(`/Type\s*/Page(?:[^A-Za-z0-9]|$)`)
	// countEntry matches a /Count entry (page tree nodes; outlines too).
	countEntry = regexp.MustCompile(`/Count\s+(\d{1,7})`)
)

// PDFPages estimates a PDF's page count from its uncompressed structure: the
// larger of the number of page objects and the largest /Count entry. Both
// can overcount (an outline's /Count, incremental updates that repeat a
// page), which is the safe direction for an estimate. ok is false when
// neither is visible, as in a PDF that keeps every object in compressed
// object streams.
func PDFPages(data []byte) (int, bool) {
	pages := len(pageObject.FindAllIndex(data, -1))
	for _, m := range countEntry.FindAllSubmatch(data, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > pages {
			pages = n
		}
	}
	return pages, pages > 0
}
