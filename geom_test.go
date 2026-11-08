package main

import (
	"context"
	"math"
	"strings"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestHTMLOffsetTable(t *testing.T) {
	// The boxes the design doc's table was checked with against real renders.
	m, c := [4]float64{10, 20, 622, 812}, [4]float64{46, 70, 510, 720}
	want := map[int][2]float64{0: {36, 92}, 90: {50, 36}, 180: {112, 50}, 270: {92, 112}}
	for rot, w := range want {
		x, y := pageGeom{Media: m, Crop: c, Rot: rot}.htmlOffset()
		if x != w[0] || y != w[1] {
			t.Errorf("rot %d: offset (%g, %g), want (%g, %g)", rot, x, y, w[0], w[1])
		}
	}
}

func TestParsePageGeoms(t *testing.T) {
	out := []byte(`Pages:          2
Page    1 size:     612.00 x 792.00 pts (letter)
Page    1 rot:      90
Page    1 MediaBox:     0.00     0.00   612.00   792.00
Page    1 CropBox:     36.00    36.00   576.00   756.00
Page    2 size:     288.00 x 432.00 pts
Page    2 rot:      0
Page    2 MediaBox:     0.00     0.00   288.00   432.00
Page    2 CropBox:      0.00     0.00   288.00   432.00
`)
	g, err := parsePageGeoms(out, 2)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := g[0].size(); w != 720 || h != 540 {
		t.Errorf("page 1 displayed %gx%g, want 720x540 (crop box, turned)", w, h)
	}
	if w, h := g[1].inches(); w != 4 || h != 6 {
		t.Errorf("page 2 %gx%g in", w, h)
	}
	if _, err := parsePageGeoms(out, 3); err == nil {
		t.Error("a page with no boxes must be an error")
	}
}

func TestOpenReadsGeometry(t *testing.T) {
	requirePoppler(t)
	r := newPDFRenderer(203, discardLog)
	doc, err := r.open(context.Background(), fixUPS2up(true, 200))
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if w, h := doc.geom(1).inches(); !near(w, 10.5, 1e-9) || !near(h, 8, 1e-9) {
		t.Fatalf("displayed %gx%g in, want 10.5x8", w, h)
	}
	if doc.geom(1).Rot != 270 {
		t.Fatalf("rot %d", doc.geom(1).Rot)
	}
}

func TestBoxPixels(t *testing.T) {
	b := box{X: 0.1, Y: 0.25, W: 0.5, H: 0.5}
	if r := b.pixels(100, 200); r.Min.X != 10 || r.Min.Y != 50 || r.Max.X != 60 || r.Max.Y != 150 {
		t.Fatalf("got %v", r)
	}
	if i, ok := b.intersect(box{X: 0.5, Y: 0, W: 1, H: 1}); !ok || !near(i.W, 0.1, 1e-12) {
		t.Fatalf("intersect = %+v %v", i, ok)
	}
}

func TestParseCrops(t *testing.T) {
	inch := func(int) (float64, float64) { return 8.5, 11 }
	sel := []int{1, 2}
	got, err := parseCrops(`{"1": [[0.1, 0.1, 0.5, 0.5]], "7": [[0,0,0.01,0.01]]}`, sel, inch)
	if err != nil || len(got) != 1 || got[1][0] != (box{0.1, 0.1, 0.5, 0.5}) {
		t.Fatalf("%v %v (page 7 isn't selected: ignored, even though invalid)", got, err)
	}
	if got, err := parseCrops("", sel, inch); got != nil || err != nil {
		t.Fatal("empty means none")
	}
	bad := map[string]string{
		"not json":   `{`,
		"page key":   `{"x": [[0,0,1,1]]}`,
		"empty list": `{"1": []}`,
		"three nums": `{"1": [[0,0,1]]}`,
		"off page":   `{"1": [[0.6,0,0.5,0.5]]}`,
		"too small":  `{"1": [[0,0,0.05,0.5]]}`,
		"nine boxes": `{"1": [` + strings.Repeat(`[0,0,0.1,0.1],`, 8) + `[0,0,0.1,0.1]]}`,
		"leading 0":  `{"01": [[0,0,1,1]]}`,
		"plus sign":  `{"+1": [[0,0,1,1]]}`,
		"spaces":     `{" 1": [[0,0,1,1]]}`,
		// one page spelled three ways: 17 boxes on page 1
		"aliases": `{"1": [` + strings.Repeat(`[0,0,1,1],`, 7) + `[0,0,1,1]], "01": [` +
			strings.Repeat(`[0,0,1,1],`, 7) + `[0,0,1,1]], "+1": [[0,0,1,1]]}`,
	}
	for name, v := range bad {
		if _, err := parseCrops(v, sel, inch); err == nil {
			t.Errorf("%s: no error", name)
		} else if ue, ok := err.(*userError); !ok || ue.Field != "crops" {
			t.Errorf("%s: error %v isn't tagged crops", name, err)
		}
	}
}

func TestParseLayout(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "auto": "auto", "page": "page"} {
		if got, err := parseLayout(in); got != want || err != nil {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := parseLayout("x"); err == nil {
		t.Error("unknown layout accepted")
	}
}
