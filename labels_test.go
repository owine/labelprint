package main

import (
	"context"
	"image"
	"testing"
)

func openTest(t *testing.T, pdf []byte) *pdfDoc {
	t.Helper()
	requirePoppler(t)
	doc, err := newPDFRenderer(203, discardLog).open(context.Background(), pdf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(doc.Close)
	return doc
}

var canvas46 = canvas{W: 812, H: 1218}

// boxIn is a box from inches on a page of the given displayed size.
func boxIn(pw, ph, x, y, w, h float64) box { return box{x / pw, y / ph, w / pw, h / ph} }

func assertPureLabel(t *testing.T, g *image.Gray, cv canvas) {
	t.Helper()
	if g.Rect.Dx() != cv.W || g.Rect.Dy() != cv.H {
		t.Fatalf("label is %dx%d, want %dx%d", g.Rect.Dx(), g.Rect.Dy(), cv.W, cv.H)
	}
	for _, v := range g.Pix {
		if v != 0 && v != 255 {
			t.Fatalf("pixel value %d: labels must be pure black/white", v)
		}
	}
}

func TestCanvasFor(t *testing.T) {
	m, _ := mediaSize("na_index-4x6_4x6in")
	if c := canvasFor(m, true, 203); c != canvas46 {
		t.Fatalf("got %+v", c)
	}
	if canvasFor(m, false, 203).known() {
		t.Fatal("no media, no canvas")
	}
}

func TestRuns(t *testing.T) {
	got := runs([]int{1, 2, 3, 7, 9, 10})
	want := [][2]int{{1, 3}, {7, 7}, {9, 10}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v", got)
		}
	}
}

func TestRenderPages(t *testing.T) {
	doc := openTest(t, makeLabelPDF(3))
	out, err := doc.renderPages(context.Background(), []int{1, 3}, 50, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1] == nil || out[3] == nil {
		t.Fatalf("pages %v", len(out))
	}
	img, err := decodePNGBytes(out[3])
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 200 || b.Dy() != 300 {
		t.Fatalf("page 3 at 50 dpi is %v", b)
	}
}

func TestRenderLabelTurnsLandscapeLabels(t *testing.T) {
	doc := openTest(t, fix2up())
	// the top 6 × 4 in label, with a little margin
	g, err := doc.renderLabel(context.Background(), 1, boxIn(8.5, 11, 1.2, 0.7, 6.1, 4.1), canvas46)
	if err != nil {
		t.Fatal(err)
	}
	assertPureLabel(t, g, canvas46)
	// the corner block (top-left of the label as drawn) ends up top-right
	if g.Pix[75*g.Stride+742] != 0 {
		t.Error("corner block isn't at the top right after turning clockwise")
	}
	if g.Pix[75*g.Stride+70] != 255 {
		t.Error("top left should be blank")
	}
}

func TestRenderLabelShrinksToFit(t *testing.T) {
	doc := openTest(t, fixWide()) // 6.25 × 4 in
	g, err := doc.renderLabel(context.Background(), 1, box{0, 0, 1, 1}, canvas46)
	if err != nil {
		t.Fatal(err)
	}
	assertPureLabel(t, g, canvas46)
}
