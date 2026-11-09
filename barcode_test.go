package main

import (
	"image"
	"slices"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// ---------------------------------------------------------------- synthetic barcodes (made-up values only)

// barcodeModules is contents as 1D modules, quiet zones included: true is a
// bar. In Code 128, 'ñ' is FNC1: a leading one makes the barcode GS1-128.
func barcodeModules(t testing.TB, format gozxing.BarcodeFormat, contents string) []bool {
	t.Helper()
	var w gozxing.Writer = oned.NewCode128Writer()
	if format == gozxing.BarcodeFormat_CODE_39 {
		w = oned.NewCode39Writer()
	}
	m, err := w.Encode(contents, format, 0, 1, nil)
	if err != nil {
		t.Fatalf("encoding %q: %v", contents, err)
	}
	mods := make([]bool, m.GetWidth())
	for x := range mods {
		mods[x] = m.Get(x, 0)
	}
	return mods
}

func code128(t testing.TB, contents string) []bool {
	return barcodeModules(t, gozxing.BarcodeFormat_CODE_128, contents)
}

// drawBars draws modules at (x, y), module px per module and height px tall.
// vertical turns the barcode 90°: its bars run across and it reads downwards.
func drawBars(g *image.Gray, mods []bool, x, y, module, height int, vertical bool) {
	for i, bar := range mods {
		if !bar {
			continue
		}
		for a := i * module; a < (i+1)*module; a++ {
			for b := range height {
				px, py := x+a, y+b
				if vertical {
					px, py = x+b, y+a
				}
				g.Pix[py*g.Stride+px] = 0
			}
		}
	}
}

// drawQR draws contents as a QR code at (x, y), module px per module.
func drawQR(t testing.TB, g *image.Gray, contents string, x, y, module int) {
	t.Helper()
	m, err := qrcode.NewQRCodeWriter().Encode(contents, gozxing.BarcodeFormat_QR_CODE, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for my := range m.GetHeight() {
		for mx := range m.GetWidth() {
			if m.Get(mx, my) {
				for dy := range module {
					for dx := range module {
						g.Pix[(y+my*module+dy)*g.Stride+x+mx*module+dx] = 0
					}
				}
			}
		}
	}
}

// label4x6 is a blank 4×6 in label at 203 dpi, as the pipeline sends it.
func label4x6() *image.Gray { return newWhite(812, 1218) }

// scaleGray resamples g by f with area averaging (4×4 samples a pixel) and
// thresholds it at 128: what shrinking a label to fit does to its bars.
func scaleGray(g *image.Gray, f float64) *image.Gray {
	w, h := int(float64(g.Rect.Dx())*f), int(float64(g.Rect.Dy())*f)
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			sum := 0
			for sy := range 4 {
				for sx := range 4 {
					ix := int((float64(x) + (float64(sx)+0.5)/4) / f)
					iy := int((float64(y) + (float64(sy)+0.5)/4) / f)
					sum += int(g.Pix[iy*g.Stride+ix])
				}
			}
			if sum/16 >= 128 {
				out.Pix[y*out.Stride+x] = 255
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- decodeLabel

const (
	gs1IMpbZIP5 = "ñ420" + fakeZIP5 + "ñ" + fakeIMpb22
	gs1IMpbZIP9 = "ñ420" + fakeZIP9 + "ñ" + fakeIMpb22
	gs1Routing  = "ñ420" + fakeZIP5
)

func TestDecodeLabel(t *testing.T) {
	horizontal := func(contents string) func(t *testing.T) *image.Gray {
		return func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, contents), 40, 700, 3, 200, false)
			return g
		}
	}
	for _, c := range []struct {
		name     string
		draw     func(t *testing.T) *image.Gray
		status   string
		carrier  string
		tracking string
		formats  []string
	}{
		{"ups", horizontal(fakeUPS), checkOK, "UPS", fakeUPS, []string{"code128"}},
		{"impb zip5", horizontal(gs1IMpbZIP5), checkOK, "USPS", fakeIMpb22, []string{"code128"}},
		// 420 + ZIP9 + 22 is 34 digits: still USPS, never FedEx 34
		{"impb zip9", horizontal(gs1IMpbZIP9), checkOK, "USPS", fakeIMpb22, []string{"code128"}},
		{"usps plain", horizontal(fakeIMpb22), checkOK, "USPS", fakeIMpb22, []string{"code128"}},
		{"fedex ground", horizontal(fakeFedEx), checkOK, "FedEx", "678901234567890", []string{"code128"}},
		{"fedex ground 34", horizontal(fakeGSN), checkOK, "FedEx", "123456789012", []string{"code128"}},
		{"fedex express 34", horizontal(fakeExp34), checkOK, "FedEx", "123456789012", []string{"code128"}},
		{"routing only", horizontal(gs1Routing), checkReads, "", "", []string{"code128"}},
		{"plain 12 digits", horizontal("123456789012"), checkReads, "", "", []string{"code128"}},
		{"code 39", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, barcodeModules(t, gozxing.BarcodeFormat_CODE_39, "REF-1234"), 40, 700, 3, 200, false)
			return g
		}, checkReads, "", "", []string{"code39"}},
		{"qr only", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawQR(t, g, "https://example.com/return/12345", 200, 300, 8)
			return g
		}, checkReads, "", "", []string{"qr"}},
		{"blank", func(*testing.T) *image.Gray { return label4x6() }, checkNone, "", "", nil},
		// found by TRY_HARDER's own turn, when nothing reads upright
		{"vertical ups alone", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, fakeUPS), 300, 200, 3, 200, true)
			return g
		}, checkOK, "UPS", fakeUPS, []string{"code128"}},
		// the postal barcode reads upright, so TRY_HARDER never turns: the column bands find the 1Z
		{"horizontal postal, vertical ups", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, gs1Routing), 40, 560, 3, 150, false)
			drawBars(g, code128(t, fakeUPS), 560, 20, 3, 200, true)
			return g
		}, checkOK, "UPS", fakeUPS, []string{"code128"}},
		// the whole-image pass reads the postal barcode at the middle; a row band finds the 1Z
		{"postal above ups", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, gs1Routing), 40, 540, 3, 150, false)
			drawBars(g, code128(t, fakeUPS), 40, 950, 3, 200, false)
			return g
		}, checkOK, "UPS", fakeUPS, []string{"code128"}},
		// rank 5 in pass 1 doesn't stop the passes: the 1Z in a band still wins
		{"plain usps, then ups in a band", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, fakeIMpb22), 40, 540, 3, 150, false)
			drawBars(g, code128(t, fakeUPS), 40, 950, 3, 200, false)
			return g
		}, checkOK, "UPS", fakeUPS, []string{"code128"}},
		// a label shrunk to fit (86%) and thresholded again: 3-dot modules survive
		{"scaled, 3-dot modules", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, fakeUPS), 40, 700, 3, 200, false)
			return scaleGray(g, 0.86)
		}, checkOK, "UPS", fakeUPS, []string{"code128"}},
		// 2-dot modules shrunk to 75% don't: the check saying so is the feature working
		{"scaled, 2-dot modules", func(t *testing.T) *image.Gray {
			g := label4x6()
			drawBars(g, code128(t, fakeUPS), 40, 700, 2, 200, false)
			return scaleGray(g, 0.75)
		}, checkNone, "", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := decodeLabel(c.draw(t))
			if got.Status != c.status || got.Carrier != c.carrier || got.Tracking != c.tracking || !slices.Equal(got.Formats, c.formats) {
				t.Errorf("got %+v, want %s %s %q %v", got, c.status, c.carrier, c.tracking, c.formats)
			}
		})
	}
}

// ---------------------------------------------------------------- bounds

func TestCheckRefusesTinyImages(t *testing.T) {
	never := func(*image.Gray) labelCheck {
		t.Error("the decoder was called")
		return labelCheck{Status: checkOK}
	}
	if got := checkGray(image.NewGray(image.Rect(0, 0, 0, 0)), never); got.Status != checkError {
		t.Errorf("0×0: %+v", got)
	}
	for _, side := range []int{1, 7} {
		if got := checkLabelPNG(encodePNG(t, newWhite(side, side)), never); got.Status != checkError {
			t.Errorf("%d×%d: %+v", side, side, got)
		}
	}
	if got := checkLabelPNG(encodePNG(t, newWhite(8, 7)), never); got.Status != checkError {
		t.Errorf("8×7: %+v", got)
	}
	if got := checkLabelPNG([]byte("not a png"), never); got.Status != checkError {
		t.Errorf("not a png: %+v", got)
	}
	called := false
	checkLabelPNG(encodePNG(t, newWhite(8, 8)), func(*image.Gray) labelCheck { called = true; return labelCheck{Status: checkNone} })
	if !called {
		t.Error("8×8 wasn't decoded")
	}
}

// A canvas over maxRenderPx is legitimate (a long roll at a high dpi): it's
// halved until it fits and checked, never refused.
func TestCheckDownscalesLargeCanvas(t *testing.T) {
	g := newWhite(4000, 5000) // 20 Mpx
	drawBars(g, code128(t, fakeUPS), 100, 2500, 6, 400, false)
	var size image.Point
	got := checkGray(g, func(h *image.Gray) labelCheck { size = h.Rect.Size(); return decodeLabel(h) })
	if size != image.Pt(2000, 2500) {
		t.Errorf("decoded at %v, want 2000×2500", size)
	}
	if got.Status != checkOK || got.Tracking != fakeUPS {
		t.Errorf("got %+v", got)
	}
}

// Two halvings or more and nothing found: "didn't finish", since that much
// shrinking can lose a good barcode. One halving and nothing found is still
// "no barcode".
func TestCheckHalvedTwiceNoneIsTimeout(t *testing.T) {
	none := func(*image.Gray) labelCheck { return labelCheck{Status: checkNone} }
	if got := checkGray(newWhite(4000, 5000), none); got.Status != checkNone {
		t.Errorf("one halving: %+v", got)
	}
	if got := checkGray(newWhite(8200, 8000), none); got.Status != checkTimedOut {
		t.Errorf("two halvings: %+v", got)
	}
}

func TestCheckRecoversDecoderPanic(t *testing.T) {
	got := checkLabelPNG(encodePNG(t, label4x6()), func(*image.Gray) labelCheck { panic("decoded 1ZSECRET") })
	if got.Status != checkError || !got.panicked || got.Tracking != "" {
		t.Errorf("got %+v", got)
	}
}

func TestHalve(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 3, 3))
	copy(g.Pix, []uint8{0, 255, 100, 255, 0, 100, 50, 50, 7})
	h := halve(g)
	if h.Rect.Dx() != 2 || h.Rect.Dy() != 2 {
		t.Fatalf("%v", h.Rect)
	}
	// (0+255+255+0)/4, (100+100)/2, (50+50)/2, 7
	if want := []uint8{128, 100, 50, 7}; !slices.Equal(h.Pix, want) {
		t.Errorf("%v, want %v", h.Pix, want)
	}
}
