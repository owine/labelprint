package main

import (
	"image"
	"testing"
)

// maskOf is a w×h mask with the given rectangles inked solid.
func maskOf(w, h int, rects ...image.Rectangle) *mask {
	m := &mask{w: w, h: h, ink: make([]bool, w*h)}
	for _, r := range rects {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				m.ink[y*w+x] = true
			}
		}
	}
	return m
}

func detect46(m *mask, images ...image.Rectangle) ([]found, bool) {
	d := &detector{m: m, dpi: 50, mediaW: 4, mediaH: 6, imageRects: func() []image.Rectangle { return images }}
	return d.detect()
}

func TestDetectTwoLabels(t *testing.T) {
	// Letter landscape (550×425): two 4×6 labels with 1.5 in between
	m := maskOf(550, 425, image.Rect(40, 60, 240, 360), image.Rect(315, 60, 515, 360))
	got, fallback := detect46(m)
	if fallback || len(got) != 2 {
		t.Fatalf("got %d boxes, fallback %v", len(got), fallback)
	}
	if want := image.Rect(38, 58, 242, 362); got[0].box != want { // padded 2 px
		t.Errorf("box 0 = %v, want %v", got[0].box, want)
	}
}

func TestDetectDropsSmallBlocks(t *testing.T) {
	// a label and a 2 × 0.4 in block of text beside it
	m := maskOf(550, 425, image.Rect(40, 60, 240, 360), image.Rect(300, 100, 400, 120))
	got, fallback := detect46(m)
	if fallback || len(got) != 1 {
		t.Fatalf("got %+v fallback %v", got, fallback)
	}
}

func TestDetectKeepsALabelWhole(t *testing.T) {
	// one label made of two halves with a 0.4 in white band between: it's
	// label-sized as a whole, so it must not be split
	m := maskOf(300, 400, image.Rect(50, 50, 250, 190), image.Rect(50, 210, 250, 350))
	got, _ := detect46(m)
	if len(got) != 1 || got[0].box != image.Rect(48, 48, 252, 352) {
		t.Fatalf("got %+v", got)
	}
}

func TestDetectFallback(t *testing.T) {
	// two lines of text 6 in apart: 5 × 6.2 in as a block, too tall to be a label
	text := maskOf(425, 550, image.Rect(50, 50, 300, 60), image.Rect(50, 350, 300, 360))
	got, fallback := detect46(text)
	if !fallback || len(got) != 1 || got[0].box != image.Rect(48, 48, 302, 362) {
		t.Fatalf("text only: %+v %v", got, fallback)
	}
	got, fallback = detect46(maskOf(100, 100))
	if !fallback || got[0].box != image.Rect(0, 0, 100, 100) {
		t.Fatalf("blank: %+v %v", got, fallback)
	}
	// nine labels in a grid is not a sheet this is for
	var rects []image.Rectangle
	for i := range 9 {
		x, y := 10+(i%3)*130, 10+(i/3)*190
		rects = append(rects, image.Rect(x, y, x+120, y+180))
	}
	if _, fallback := detect46(maskOf(400, 580, rects...)); !fallback {
		t.Fatal("more than 8 labels should fall back")
	}
	d := &detector{m: maskOf(300, 400, image.Rect(50, 50, 250, 350)), dpi: 50} // unknown media
	if _, fallback := d.detect(); !fallback {
		t.Fatal("unknown media always falls back")
	}
}

func TestDetectDocTab(t *testing.T) {
	// a 6 × 4 in label, a 0.2 in gutter, then a 0.8 in tab: 7 in long (1.17×)
	label, tab := image.Rect(10, 10, 310, 210), image.Rect(320, 60, 360, 160)
	m := maskOf(400, 300, label, tab)
	img := image.Rect(5, 5, 380, 215)

	got, _ := detect46(m, img)
	if len(got) != 1 || got[0].tab == nil {
		t.Fatalf("bitmap-covered: %+v", got)
	}
	if got[0].box != image.Rect(8, 8, 312, 212) {
		t.Errorf("box = %v", got[0].box)
	}
	if *got[0].tab != image.Rect(318, 58, 362, 162) {
		t.Errorf("tab = %v", *got[0].tab)
	}

	got, _ = detect46(m) // no bitmap: one oversize label, no split
	if len(got) != 1 || got[0].tab != nil || got[0].box != image.Rect(8, 8, 362, 212) {
		t.Fatalf("vector: %+v", got)
	}

	narrow := maskOf(400, 300, label, image.Rect(314, 60, 354, 160)) // 0.08 in gutter
	if got, _ := detect46(narrow, img); got[0].tab != nil {
		t.Fatal("a gutter under 0.1 in must not split")
	}
}
