package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

var size4x6 = labelSize{WidthMm: 101.6, HeightMm: 152.4}

func TestZPLSizeFor(t *testing.T) {
	z := newZPLRenderer(203)
	cases := []struct {
		zpl  string
		want labelSize
	}{
		{"^XA^FDx^FS^XZ", size4x6},                                                      // nothing set: the media
		{"^XA^PW400^LL300^XZ", labelSize{50, 37.5}},                                     // dots / 8 dpmm
		{"^XA^pw 812^XZ", labelSize{101.5, 152.4}},                                      // case and spacing; ^LL from media
		{"^XA^PW400^XZ^XA^PW600^XZ", labelSize{75, 152.4}},                              // last one wins
		{"^XA^PW99999^LL99999999^XZ", labelSize{maxPreviewWidthMm, maxPreviewHeightMm}}, // clamped
		{"^XA^PW0^XZ", size4x6},                                                         // zero ignored
	}
	for _, c := range cases {
		if got := z.sizeFor([]byte(c.zpl), size4x6).labelSize; got != c.want {
			t.Errorf("%q: got %+v want %+v", c.zpl, got, c.want)
		}
	}
	// display inches: dots at the real 203 dpi, media sizes as named
	if got := z.sizeFor([]byte("^XA^PW812^XZ"), size4x6); got.WidthIn != 4 || got.HeightIn != 6 {
		t.Errorf("^PW812 on 4x6 media shows as %v × %v in", got.WidthIn, got.HeightIn)
	}
	if got := z.sizeFor([]byte("^XA^LL1218^XZ"), size4x6); got.HeightIn != 6 {
		t.Errorf("^LL1218 shows as %v in", got.HeightIn)
	}
	if z.dpmm != 8 {
		t.Errorf("203 dpi should be 8 dpmm, got %d", z.dpmm)
	}
}

func TestMediaSize(t *testing.T) {
	got, ok := mediaSize("na_index-4x6_4x6in")
	if !ok || math.Abs(got.WidthMm-101.6) > 1e-9 || math.Abs(got.HeightMm-152.4) > 1e-9 {
		t.Fatalf("got %+v %v", got, ok)
	}
	if got, ok := mediaSize("iso_a6_105x148mm"); !ok || got != (labelSize{105, 148}) {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, ok := mediaSize("custom"); ok {
		t.Fatal("want !ok")
	}
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func darkPixels(img image.Image) int {
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y < 128 {
				n++
			}
		}
	}
	return n
}

func TestZPLRenderFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.zpl")
	if err != nil {
		t.Fatal(err)
	}
	p, err := newZPLRenderer(203).render(context.Background(), data, size4x6, 12)
	if err != nil {
		t.Fatal(err)
	}
	if p.Labels != 1 || len(p.Pages) != 1 {
		t.Fatalf("labels=%d pages=%d", p.Labels, len(p.Pages))
	}
	img := decodePNG(t, p.Pages[0].PNG)
	// ^PW812 ^LL1218 at 8 dpmm
	if b := img.Bounds(); b.Dx() != 812 || b.Dy() != 1218 {
		t.Errorf("canvas %dx%d, want 812x1218", b.Dx(), b.Dy())
	}
	// Text, rules, three barcodes and a MaxiCode: well over 2% ink.
	if dark := darkPixels(img); dark < 812*1218/50 {
		t.Errorf("only %d dark pixels; did anything draw?", dark)
	}
}

func TestZPLRenderLimitsLabels(t *testing.T) {
	two := []byte("^XA^FO10,10^GB50,50,50^FS^XZ^XA^FO10,10^GB50,50,50^FS^XZ")
	p, err := newZPLRenderer(203).render(context.Background(), two, size4x6, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Labels != 2 || len(p.Pages) != 1 {
		t.Fatalf("labels=%d pages=%d", p.Labels, len(p.Pages))
	}
	// the filled box is at the top left
	if c := color.GrayModel.Convert(decodePNG(t, p.Pages[0].PNG).At(30, 30)).(color.Gray); c.Y > 128 {
		t.Errorf("box pixel is %v, want black", c)
	}
}

// Malformed input must come back as a result or an error, never a panic or
// a hang: zebrash has no recover of its own.
func TestZPLRenderSurvivesGarbage(t *testing.T) {
	inputs := []string{
		"^XA^GFA,10,10,1,ZZZZ^XZ",
		"^XA^GFA,99999,99999,1,^XZ",
		"^XA^BD^FD^XZ",
		"^XA^B7N,,,,,^FD^XZ",
		"^XA^BQN,2,10^FD^XZ",
		"^XA^FO99999999,99999999^GB99999,99999,9999^FS^XZ",
		"^XA^A0N,0,0^FDx^FS^XZ",
		"^XA^FB-1,-1^FDx^FS^XZ",
		"^XA^XA^XA",
		"^XZ^XZ",
		"\x00\xff\x89PNG^XA^FD\x00^XZ",
		strings.Repeat("^XA^FDx^FS^XZ", 500),
	}
	z := newZPLRenderer(203)
	for _, in := range inputs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = z.render(ctx, []byte(in), size4x6, 12)
		cancel()
	}
	// every render released its slot
	if len(z.slots) != 0 {
		t.Fatalf("%d render slots still held", len(z.slots))
	}
}

func TestPreviewZPL(t *testing.T) {
	fake, h := newTestServer(t)
	data, _ := os.ReadFile("testdata/sample.zpl")
	rec, out := post(t, h, "/api/preview", "sample.zpl", data, map[string]string{"media": "oe_2x1-label_2x1in"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if out["kind"] != "zpl" || out["labels"] != 1.0 || out["previewError"] != nil {
		t.Fatalf("got %v", out)
	}
	// the zpl's own ^PW/^LL beat the 2×1 media
	if out["widthIn"] != 4.0 || out["heightIn"] != 6.0 {
		t.Fatalf("size %v × %v", out["widthIn"], out["heightIn"])
	}
	pages := out["pages"].([]any)
	if len(pages) != 1 || !strings.HasPrefix(pages[0].(map[string]any)["image"].(string), "data:image/png;base64,") {
		t.Fatalf("pages = %v", pages)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("preview printed")
	}

	// zebrash refusing a label (here a MaxiCode with a bad postcode) is a
	// missing preview, not a rejected file: the printer reads the zpl itself.
	bad := []byte("^XA^FO40,40^BD2^FD001840000000000000[)>_1E01_1D96X_1D_1E_04^FS^XZ")
	rec, out = post(t, h, "/api/preview", "bad.zpl", bad, nil)
	if rec.Code != 200 || out["kind"] != "zpl" || !strings.Contains(out["previewError"].(string), "maxicode") {
		t.Fatalf("status %d: %v", rec.Code, out)
	}
	rec, _ = post(t, h, "/api/print", "bad.zpl", bad, nil)
	if rec.Code != 200 || len(fake.printJobs()) != 1 || !bytes.Equal(fake.printJobs()[0].Data, bad) {
		t.Fatalf("print of an unpreviewable label: status %d", rec.Code)
	}

	// no ^PW/^LL: the chosen media sets the canvas
	rec, out = post(t, h, "/api/preview", "x.zpl", []byte("^XA^FO10,10^GB50,50,50^FS^XZ"), map[string]string{"media": "oe_2x1-label_2x1in"})
	if rec.Code != 200 || out["widthIn"] != 2.0 || out["heightIn"] != 1.0 {
		t.Fatalf("status %d: %v", rec.Code, out)
	}
}
