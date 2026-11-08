package main

import (
	"context"
	"image"
	"testing"
)

func TestCountImageRows(t *testing.T) {
	head := "page   num  type   width height color comp bpc  enc interp  object ID x-ppi y-ppi size ratio\n" +
		"--------------------------------------------------------------------------------------------\n"
	row := "   1     0 image     800  1200  gray    1   1  image  no        20  0   200   200 20.2K 2.2%\n"
	if n, err := countImageRows([]byte(head)); n != 0 || err != nil {
		t.Errorf("no images: %d %v", n, err)
	}
	if n, err := countImageRows([]byte(head + row + row)); n != 2 || err != nil {
		t.Errorf("two: %d %v", n, err)
	}
	big := "   1     0 image    5000  1200  gray    1   1  image  no        20  0   200   200 20.2K 2.2%\n"
	if _, err := countImageRows([]byte(head + big)); err == nil {
		t.Error("an image over 4000 px must fail the gate")
	}
	var many string
	for range 17 {
		many += row
	}
	if _, err := countImageRows([]byte(head + many)); err == nil {
		t.Error("17 images must fail the gate")
	}
}

func TestParseHTMLImages(t *testing.T) {
	g := pageGeom{Media: letter, Crop: letter}
	xml := []byte(`<page number="1" position="absolute" top="0" left="0" height="2376" width="1836">
<text top="10" left="10">a &amp; b <b>broken</text>
<image top="864" left="106" width="1465" height="882" src="/elsewhere/p-1_1.png"/>
<image top="756" left="0" width="432" height="-755" src="p-1_2.png"/>
</page>`)
	imgs, err := parseHTMLImages(xml, g, "/scratch")
	if err != nil || len(imgs) != 2 {
		t.Fatalf("%v %v", imgs, err)
	}
	r := imgs[0].rect
	if !near(r.X, 106.0/3/612, 1e-9) || !near(r.Y, 864.0/3/792, 1e-9) || !near(r.W, 1465.0/3/612, 1e-9) {
		t.Errorf("rect %+v", r)
	}
	if imgs[0].path != "/scratch/p-1_1.png" {
		t.Errorf("path %q: must stay inside the scratch dir", imgs[0].path)
	}
	if r := imgs[1].rect; !near(r.Y, 1.0/3/792, 1e-9) || !near(r.H, 755.0/3/792, 1e-9) {
		t.Errorf("negative height not normalised: %+v", r)
	}
}

// pdftohtml positions are MediaBox-relative; pageImages must return them
// crop-relative for every /Rotate.
func TestPageImagesCropRelative(t *testing.T) {
	for _, rot := range []int{0, 90, 180, 270} {
		var b pdfBuilder
		p := b.page([4]float64{10, 20, 622, 812}, rot)
		p.crop = [4]float64{46, 70, 510, 720}
		p.image(labelBitmap(200, 300), 1, 1.5, 2, 3, 0)
		doc := openTest(t, b.bytes())
		imgs := doc.pageImages(context.Background(), 1)
		if len(imgs) != 1 {
			t.Fatalf("rot %d: %d images", rot, len(imgs))
		}
		pw, ph := doc.geom(1).inches()
		r := imgs[0].rect
		if !near(r.X*pw, 1, 0.02) || !near(r.Y*ph, 1.5, 0.02) || !near(r.W*pw, 2, 0.02) || !near(r.H*ph, 3, 0.02) {
			t.Errorf("rot %d: image at %.3f,%.3f %.3fx%.3f in, want 1,1.5 2x3",
				rot, r.X*pw, r.Y*ph, r.W*pw, r.H*ph)
		}
	}
}

// assertSourcePixels checks a native label is the bitmap's pixels, unscaled,
// at offset (ox, oy) on the canvas.
func assertSourcePixels(t *testing.T, g *image.Gray, src pdfImage, ox, oy int) {
	t.Helper()
	for y := range src.h {
		for x := range src.w {
			want := uint8(255)
			if src.black(x, y) {
				want = 0
			}
			if got := g.Pix[(y+oy)*g.Stride+x+ox]; got != want {
				t.Fatalf("pixel (%d,%d) = %d, want %d: not the source bitmap", x, y, got, want)
			}
		}
	}
}

func TestNativeUPS2up(t *testing.T) {
	for _, crop := range []bool{false, true} {
		doc := openTest(t, fixUPS2up(crop, 200))
		pw, ph := doc.geom(1).inches()
		g, ppi, reason := doc.native(context.Background(), 1, boxIn(pw, ph, 0.7, 1.2, 4.1, 6.1), canvas46)
		if g == nil {
			t.Fatalf("crop %v: not native: %s", crop, reason)
		}
		if !near(ppi, 200, 2) {
			t.Errorf("ppi %g", ppi)
		}
		assertPureLabel(t, g, canvas46)
		assertSourcePixels(t, g, labelBitmap(800, 1200), 6, 9)
	}
}

func TestNativeRefuses(t *testing.T) {
	cases := []struct {
		name string
		pdf  []byte
		b    func(pw, ph float64) box
	}{
		{"300 ppi", fixUPS2up(false, 300), func(pw, ph float64) box { return boxIn(pw, ph, 0.7, 1.2, 2.8, 4.1) }},
		{"stamped", fixStamped(), func(pw, ph float64) box { return boxIn(pw, ph, 2.2, 2.45, 4.1, 6.1) }},
		{"two images in the box", fixUPS2up(false, 200), func(pw, ph float64) box { return box{0, 0, 1, 1} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := openTest(t, c.pdf)
			pw, ph := doc.geom(1).inches()
			if g, _, _ := doc.native(context.Background(), 1, c.b(pw, ph), canvas46); g != nil {
				t.Fatal("passthrough must refuse this")
			}
		})
	}
}

func TestNativeDocTabShapes(t *testing.T) {
	// fixture 8: browser-printed, 206 × 196 ppi, RGB, blank tab
	doc := openTest(t, fixReturn(0, 206, 196))
	g, _, reason := doc.native(context.Background(), 1, boxIn(8.5, 11, 0.47, 4.47, 5.9, 4.15), canvas46)
	if g == nil {
		t.Fatalf("fixture 8: %s", reason)
	}
	assertPureLabel(t, g, canvas46)

	// fixture 9: 4 × 6 page, the image turned and clipped by the page edge
	doc = openTest(t, fixClippedTab())
	g, _, reason = doc.native(context.Background(), 1, box{0, 0, 1, 1}, canvas46)
	if g == nil {
		t.Fatalf("fixture 9: %s", reason)
	}
	assertPureLabel(t, g, canvas46)
	// the corner block, turned clockwise with the image, sits top right
	if g.Pix[(9+40)*g.Stride+6+760] != 0 || g.Pix[(9+40)*g.Stride+6+40] != 255 {
		t.Error("fixture 9: label isn't oriented as drawn")
	}
}
