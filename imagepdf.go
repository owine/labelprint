package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // GIF uploads; bitmap.go registers PNG and JPEG
	"math"
	"net/http"
	"strconv"
	"strings"
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

// wrapImage checks an image from its header, decodes it to gray on white and
// writes it as a one-page PDF, W×H pixels at its ppi. media is the label size
// the request resolved (mediaOK false when it's unknown), which the ppi can
// be matched to. Run it inside a pipeline slot: decoding is the costly part.
func wrapImage(ctx context.Context, data []byte, kind docKind, media labelSize, mediaOK bool, dpi int) ([]byte, imageInfo, error) {
	w, h, err := checkImage(data, kind)
	if err != nil {
		return nil, imageInfo{}, err
	}
	ppi, src := imagePPI(data, kind, w, h, media, mediaOK, dpi)
	info := imageInfo{W: w, H: h, PPI: ppi, PPISource: src}
	// Only the header so far: refuse what can't print before decoding it.
	if err := checkImageSize(w, h, ppi, dpi); err != nil {
		return nil, info, err
	}
	if err := ctx.Err(); err != nil {
		return nil, info, err
	}
	g, err := decodeGray(data, w, h)
	if err != nil {
		return nil, info, err
	}
	if err := ctx.Err(); err != nil {
		return nil, info, err
	}
	return imagePDF(g, ppi), info, nil
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

// ---------------------------------------------------------------- decoding

// decodeGray decodes data and lays it on white as w × h gray. The decoded
// image is dropped before it returns, so only the gray is left to hold.
func decodeGray(data []byte, w, h int) (*image.Gray, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errImageUnreadable
	}
	return flatten(img, w, h), nil
}

// onWhite is the gray level a premultiplied 16-bit colour shows laid on
// white, with color.GrayModel's luma weights.
func onWhite(r, g, b, a uint32) uint8 {
	w := 0xffff - a
	r, g, b = min(r+w, 0xffff), min(g+w, 0xffff), min(b+w, 0xffff)
	return uint8((19595*r + 38470*g + 7471*b + 1<<15) >> 24)
}

func gray8(r, g, b uint8) uint8 {
	return onWhite(uint32(r)*0x101, uint32(g)*0x101, uint32(b)*0x101, 0xffff)
}

// flatten lays img on a white w × h canvas at its own bounds (a GIF frame
// can be smaller than, and offset within, its logical screen) and turns it
// gray. Each type the decoders return is read from its Pix: At boxes every
// pixel into an interface, an allocation each.
func flatten(img image.Image, w, h int) *image.Gray {
	g := newWhite(w, h)
	r := img.Bounds().Intersect(g.Rect)
	if r.Empty() {
		return g
	}
	row := func(y int) []uint8 { return g.Pix[y*g.Stride : (y+1)*g.Stride] }
	switch m := img.(type) {
	case *image.Gray:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			i := m.PixOffset(r.Min.X, y)
			copy(row(y)[r.Min.X:r.Max.X], m.Pix[i:i+r.Dx()])
		}
	case *image.Gray16:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x, p = x+1, p[2:] {
				v := uint32(p[0])<<8 | uint32(p[1])
				out[x] = onWhite(v, v, v, 0xffff)
			}
		}
	case *image.YCbCr:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out := row(y)
			for x := r.Min.X; x < r.Max.X; x++ {
				yi, ci := m.YOffset(x, y), m.COffset(x, y)
				out[x] = gray8(color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci]))
			}
		}
	case *image.CMYK:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x, p = x+1, p[4:] {
				out[x] = gray8(color.CMYKToRGB(p[0], p[1], p[2], p[3]))
			}
		}
	case *image.RGBA:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x, p = x+1, p[4:] {
				out[x] = onWhite(uint32(p[0])*0x101, uint32(p[1])*0x101, uint32(p[2])*0x101, uint32(p[3])*0x101)
			}
		}
	case *image.NRGBA:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x, p = x+1, p[4:] {
				a := uint32(p[3])
				out[x] = onWhite(uint32(p[0])*0x101*a/0xff, uint32(p[1])*0x101*a/0xff, uint32(p[2])*0x101*a/0xff, a*0x101)
			}
		}
	case *image.RGBA64:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x, p = x+1, p[8:] {
				out[x] = onWhite(be16(p), be16(p[2:]), be16(p[4:]), be16(p[6:]))
			}
		}
	case *image.NRGBA64:
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x, p = x+1, p[8:] {
				a := be16(p[6:])
				out[x] = onWhite(be16(p)*a/0xffff, be16(p[2:])*a/0xffff, be16(p[4:])*a/0xffff, a)
			}
		}
	case *image.Paletted:
		var lut [256]uint8 // an index past the palette is black, as image/png pads it
		for i, c := range m.Palette {
			if c != nil && i < len(lut) {
				lut[i] = onWhite(c.RGBA())
			}
		}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out, p := row(y), m.Pix[m.PixOffset(r.Min.X, y):]
			for x := r.Min.X; x < r.Max.X; x++ {
				out[x] = lut[p[x-r.Min.X]]
			}
		}
	default: // no decoder returns another type; correct, if slow
		for y := r.Min.Y; y < r.Max.Y; y++ {
			out := row(y)
			for x := r.Min.X; x < r.Max.X; x++ {
				out[x] = onWhite(img.At(x, y).RGBA())
			}
		}
	}
	return g
}

func be16(p []uint8) uint32 { return uint32(p[0])<<8 | uint32(p[1]) }

// ---------------------------------------------------------------- size and ppi

// checkImageSize refuses an image that can't make a label at its ppi: one
// whose render at the printer's dpi would be over maxRenderPx (checkRender's
// sums for the whole page, as renderBoxAt does them), or one under 0.5 in on a
// side, which couldn't hold a box the page can draw.
func checkImageSize(w, h int, ppi float64, dpi int) error {
	f := float64(dpi) / ppi
	if err := checkRender("that image", math.Ceil(float64(w)*f)+1, math.Ceil(float64(h)*f)+1); err != nil {
		return err
	}
	if float64(w)/ppi < minImageIn || float64(h)/ppi < minImageIn {
		return errImageTooSmall
	}
	return nil
}

// imagePPI is the image's resolution, which only sets its size in inches:
// the file's own density when it's plausible, else what makes it the
// media's size (or the media plus a 1 in doc tab), else the printer's dpi.
// Every ppi but the default is rounded to 0.1.
func imagePPI(data []byte, kind docKind, w, h int, media labelSize, mediaOK bool, dpi int) (float64, string) {
	switch kind {
	case kindPNG:
		if v, ok := pngPPI(data); ok {
			return v, "phys"
		}
	case kindJPEG:
		if v, ok := jfifPPI(data); ok {
			return v, "jfif"
		}
	}
	if mediaOK && media.WidthMm > 0 && media.HeightMm > 0 {
		long, short := float64(max(w, h)), float64(min(w, h))
		mLong, mShort := max(media.WidthMm, media.HeightMm)/25.4, min(media.WidthMm, media.HeightMm)/25.4
		for _, m := range []struct {
			tab float64
			src string
		}{{0, "media"}, {1, "media+tab"}} {
			l := mLong + m.tab
			if math.Abs((long/short)/(l/mShort)-1) > aspectTolerance {
				continue
			}
			if ppi := round1(long / l); ppi >= ppiMatchMin && ppi <= ppiMatchMax {
				return ppi, m.src
			}
		}
	}
	return float64(dpi), "default"
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func plausiblePPI(v float64) (float64, bool) {
	v = round1(v)
	return v, v >= ppiMetaMin && v <= ppiMetaMax
}

// pngPPI reads a pHYs chunk (image/png doesn't expose it): pixels per metre,
// x and y within 1% of each other.
func pngPPI(data []byte) (float64, bool) {
	for i := 8; i+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		if n < 0 || n > len(data)-i-12 || typ == "IDAT" || typ == "IEND" {
			break // pHYs must come before the image data
		}
		if typ == "pHYs" && n == 9 {
			p := data[i+8:]
			x, y := float64(binary.BigEndian.Uint32(p)), float64(binary.BigEndian.Uint32(p[4:]))
			if p[8] != 1 || x <= 0 || y <= 0 || math.Abs(x-y) > 0.01*max(x, y) {
				return 0, false
			}
			return plausiblePPI(x * 0.0254)
		}
		i += 12 + n
	}
	return 0, false
}

// jfifPPI reads the JFIF APP0 density: dots per inch (units 1) or per cm
// (units 2), x equal to y.
func jfifPPI(data []byte) (float64, bool) {
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 0, false
		}
		m := data[i+1]
		if m == 0xFF { // a fill byte
			i++
			continue
		}
		if m == 0xDA || m == 0xD9 {
			return 0, false
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return 0, false
		}
		if p := data[i+4 : i+2+n]; m == 0xE0 && len(p) >= 12 && string(p[:5]) == "JFIF\x00" {
			units := p[7]
			x, y := binary.BigEndian.Uint16(p[8:]), binary.BigEndian.Uint16(p[10:])
			if x != y || x == 0 {
				return 0, false
			}
			switch units {
			case 1:
				return plausiblePPI(float64(x))
			case 2:
				return plausiblePPI(float64(x) * 2.54)
			}
			return 0, false
		}
		i += 2 + n
	}
	return 0, false
}

// ---------------------------------------------------------------- the pdf

// imagePDF writes g as a one-page PDF, W×H pixels at ppi: the page is the
// image (MediaBox only, no /Rotate or CropBox), drawn as one 8-bit
// DeviceGray XObject. It only ever wraps pixels decodeGray just made.
func imagePDF(g *image.Gray, ppi float64) []byte {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed) // only fails for a bad level
	for y := range h {
		_, _ = zw.Write(g.Pix[y*g.Stride : y*g.Stride+w]) // a bytes.Buffer doesn't fail
	}
	_ = zw.Close()
	pw, ph := pdfNum(float64(w)*72/ppi), pdfNum(float64(h)*72/ppi)
	content := fmt.Sprintf("q %s 0 0 %s 0 0 cm /Im0 Do Q\n", pw, ph)

	var out bytes.Buffer
	out.Grow(z.Len() + 1024)
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	var offsets []int // object n starts at offsets[n-1]
	obj := func(dict string, stream []byte) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\n", len(offsets), dict)
		if stream != nil {
			out.WriteString("stream\n")
			out.Write(stream)
			out.WriteString("\nendstream\n")
		}
		out.WriteString("endobj\n")
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>", nil)
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>", nil)
	obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] "+
		"/Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>", pw, ph), nil)
	obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray "+
		"/BitsPerComponent 8 /Filter /FlateDecode /Length %d >>", w, h, z.Len()), z.Bytes())
	obj(fmt.Sprintf("<< /Length %d >>", len(content)), []byte(content))
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

// pdfNum writes v with up to 4 decimals, no exponent and no trailing zeros.
func pdfNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
