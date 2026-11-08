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

// outline is a w-px-thick rectangle outline drawn just outside r: top and
// bottom lines, plus the sides when sides is true.
func outline(r image.Rectangle, w int, sides bool) []image.Rectangle {
	o := r.Inset(-w)
	out := []image.Rectangle{
		image.Rect(o.Min.X, o.Min.Y, o.Max.X, r.Min.Y),
		image.Rect(o.Min.X, r.Max.Y, o.Max.X, o.Max.Y),
	}
	if sides {
		out = append(out, image.Rect(o.Min.X, r.Min.Y, r.Min.X, r.Max.Y), image.Rect(r.Max.X, r.Min.Y, o.Max.X, r.Max.Y))
	}
	return out
}

func TestDetectFramedBlock(t *testing.T) {
	// A 6 × 4 in label and a 1 in block of other ink 0.6 in beside it, with a
	// frame hugging both (2 px clear of them). The frame's lines cross the
	// gutter between them, so the 7.8 in block can't be cut until it's gone.
	label, other := image.Rect(40, 60, 340, 260), image.Rect(370, 100, 420, 150)
	inside := image.Rect(38, 58, 422, 262)
	for _, c := range []struct {
		name  string
		w     int
		sides bool
	}{
		{"1 px top and bottom", 1, false},
		{"2 px top and bottom", 2, false},
		{"1 px all round", 1, true},
		{"3 px all round", 3, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := maskOf(550, 425, append(outline(inside, c.w, c.sides), label, other)...)
			got, fallback := detect46(m)
			if fallback || len(got) != 1 {
				t.Fatalf("got %+v fallback %v", got, fallback)
			}
			if want := image.Rect(38, 58, 342, 262); got[0].box != want || got[0].tab != nil {
				t.Errorf("box = %v tab %v, want %v", got[0].box, got[0].tab, want)
			}
		})
	}
	// a 4 px border is a drawn box, not a hairline frame: left alone
	m := maskOf(550, 425, append(outline(inside, 4, true), label, other)...)
	if _, fallback := detect46(m); !fallback {
		t.Error("a 4 px (0.08 in) border must not be stripped")
	}
}

func TestDetectBorderedLabelsKeepTheirBorders(t *testing.T) {
	// Two 4 × 6 in labels, each outlined by a 1 px hairline, side by side.
	// Each is label-sized as it is, so its outline is part of it: stripping
	// it as a frame would shift the box by a pixel.
	var rects []image.Rectangle
	for _, x := range []int{40, 315} {
		r := image.Rect(x, 60, x+200, 360)
		rects = append(rects, outline(r.Inset(1), 1, true)...)
		rects = append(rects, image.Rect(x+20, 80, x+60, 120), image.Rect(x+20, 200, x+180, 260))
	}
	got, fallback := detect46(maskOf(550, 425, rects...))
	if fallback || len(got) != 2 {
		t.Fatalf("got %+v fallback %v", got, fallback)
	}
	for i, x := range []int{40, 315} {
		if want := image.Rect(x-2, 58, x+202, 362); got[i].box != want {
			t.Errorf("box %d = %v, want %v", i, got[i].box, want)
		}
	}
}

func TestDetectDocTabAtStart(t *testing.T) {
	// A label image with its doc tab first: a 0.76 in tab, a 0.94 in gutter,
	// then a 5.92 × 4.36 in label with a 0.18 in white band through it 1.5 in
	// from its end. Split at the band, the part before it is nearer 6 in than
	// the label, but what's left over (1.52 in) is too long to be a doc tab.
	// The label's ink stops 2 px short of the image's right edge, as real
	// labels' margins do: ink running into a frame's side makes that side
	// look thicker than a line, and it isn't stripped.
	img := image.Rect(18, 300, 402, 518)
	tab := image.Rect(18, 340, 56, 480)
	label := []image.Rectangle{image.Rect(104, 300, 315, 518), image.Rect(324, 300, 400, 518)}
	for _, framed := range []bool{false, true} {
		rects := append([]image.Rectangle{tab}, label...)
		name := "bare"
		if framed { // and a 1 px frame round the image, as an HTML page draws it
			rects, name = append(rects, outline(img, 1, true)...), "framed"
		}
		t.Run(name, func(t *testing.T) {
			got, fallback := detect46(maskOf(425, 550, rects...), img)
			if fallback || len(got) != 1 || got[0].tab == nil {
				t.Fatalf("got %+v fallback %v", got, fallback)
			}
			if want := image.Rect(102, 298, 402, 520); got[0].box != want {
				t.Errorf("box = %v, want %v (the whole label)", got[0].box, want)
			}
			if want := image.Rect(16, 338, 58, 482); *got[0].tab != want {
				t.Errorf("tab = %v, want %v", *got[0].tab, want)
			}
			if w := float64(got[0].tab.Dx()) / 50; w >= 1.2 {
				t.Errorf("tab is %.2f in long", w)
			}
		})
	}
}
