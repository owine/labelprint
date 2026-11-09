package main

import (
	"bytes"
	"image"
	_ "image/gif" // GIF uploads; bitmap.go registers PNG and JPEG
	"net/http"
)

// An uploaded image goes through the PDF pipeline: it's decoded here, laid
// on white, turned gray and wrapped as a one-page PDF sized by its ppi, so it
// gets detection, the box editor, native passthrough and path B like a PDF.
//
// Go's decoders don't check a context and trust the header, so everything
// that bounds a decode is checked from the header first (checkImage), and
// wrapImage runs inside a pipeline slot and the request's deadline.

const (
	maxDecodeBytes = 96 << 20 // what one decode may hold, estimated from the header (checkImage)
	maxJPEGScans   = 64       // real files have about 10; each scan walks every block
	minImageIn     = 0.5      // MIN_IN in web/app.js: the smallest box the page can make

	ppiMetaMin, ppiMetaMax   = 100.0, 600.0 // a pHYs or JFIF density outside this is a placeholder
	ppiMatchMin, ppiMatchMax = 50.0, 600.0  // a media match may be a 96 ppi screenshot
	aspectTolerance          = 0.03
)

var (
	errImageUnreadable = &userError{Status: http.StatusUnsupportedMediaType, Msg: "couldn't read that image"}
	errImageEmpty      = &userError{Status: http.StatusUnprocessableEntity, Msg: "that image is empty"}
	errImageTooLarge   = &userError{Status: http.StatusUnprocessableEntity, Msg: "that image is too large to print as a label"}
	errImageTooSmall   = &userError{Status: http.StatusUnprocessableEntity, Msg: "that image is too small to print as a label"}
)

// imageInfo is an uploaded image as the page is told about it. PPISource is
// where PPI came from: "phys" (PNG pHYs), "jfif" (JPEG APP0), "media" or
// "media+tab" (matched to the label size, or to it plus a 1 in doc tab) or
// "default" (RENDER_DPI: a guess, which the page warns about).
type imageInfo struct {
	W         int     `json:"w"`
	H         int     `json:"h"`
	PPI       float64 `json:"ppi"`
	PPISource string  `json:"ppiSource"`
}

// ---------------------------------------------------------------- before decoding

// checkImage reads the image's header and refuses, before anything is
// decoded, what would take too much memory or time to decode. w × h is the
// image's size (a GIF's logical screen).
func checkImage(data []byte, kind docKind) (w, h int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, errImageUnreadable
	}
	w, h = cfg.Width, cfg.Height
	if w <= 0 || h <= 0 {
		return 0, 0, errImageEmpty
	}
	// Past this the pdfimages gate refuses the wrapped image, and with it
	// native passthrough and the doc-tab split. It also stops 16000000×1.
	if w > maxImageSide || h > maxImageSide {
		return 0, 0, errImageTooLarge
	}
	var bpp float64
	switch kind {
	case kindJPEG:
		j := scanJPEG(data)
		if j.scans > maxJPEGScans {
			return 0, 0, errImageUnreadable
		}
		bpp = j.bytesPerPixel()
	case kindPNG:
		bpp = pngBytesPerPixel(data)
	case kindGIF:
		bpp = 1 + 4 // the paletted frame, and room for an RGBA canvas
	default:
		return 0, 0, errImageUnreadable
	}
	if float64(w)*float64(h)*(bpp+1) > maxDecodeBytes { // +1: the gray output
		return 0, 0, errImageTooLarge
	}
	return w, h, nil
}

// jpegInfo is what a byte scan of a JPEG finds: no segment parser, so
// nothing in the file can hide a marker from it.
type jpegInfo struct {
	scans       int  // FF (FF)* DA anywhere: at least the SOS segments Go will decode
	comps       int  // the most components any SOF segment declares (0: none seen)
	progressive bool // an SOF2 anywhere
}

// scanJPEG counts every SOS marker (FF DA, after any fill bytes) in data.
// Entropy-coded data can't hold one (FF 00 is a stuffed byte, FF D0–D7 are
// restarts), and Go's decoder only decodes scans whose marker appears
// literally, so the count is never below what a decode would process. Its
// false positives (FF DA in an APPn payload or after EOI) only make it
// stricter, as does reading every SOF, an EXIF thumbnail's included.
func scanJPEG(data []byte) jpegInfo {
	var j jpegInfo
	for i := 0; i < len(data); i++ {
		if data[i] != 0xFF {
			continue
		}
		k := i + 1
		for k < len(data) && data[k] == 0xFF {
			k++
		}
		if k >= len(data) {
			break
		}
		switch m := data[k]; m {
		case 0xDA:
			j.scans++
		case 0xC0, 0xC1, 0xC2: // the SOFs Go decodes: FF Cn, length, P, Y, X, Nf
			j.progressive = j.progressive || m == 0xC2
			if k+8 < len(data) {
				if n := int(data[k+8]); n == 1 || n == 3 || n == 4 {
					j.comps = max(j.comps, n)
				}
			}
		}
		i = k
	}
	return j
}

// bytesPerPixel is what Go's JPEG decoder holds at once per pixel, measured:
// convertToRGB and applyBlack keep the YCbCr planes beside their output, and
// a progressive decode keeps every block's [64]int32 coefficients.
func (j jpegInfo) bytesPerPixel() float64 {
	c := j.comps
	if c == 0 {
		c = 4 // no SOF seen: assume the worst
	}
	b := map[int]float64{1: 1, 3: 7, 4: 8}[c]
	if j.progressive {
		b += 4 * float64(c)
	}
	return b
}

// pngBytesPerPixel is what Go's PNG decoder holds per pixel, from the IHDR
// alone: DecodeConfig stops there, so a tRNS chunk (which turns gray or RGB
// into NRGBA) is assumed. Interlacing adds the pass images.
func pngBytesPerPixel(data []byte) float64 {
	if len(data) < 29 {
		return 16 // DecodeConfig read an IHDR, so this can't happen
	}
	depth, colorType, interlace := data[24], data[25], data[28]
	b := 4.0
	switch {
	case colorType == 3:
		b = 1
	case depth == 16:
		b = 8
	}
	if interlace == 1 {
		b *= 2
	}
	return b
}
