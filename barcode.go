package main

import (
	"bytes"
	"image"
	"image/png"
	"slices"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// The barcode check reads each label image exactly as LPrint will get it,
// with gozxing in-process, and says whether its tracking barcode decodes.
// It's a hint for the page and never blocks a print. Decoded values are
// never logged.

// Check statuses, as the page gets them.
const (
	checkPending  = "pending"
	checkOK       = "ok"      // a value matched a tracking rule
	checkReads    = "reads"   // something decoded, nothing matched
	checkNone     = "none"    // nothing decoded
	checkTimedOut = "timeout" // didn't finish
	checkError    = "error"   // a recovered panic or a refused size
)

// labelCheck is one label's result.
type labelCheck struct {
	Status   string   `json:"status"`
	Carrier  string   `json:"carrier,omitempty"`
	Tracking string   `json:"tracking,omitempty"`
	Formats  []string `json:"formats,omitempty"` // what decoded: code128, code39, qr

	panicked bool // for the store's log line; never sent
}

// minCheckSide refuses images too small to hold a barcode before gozxing
// sees them.
const minCheckSide = 8

// decodeLabel runs the passes on one label, collecting distinct values:
//
//  1. the whole image, Code 128 and Code 39, TRY_HARDER + ASSUME_GS1
//     (TRY_HARDER scans every few rows and, only if that finds nothing,
//     the image turned 90°);
//  2. row bands: strips ⅙ of the height overlapping by half, ASSUME_GS1
//     only, so a second barcode above or below the first is found;
//  3. column bands: the same on the image turned 90°, for a vertical
//     barcode beside a horizontal one;
//  4. QR, once, on the whole image.
//
// A value of rank 1 or 2 (UPS, USPS IMpb in GS1 form) skips the passes after
// the one that found it. A band cut with Crop still supports rotation, so the
// bands go without TRY_HARDER: it would turn and rescan a strip that can't
// hold a vertical barcode.
func decodeLabel(img *image.Gray) labelCheck {
	src := gozxing.NewLuminanceSourceFromImage(img)
	d := &decodeRun{seen: map[string]bool{}}
	hard := map[gozxing.DecodeHintType]interface{}{
		gozxing.DecodeHintType_TRY_HARDER: true,
		gozxing.DecodeHintType_ASSUME_GS1: true,
	}
	gs1 := map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_ASSUME_GS1: true}

	passes := []func(){
		func() { d.oneD(src, hard) },
		func() { d.bands(src, gs1, false) },
		func() {
			if rot, err := src.RotateCounterClockwise(); err == nil {
				d.bands(rot, gs1, true)
			}
		},
		func() { d.try(src, qrcode.NewQRCodeReader(), nil) },
	}
	for _, pass := range passes {
		pass()
		if m := bestTracking(d.texts); m.rank == 1 || m.rank == 2 {
			break
		}
	}
	return d.result()
}

type decodeRun struct {
	texts   []string // distinct, in reading order
	seen    map[string]bool
	formats []string
}

func (d *decodeRun) oneD(src gozxing.LuminanceSource, hints map[gozxing.DecodeHintType]interface{}) {
	d.try(src, oned.NewCode128Reader(), hints)
	d.try(src, oned.NewCode39Reader(), hints)
}

// bands scans 11 overlapping strips ⅙ of src's height, top to bottom, or
// bottom to top when src is the image turned counter-clockwise, so that
// column bands go left to right across the label.
func (d *decodeRun) bands(src gozxing.LuminanceSource, hints map[gozxing.DecodeHintType]interface{}, reverse bool) {
	w, h := src.GetWidth(), src.GetHeight()
	bh := max(1, h/6)
	step := max(1, bh/2)
	var tops []int
	for top := 0; top+bh <= h; top += step {
		tops = append(tops, top)
	}
	if reverse {
		slices.Reverse(tops)
	}
	for _, top := range tops {
		band, err := src.Crop(0, top, w, bh)
		if err != nil {
			continue
		}
		d.oneD(band, hints)
	}
}

// try decodes src once with its own binarizer and BinaryBitmap: a shared one
// is the race gozxing fixed in PR #75. Errors are dropped unread: their text
// can quote what was decoded.
func (d *decodeRun) try(src gozxing.LuminanceSource, r gozxing.Reader, hints map[gozxing.DecodeHintType]interface{}) {
	bmp, err := gozxing.NewBinaryBitmap(gozxing.NewHybridBinarizer(src))
	if err != nil {
		return
	}
	res, err := r.Decode(bmp, hints)
	if err != nil || res == nil {
		return
	}
	text := res.GetText()
	if text == "" || d.seen[text] {
		return
	}
	d.seen[text] = true
	d.texts = append(d.texts, text)
	if f := formatName(res.GetBarcodeFormat()); !slices.Contains(d.formats, f) {
		d.formats = append(d.formats, f)
	}
}

func (d *decodeRun) result() labelCheck {
	if len(d.texts) == 0 {
		return labelCheck{Status: checkNone}
	}
	c := labelCheck{Status: checkReads, Formats: d.formats}
	if m := bestTracking(d.texts); m.rank > 0 {
		c.Status, c.Carrier, c.Tracking = checkOK, m.carrier, m.value
	}
	return c
}

func formatName(f gozxing.BarcodeFormat) string {
	switch f {
	case gozxing.BarcodeFormat_CODE_128:
		return "code128"
	case gozxing.BarcodeFormat_CODE_39:
		return "code39"
	case gozxing.BarcodeFormat_QR_CODE:
		return "qr"
	}
	return "other"
}

// checkLabelPNG checks one label PNG. Its size is read from the header
// first, so an image too small to hold a barcode is refused before it's
// decoded; then checkGray.
func checkLabelPNG(data []byte, decode func(*image.Gray) labelCheck) labelCheck {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < minCheckSide || cfg.Height < minCheckSide {
		return labelCheck{Status: checkError}
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return labelCheck{Status: checkError}
	}
	return checkGray(toGray(img), decode)
}

// checkGray checks a decoded label. A panic in decode is an error, never a
// crash, and its value is dropped: it can quote what was being decoded. An
// image over maxRenderPx (a long roll at a high RENDER_DPI) is halved by
// area averaging until it fits: a large canvas is legitimate, never an
// error. After two halvings or more, nothing found means "didn't finish",
// not "no barcode": that much shrinking can make a good label unreadable.
func checkGray(g *image.Gray, decode func(*image.Gray) labelCheck) (c labelCheck) {
	defer func() {
		if recover() != nil {
			c = labelCheck{Status: checkError, panicked: true}
		}
	}()
	if g.Rect.Dx() < minCheckSide || g.Rect.Dy() < minCheckSide {
		return labelCheck{Status: checkError}
	}
	g = toGray(g) // origin at 0,0, which halve relies on
	halvings := 0
	for g.Rect.Dx()*g.Rect.Dy() > maxRenderPx {
		g = halve(g)
		halvings++
	}
	c = decode(g)
	if halvings >= 2 && c.Status == checkNone {
		c.Status = checkTimedOut
	}
	return c
}

// halve shrinks g to half each way, each pixel the mean of the 2×2 (or, at
// an odd edge, smaller) block it covers.
func halve(g *image.Gray) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	out := image.NewGray(image.Rect(0, 0, (w+1)/2, (h+1)/2))
	for y := range out.Rect.Dy() {
		for x := range out.Rect.Dx() {
			sum, n := 0, 0
			for dy := range 2 {
				for dx := range 2 {
					sx, sy := 2*x+dx, 2*y+dy
					if sx < w && sy < h {
						sum += int(g.Pix[sy*g.Stride+sx])
						n++
					}
				}
			}
			out.Pix[y*out.Stride+x] = uint8((sum + n/2) / n)
		}
	}
	return out
}
