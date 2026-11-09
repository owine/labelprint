package main

import (
	"context"
	"errors"
	"fmt"
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
	got, _, err := parseCrops(`{"1": [[0.1, 0.1, 0.5, 0.5]], "7": [[0,0,0.01,0.01]]}`, sel, inch, docNoun{})
	if err != nil || len(got) != 1 || got[1][0] != (box{0.1, 0.1, 0.5, 0.5}) {
		t.Fatalf("%v %v (page 7 isn't selected: ignored, even though invalid)", got, err)
	}
	if got, flips, err := parseCrops("", sel, inch, docNoun{}); got != nil || flips != nil || err != nil {
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
		if _, _, err := parseCrops(v, sel, inch, docNoun{}); err == nil {
			t.Errorf("%s: no error", name)
		} else if ue, ok := err.(*userError); !ok || ue.Field != "crops" {
			t.Errorf("%s: error %v isn't tagged crops", name, err)
		}
	}
}

// A fifth number flips a box's label 180°: 1 flips it, 0 is the same as
// leaving the number out. The flips come back indexed like the boxes, and nil
// when nothing is flipped, so a request without them is planned as before.
func TestParseCropsFlips(t *testing.T) {
	inch := func(int) (float64, float64) { return 8.5, 11 }
	sel := []int{1, 2}
	boxes, flips, err := parseCrops(`{"1": [[0.1, 0.1, 0.5, 0.4], [0.1, 0.5, 0.5, 0.4, 0]]}`, sel, inch, docNoun{})
	if err != nil || flips != nil || len(boxes[1]) != 2 {
		t.Fatalf("nothing flipped: boxes %v flips %v err %v; want 2 boxes and nil flips", boxes, flips, err)
	}
	boxes, flips, err = parseCrops(`{"1": [[0.1, 0.1, 0.5, 0.4], [0.1, 0.5, 0.5, 0.4, 1], [0.6, 0.1, 0.3, 0.4, 0]],`+
		` "2": [[0, 0, 1, 1]]}`, sel, inch, docNoun{})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(flips); got != "map[1:[false true false] 2:[false]]" {
		t.Errorf("flips = %s, want map[1:[false true false] 2:[false]]", got)
	}
	if boxes[1][1] != (box{0.1, 0.5, 0.5, 0.4}) {
		t.Errorf("a flipped box's geometry = %+v, want {0.1 0.5 0.5 0.4}", boxes[1][1])
	}
	for v, want := range map[string]string{
		`{"1": [[0, 0, 1, 1, 2]]}`:    "box 1 on page 1 has a bad flip value",
		`{"1": [[0, 0, 1, 1, -1]]}`:   "box 1 on page 1 has a bad flip value",
		`{"1": [[0, 0, 1, 1, 0.5]]}`:  "box 1 on page 1 has a bad flip value",
		`{"1": [[0, 0, 1, 1, 1, 0]]}`: "box 1 on page 1 needs 4 or 5 numbers",
	} {
		_, _, err := parseCrops(v, sel, inch, docNoun{})
		var ue *userError
		if !errors.As(err, &ue) || ue.Status != 400 || ue.Field != "crops" || ue.Msg != want {
			t.Errorf("%s: %v, want a 400 on crops: %q", v, err, want)
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

// The page sends fractions rounded to 4 decimals, so a box at the page's edge
// can come back with x + w = 1.0001. That's on the page (and clamped); a box
// really past the edge isn't.
func TestCropsAtThePageEdge(t *testing.T) {
	inch := func(int) (float64, float64) { return 8.5, 11 }
	got, _, err := parseCrops(`{"1": [[0.5, 0.4, 0.5001, 0.6001]]}`, []int{1}, inch, docNoun{})
	if err != nil {
		t.Fatal(err)
	}
	if b := got[1][0]; b.X+b.W > 1 || b.Y+b.H > 1 {
		t.Errorf("not clamped to the page: %+v", b)
	}
	for _, b := range []box{{-0.0001, 0, 0.5, 0.5}, {0.5, 0.5, 0.50015, 0.5}} {
		if !b.valid() {
			t.Errorf("%+v: rounding slack refused", b)
		}
	}
	for _, b := range []box{{-0.001, 0, 0.5, 0.5}, {0.5, 0.5, 0.501, 0.5}} {
		if b.valid() {
			t.Errorf("%+v: off the page, accepted", b)
		}
	}
}

// An image is one page to the pipeline, and its messages say "the image".
func TestParseCropsNamesTheImage(t *testing.T) {
	inch := func(int) (float64, float64) { return 4, 6 }
	for v, want := range map[string]string{
		`{"1": [[0.6, 0, 0.5, 0.5]]}`: "box 1 on the image runs off the image",
		`{"1": [[0, 0, 0.05, 0.5]]}`:  "box 1 on the image is too small (under 0.5 in)",
		`{"1": [[0, 0, 1]]}`:          "box 1 on the image needs 4 or 5 numbers",
		`{"1": []}`:                   "the image has no label boxes",
	} {
		_, _, err := parseCrops(v, []int{1}, inch, docNoun{image: true})
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", v, err, want)
		}
	}
	_, _, err := parseCrops(`{"1": [[0.6, 0, 0.5, 0.5]]}`, []int{1}, inch, docNoun{})
	if err == nil || err.Error() != "box 1 on page 1 runs off the page" {
		t.Errorf("pdf wording changed: %v", err)
	}
}
