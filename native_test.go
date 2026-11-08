package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageListSizes(t *testing.T) {
	head := "page   num  type   width height color comp bpc  enc interp  object ID x-ppi y-ppi size ratio\n" +
		"--------------------------------------------------------------------------------------------\n"
	row := "   1     0 image     800  1200  gray    1   1  image  no        20  0   200   200 20.2K 2.2%\n"
	if s, err := imageListSizes([]byte(head)); len(s) != 0 || err != nil {
		t.Errorf("no images: %v %v", s, err)
	}
	if s, err := imageListSizes([]byte(head + row + row)); len(s) != 2 || s[0] != image.Pt(800, 1200) || err != nil {
		t.Errorf("two: %v %v", s, err)
	}
	big := "   1     0 image    5000  1200  gray    1   1  image  no        20  0   200   200 20.2K 2.2%\n"
	if _, err := imageListSizes([]byte(head + big)); err == nil {
		t.Error("an image over 4000 px must fail the gate")
	}
	var many string
	for range 17 {
		many += row
	}
	if _, err := imageListSizes([]byte(head + many)); err == nil {
		t.Error("17 images must fail the gate")
	}
	// It's a safety gate: what it can't read refuses rather than being skipped.
	for name, out := range map[string]string{
		"short row":   head + "   1     0 image\n",
		"odd width":   head + "   1     0 image     8e2  1200  gray    1   1  image  no        20  0   200   200 20.2K 2.2%\n",
		"no header":   row,
		"empty":       "",
		"zero height": head + "   1     0 image     800     0  gray    1   1  image  no        20  0   200   200 20.2K 2.2%\n",
	} {
		if _, err := imageListSizes([]byte(out)); err == nil {
			t.Errorf("%s: must fail the gate", name)
		}
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

func assertGray(t *testing.T, got, want *image.Gray, what string) {
	t.Helper()
	if got.Rect != want.Rect {
		t.Fatalf("%s: %v, want %v", what, got.Rect, want.Rect)
	}
	for i := range got.Pix {
		if got.Pix[i] != want.Pix[i] {
			w := got.Rect.Dx()
			t.Fatalf("%s: pixel (%d,%d) = %d, want %d", what, i%w, i/w, got.Pix[i], want.Pix[i])
		}
	}
}

// Every one of the 8 placements is accepted, as exactly the bitmap as
// displayed: whole pixels or a fractional offset, and up against the page's
// right and bottom edges (where poppler's render loses the last row and
// column).
func TestNativeAllPlacements(t *testing.T) {
	src := labelBitmap(400, 600) // 2 × 3 in at 200 ppi
	for _, at := range [][2]float64{{1, 1}, {1.013, 0.987}} {
		for turns := range 4 {
			for _, flip := range []bool{false, true} {
				t.Run(fmt.Sprintf("at %v, turns %d, mirror %v", at, turns, flip), func(t *testing.T) {
					t.Parallel()
					var b pdfBuilder
					p := b.page(letter, 0)
					w, h := 2.0, 3.0
					if turns%2 == 1 {
						w, h = 3, 2
					}
					p.imageAs(src, at[0], at[1], w, h, flip, turns)
					doc := openTest(t, b.bytes())
					g, _, reason := doc.native(context.Background(), 1, boxIn(8.5, 11, at[0]-0.05, at[1]-0.05, w+0.1, h+0.1), canvas46)
					if g == nil {
						t.Fatalf("refused: %s", reason)
					}
					o := src.gray()
					if flip {
						o = mirror(o)
					}
					assertGray(t, g, fitCanvas(rotate(o, turns), canvas46), "label")
				})
			}
		}
	}
	for _, c := range []struct {
		w, h   int
		pw, ph float64
		cv     canvas
	}{{800, 1200, 4, 6, canvas46}, {1700, 2200, 8.5, 11, canvas{W: 1726, H: 2233}}} {
		t.Run(fmt.Sprintf("%d×%d filling the page", c.w, c.h), func(t *testing.T) {
			t.Parallel()
			var b pdfBuilder
			im := labelBitmap(c.w, c.h)
			b.page(inches(c.pw, c.ph), 0).image(im, 0, 0, c.pw, c.ph, 0)
			g, _, reason := openTest(t, b.bytes()).native(context.Background(), 1, box{0, 0, 1, 1}, c.cv)
			if g == nil {
				t.Fatalf("refused: %s", reason)
			}
			assertGray(t, g, fitCanvas(im.gray(), c.cv), "label")
		})
	}
}

func TestNativeRefuses(t *testing.T) {
	label := func(draw func(p *pdfPage)) []byte {
		var b pdfBuilder
		p := b.page(letter, 0)
		im := labelBitmap(800, 1200)
		im.fill(400, 100, 700, 300) // a solid block for a white box to hide
		p.image(im, 1, 1, 4, 6, 0)
		draw(p)
		return b.bytes()
	}
	whiteBox := func(x, y, side float64) []byte {
		return label(func(p *pdfPage) {
			p.ops.WriteString("1 g\n")
			p.fillRect(x, y, side, side)
			p.ops.WriteString("0 g\n")
		})
	}
	labelBox := func(pw, ph float64) box { return boxIn(pw, ph, 0.95, 0.95, 4.1, 6.1) }
	// A corner of the old 32 px tile grid, which starts at the box's top-left
	// in the check render (200 ppi here): a spot centred on it used to split
	// into four tiles that each passed.
	br := labelBox(8.5, 11).pixels(8.5*200, 11*200)
	cx, cy := br.Min.X+4*32, br.Min.Y+4*32 // page pixels
	mark := func(white bool) []byte {
		var b pdfBuilder
		p := b.page(letter, 0)
		im := labelBitmap(800, 1200)
		ix, iy := cx-200, cy-200 // the bitmap sits at 1 in: 200 px
		if white {
			im.fill(ix-10, iy-10, ix+10, iy+10) // ink for an 8 × 8 white box to hide
		}
		p.image(im, 1, 1, 4, 6, 0)
		if white {
			p.ops.WriteString("1 g\n")
		}
		p.fillRect(float64(cx-4)/200, float64(cy-4)/200, 8.0/200, 8.0/200)
		p.ops.WriteString("0 g\n")
		return b.bytes()
	}
	var blank pdfBuilder
	blank.page(letter, 0).image(newBitmap(800, 1200), 1, 1, 4, 6, 0)
	cases := []struct {
		name, reason string
		pdf          []byte
		b            func(pw, ph float64) box
	}{
		{"300 ppi", "resolution", fixUPS2up(false, 300), func(pw, ph float64) box { return boxIn(pw, ph, 0.7, 1.2, 2.8, 4.1) }},
		{"stamped", "doesn't match the render (", fixStamped(), func(pw, ph float64) box { return boxIn(pw, ph, 2.2, 2.45, 4.1, 6.1) }},
		{"two images in the box", "covers", fixUPS2up(false, 200), func(pw, ph float64) box { return box{0, 0, 1, 1} }},
		// a line of 4 pt text under the bitmap, inside the box: passthrough would drop it
		{"text beside", "ink beside the image", label(func(p *pdfPage) { p.text(1.05, 7.2, 4, "REF 1Z999AA10123456784") }),
			func(pw, ph float64) box { return boxIn(pw, ph, 0.97, 0.97, 4.06, 6.3) }},
		// 4 pt text over a white part of the bitmap: too little ink to fail on share
		{"text over", "in one spot", label(func(p *pdfPage) { p.text(1.5, 2.5, 4, "REF 1Z999AA10123456784") }), labelBox},
		// white boxes hiding part of the bitmap, which passthrough would print
		{"0.05 in white-out", "in one spot", whiteBox(3, 1.5, 0.05), labelBox},
		{"0.2 in white-out", "in one spot", whiteBox(3, 1.5, 0.2), labelBox},
		{"8 px mark on a grid corner", "in one spot", mark(false), labelBox},
		{"8 px white-out on a grid corner", "in one spot", mark(true), labelBox},
		{"blank image", "no ink", blank.bytes(), labelBox},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			doc := openTest(t, c.pdf)
			pw, ph := doc.geom(1).inches()
			g, _, reason := doc.native(context.Background(), 1, c.b(pw, ph), canvas46)
			if g != nil {
				t.Fatal("passthrough must refuse this")
			}
			if !strings.Contains(reason, c.reason) {
				t.Errorf("refused because %q, want %q", reason, c.reason)
			}
		})
	}
}

// A 300 ppi full-page scan (the USPS shape) is refused on its header's size,
// without decoding it.
func TestNativeRefusesBeforeDecoding(t *testing.T) {
	var b pdfBuilder
	b.page(letter, 0).image(labelBitmap(2550, 3300), 0, 0, 8.5, 11, 0)
	doc := openTest(t, b.bytes())
	imgs := doc.pageImages(context.Background(), 1)
	if len(imgs) != 1 || imgs[0].size != image.Pt(2550, 3300) {
		t.Fatalf("images %+v", imgs)
	}
	if err := os.WriteFile(imgs[0].path, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, reason := doc.native(context.Background(), 1, boxIn(8.5, 11, 1, 1, 4, 6), canvas46)
	if !strings.Contains(reason, "resolution") {
		t.Errorf("refused because %q: it decoded the image", reason)
	}
}

// pdftohtml copies a DCT stream out as is, so a JPEG whose header claims
// 60000 × 60000 behind a 100 × 100 dictionary must never be decoded.
func TestNativeJPEGBomb(t *testing.T) {
	var j bytes.Buffer
	if err := jpeg.Encode(&j, newWhite(100, 100), nil); err != nil {
		t.Fatal(err)
	}
	data := j.Bytes()
	i := bytes.Index(data, []byte{0xFF, 0xC0}) // SOF0: length(2) precision(1) height(2) width(2)
	data[i+5], data[i+6], data[i+7], data[i+8] = 0xEA, 0x60, 0xEA, 0x60
	var b pdfBuilder
	b.page(letter, 0).image(pdfImage{w: 100, h: 100, kind: "dct", dct: data}, 1, 1, 0.5, 0.5, 0)
	doc := openTest(t, b.bytes())
	if imgs := doc.pageImages(context.Background(), 1); len(imgs) != 0 {
		t.Fatalf("kept %+v", imgs)
	}
	// The test is only worth something if pdftohtml really wrote the bomb.
	files, _ := filepath.Glob(filepath.Join(doc.dir, "html-*", "*.jpg"))
	if len(files) != 1 {
		t.Fatalf("pdftohtml wrote %v", files)
	}
	if sz, err := imageFileSize(files[0]); err != nil || sz != image.Pt(60000, 60000) {
		t.Fatalf("written jpeg is %v %v", sz, err)
	}
	if g, _, reason := doc.native(context.Background(), 1, boxIn(8.5, 11, 1, 1, 0.5, 0.5), canvas46); g != nil || !strings.Contains(reason, "covers") {
		t.Errorf("native %v %q", g != nil, reason)
	}
}

func TestCompareRender(t *testing.T) {
	label := labelBitmap(200, 300).gray()
	ref := newWhite(240, 340)
	draw.Draw(ref, label.Rect.Add(image.Pt(20, 20)), label, image.Point{}, draw.Src)
	at := image.Pt(20, 20)

	if m := compareRender(ref, label, at); m.diff != 0 || m.tile != 0 || m.stray || m.refusal() != "" {
		t.Errorf("identical: %+v", m)
	}
	if m := compareRender(ref, mirror(label), at); m.diff < 0.5 || m.refusal() == "" {
		t.Errorf("mirrored: %+v", m)
	}
	if m := compareRender(newWhite(240, 340), newWhite(200, 300), at); !math.IsInf(m.diff, 1) || !strings.Contains(m.refusal(), "no ink") {
		t.Errorf("empty: %+v", m)
	}
	// ink in the render 3 px outside where the crop sits
	stray := subGray(ref, ref.Rect)
	stray.Pix[10*stray.Stride+120] = 0
	if m := compareRender(stray, label, at); !m.stray || !strings.Contains(m.refusal(), "beside") {
		t.Errorf("stray ink: %+v", m)
	}
	// an 8 × 8 hole in the corner block: a tiny share, but all in one tile
	hole := subGray(ref, ref.Rect)
	for y := 40; y < 48; y++ {
		for x := 40; x < 48; x++ {
			hole.Pix[y*hole.Stride+x] = 255
		}
	}
	if m := compareRender(hole, label, at); m.diff >= verifyMaxDiff || m.tile <= verifyTileMax || !strings.Contains(m.refusal(), "in one spot") {
		t.Errorf("hole: %+v", m)
	}
}

func TestNativeDocTabShapes(t *testing.T) {
	// fixture 8: browser-printed, 206 × 196 ppi, RGB, blank tab. The crop is
	// the 1202 px label's ink plus the 4 px margin (clamped to the bitmap),
	// turned to portrait.
	doc := openTest(t, fixReturn(0, 206, 196))
	g, _, reason := doc.native(context.Background(), 1, boxIn(8.5, 11, 0.47, 4.47, 5.9, 4.15), canvas46)
	if g == nil {
		t.Fatalf("fixture 8: %s", reason)
	}
	assertGray(t, g, fitCanvas(subGray(docTabBitmap(0).gray(), image.Rect(0, 0, 1206, 800)), canvas46), "fixture 8")

	// fixture 9: 4 × 6 page, the image turned clockwise and clipped by the
	// page's bottom edge, which cuts off the blank tab
	doc = openTest(t, fixClippedTab())
	g, _, reason = doc.native(context.Background(), 1, box{0, 0, 1, 1}, canvas46)
	if g == nil {
		t.Fatalf("fixture 9: %s", reason)
	}
	assertGray(t, g, fitCanvas(subGray(rotate(docTabBitmap(0).gray(), 1), image.Rect(0, 0, 800, 1200)), canvas46), "fixture 9")
	// the corner block, turned clockwise with the image, sits top right
	if g.Pix[(9+40)*g.Stride+6+760] != 0 || g.Pix[(9+40)*g.Stride+6+40] != 255 {
		t.Error("fixture 9: label isn't oriented as drawn")
	}
}
