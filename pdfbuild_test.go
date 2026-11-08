package main

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pdfBuilder writes small PDFs for tests: pages of any size, with /Rotate and
// a CropBox, drawn in inches from the top-left of the page as displayed.
type pdfBuilder struct{ pages []*pdfPage }

type pdfPage struct {
	media, crop [4]float64 // user space points
	rotate      int
	ops         strings.Builder
	images      []pdfImage
}

// pdfImage is a black/white bitmap: true = black, row-major from the top.
type pdfImage struct {
	w, h int
	pix  []bool
	kind string // "gray1" (1-bit DeviceGray, the default), "rgb", "indexed" or "dct"
	dct  []byte // kind "dct": the stream as is, whatever its own header says
}

func newBitmap(w, h int) pdfImage { return pdfImage{w: w, h: h, pix: make([]bool, w*h), kind: "gray1"} }

func (im pdfImage) set(x, y int)        { im.pix[y*im.w+x] = true }
func (im pdfImage) black(x, y int) bool { return im.pix[y*im.w+x] }

// gray is im as an 8-bit gray image: black 0, white 255.
func (im pdfImage) gray() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, im.w, im.h))
	for i, b := range im.pix {
		if !b {
			g.Pix[i] = 255
		}
	}
	return g
}

func (im pdfImage) fill(x0, y0, x1, y1 int) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			im.pix[y*im.w+x] = true
		}
	}
}

func inches(w, h float64) [4]float64 { return [4]float64{0, 0, w * 72, h * 72} }

var letter = inches(8.5, 11)

func (b *pdfBuilder) page(media [4]float64, rotate int) *pdfPage {
	p := &pdfPage{media: media, crop: media, rotate: rotate}
	b.pages = append(b.pages, p)
	return p
}

// displayed is the crop box's size as displayed, in points.
func (p *pdfPage) displayed() (w, h float64) {
	w, h = p.crop[2]-p.crop[0], p.crop[3]-p.crop[1]
	if p.rotate == 90 || p.rotate == 270 {
		w, h = h, w
	}
	return w, h
}

// frame maps the displayed crop box (points, origin bottom-left) to user space.
func (p *pdfPage) frame() string {
	c := p.crop
	switch p.rotate {
	case 90:
		return fmt.Sprintf("0 1 -1 0 %g %g", c[2], c[1])
	case 180:
		return fmt.Sprintf("-1 0 0 -1 %g %g", c[2], c[3])
	case 270:
		return fmt.Sprintf("0 -1 1 0 %g %g", c[0], c[3])
	}
	return fmt.Sprintf("1 0 0 1 %g %g", c[0], c[1])
}

// pt converts a rectangle in inches from the displayed top-left to frame points.
func (p *pdfPage) pt(x, y, w, h float64) (fx, fy, fw, fh float64) {
	_, dh := p.displayed()
	return x * 72, dh - (y+h)*72, w * 72, h * 72
}

func (p *pdfPage) fillRect(x, y, w, h float64) {
	fx, fy, fw, fh := p.pt(x, y, w, h)
	fmt.Fprintf(&p.ops, "%g %g %g %g re f\n", fx, fy, fw, fh)
}

func (p *pdfPage) strokeRect(x, y, w, h, lineWidth float64) {
	fx, fy, fw, fh := p.pt(x, y, w, h)
	fmt.Fprintf(&p.ops, "%g w %g %g %g %g re S\n", lineWidth, fx, fy, fw, fh)
}

// text draws a line of unembedded Helvetica with its baseline y inches from the top.
func (p *pdfPage) text(x, y, size float64, s string) {
	_, dh := p.displayed()
	fmt.Fprintf(&p.ops, "BT /F1 %g Tf %g %g Td (%s) Tj ET\n", size, x*72, dh-y*72, s)
}

// image draws im into the displayed rectangle with its content turned clockwise
// by quarter turns. With turns 1 or 3 the image's width runs down the rectangle.
func (p *pdfPage) image(im pdfImage, x, y, w, h float64, turns int) {
	fx, fy, fw, fh := p.pt(x, y, w, h)
	var m string
	switch turns % 4 {
	case 1:
		m = fmt.Sprintf("0 %g %g 0 %g %g", -fh, fw, fx, fy+fh)
	case 2:
		m = fmt.Sprintf("%g 0 0 %g %g %g", -fw, -fh, fx+fw, fy+fh)
	case 3:
		m = fmt.Sprintf("0 %g %g 0 %g %g", fh, -fw, fx+fw, fy)
	default:
		m = fmt.Sprintf("%g 0 0 %g %g %g", fw, fh, fx, fy)
	}
	fmt.Fprintf(&p.ops, "q %s cm /Im%d Do Q\n", m, len(p.images))
	p.images = append(p.images, im)
}

// imageAs draws im into the displayed rectangle so that what's displayed is
// rotate(mirror(im) if flip, turns): any of the 8 placements a PDF can make.
func (p *pdfPage) imageAs(im pdfImage, x, y, w, h float64, flip bool, turns int) {
	fx, fy, fw, fh := p.pt(x, y, w, h)
	// (u, v): the image's unit square, origin bottom-left, as cm maps it.
	// Its top-down point (u, 1−v) is displayed, top-down, at to(u, 1−v).
	to := func(a, b float64) (float64, float64) {
		if flip {
			a = 1 - a
		}
		for range turns { // clockwise
			a, b = 1-b, a
		}
		return a, b
	}
	at := func(u, v float64) (float64, float64) {
		s, t := to(u, 1-v)
		return fx + s*fw, fy + fh - t*fh
	}
	e, f := at(0, 0)
	ax, ay := at(1, 0)
	cx, cy := at(0, 1)
	fmt.Fprintf(&p.ops, "q %g %g %g %g %g %g cm /Im%d Do Q\n", ax-e, ay-f, cx-e, cy-f, e, f, len(p.images))
	p.images = append(p.images, im)
}

// vectorLabel draws a label in vector: a 2 pt border, a solid block in its
// top-left corner (to catch mirrored or turned output), bars and a line of text.
func (p *pdfPage) vectorLabel(x, y, w, h float64) {
	p.strokeRect(x+0.05, y+0.05, w-0.1, h-0.1, 2)
	p.fillRect(x+0.15, y+0.15, 0.4, 0.4)
	for i := range 24 {
		p.fillRect(x+0.3+float64(i)*(w-0.6)/24, y+h*0.55, 0.02+0.01*float64(i%3), h*0.25)
	}
	p.text(x+0.7, y+0.45, 14, "SHIP TO LABELPRINT")
}

// paragraph draws lines of 10 pt instruction text, 13 pt apart.
func (p *pdfPage) paragraph(x, y float64, lines, chars int) {
	line := strings.Repeat("Fold here and attach ", chars/21+1)[:chars]
	for i := range lines {
		p.text(x, y+0.15+float64(i)*13/72, 10, line)
	}
}

func (im pdfImage) object() string {
	if im.kind == "dct" {
		return fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray "+
			"/BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", im.w, im.h, len(im.dct), im.dct)
	}
	var raw []byte
	cs, bpc := "/DeviceGray", 1
	switch im.kind {
	case "rgb":
		cs, bpc = "/DeviceRGB", 8
		for _, b := range im.pix {
			v := byte(255)
			if b {
				v = 0
			}
			raw = append(raw, v, v, v)
		}
	case "indexed":
		cs, bpc = "[/Indexed /DeviceRGB 1 <000000FFFFFF>]", 8
		for _, b := range im.pix {
			v := byte(1)
			if b {
				v = 0
			}
			raw = append(raw, v)
		}
	default: // 1-bit gray: a set bit is white
		row := (im.w + 7) / 8
		raw = make([]byte, row*im.h)
		for y := range im.h {
			for x := range im.w {
				if !im.black(x, y) {
					raw[y*row+x/8] |= 0x80 >> (x % 8)
				}
			}
		}
	}
	var z bytes.Buffer
	zw, err := zlib.NewWriterLevel(&z, zlib.NoCompression)
	if err != nil {
		panic(err)
	}
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s "+
		"/BitsPerComponent %d /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream",
		im.w, im.h, cs, bpc, z.Len(), z.Bytes())
}

func (b *pdfBuilder) bytes() []byte {
	var objs []string // object n is objs[n-1]
	add := func(s string) int { objs = append(objs, s); return len(objs) }
	catalog, pages := add(""), add("")
	font := add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	rect := func(r [4]float64) string { return fmt.Sprintf("[%g %g %g %g]", r[0], r[1], r[2], r[3]) }
	var kids []string
	for _, p := range b.pages {
		var xobjs []string
		for i, im := range p.images {
			xobjs = append(xobjs, fmt.Sprintf("/Im%d %d 0 R", i, add(im.object())))
		}
		content := "q " + p.frame() + " cm\n" + p.ops.String() + "Q\n"
		c := add(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		pg := add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox %s /CropBox %s /Rotate %d "+
			"/Resources << /Font << /F1 %d 0 R >> /XObject << %s >> >> /Contents %d 0 R >>",
			pages, rect(p.media), rect(p.crop), p.rotate, font, strings.Join(xobjs, " "), c))
		kids = append(kids, fmt.Sprintf("%d 0 R", pg))
	}
	objs[catalog-1] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pages)
	objs[pages-1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// ---------------------------------------------------------------- bitmaps

// labelBitmap is a w×h black/white label: a 1 px border (so its ink reaches
// every edge), a solid 60 px block in the top-left corner and bars.
func labelBitmap(w, h int) pdfImage {
	im := newBitmap(w, h)
	im.fill(0, 0, w, 1)
	im.fill(0, h-1, w, h)
	im.fill(0, 0, 1, h)
	im.fill(w-1, 0, w, h)
	im.fill(10, 10, 70, 70)
	step := (w - 120) / 30
	for i := range 30 {
		x := 60 + i*step
		im.fill(x, h/2, x+2+i%3, h/2+h/6)
	}
	return im
}

// docTabBitmap is the UPS API image: 1400×800, a 1202 px label, then a doc
// tab that's blank (tabEnd 0) or carries bars from x 1240 up to tabEnd.
func docTabBitmap(tabEnd int) pdfImage {
	im := newBitmap(1400, 800)
	lbl := labelBitmap(1202, 800)
	for y := range 800 {
		for x := range 1202 {
			if lbl.black(x, y) {
				im.set(x, y)
			}
		}
	}
	for x := 1240; x+3 <= tabEnd; x += 6 {
		im.fill(x, 200, x+3, 600)
	}
	return im
}

// ---------------------------------------------------------------- fixtures (numbered as in the spec)

// 1: Letter landscape via /Rotate 90, a vector label in the left half and
// instruction text (about 4.4 × 1.9 in, not label-sized) in the right half.
func fixLabelBesideText() []byte {
	var b pdfBuilder
	p := b.page(letter, 90)
	p.vectorLabel(0.75, 1.25, 4, 6)
	p.paragraph(6, 3, 10, 64)
	return b.bytes()
}

// 2 and 10: Letter landscape via /Rotate 270, two 800×1200 bitmap labels
// placed at ppi; crop adds a CropBox inset 0.25 in.
func fixUPS2up(crop bool, ppi float64) []byte {
	var b pdfBuilder
	p := b.page(letter, 270)
	if crop {
		p.crop = [4]float64{18, 18, 594, 774}
	}
	w, h := 800/ppi, 1200/ppi
	p.image(labelBitmap(800, 1200), 0.75, 1.25, w, h, 0)
	p.image(labelBitmap(800, 1200), 6.25, 1.25, w, h, 0)
	return b.bytes()
}

// 3: Letter portrait, a label outlined by a 0.25 pt hairline, centred.
func fixHairline() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.strokeRect(2.25, 2.5, 4, 6, 0.25)
	p.text(2.6, 3.2, 12, "USPS GROUND ADVANTAGE")
	for i := range 20 {
		p.fillRect(2.7+float64(i)*0.15, 6, 0.05, 1)
	}
	return b.bytes()
}

// 4: Letter portrait, two 6 × 4 in landscape vector labels, top and bottom.
func fix2up() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.vectorLabel(1.25, 0.75, 6, 4)
	p.vectorLabel(1.25, 5.75, 6, 4)
	return b.bytes()
}

// 5: a 6.25 × 4 in landscape page holding one vector label.
func fixWide() []byte {
	var b pdfBuilder
	p := b.page(inches(6.25, 4), 0)
	p.vectorLabel(0.05, 0.05, 6.15, 3.9)
	return b.bytes()
}

// 6: Letter, instruction text only.
func fixTextOnly() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	for _, y := range []float64{1, 4.5, 8} { // spread out, so the block as a whole isn't label-sized
		p.paragraph(1, y, 5, 70)
	}
	return b.bytes()
}

// 7: an 800×1200 bitmap label with a vector stamp drawn over a white area of it.
func fixStamped() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.image(labelBitmap(800, 1200), 2.25, 2.5, 4, 6, 0)
	p.fillRect(3, 3.5, 1.5, 0.5)
	return b.bytes()
}

// 8, 11 and 12: a UPS return on Letter portrait: a heading and instructions, a
// dashed cut line (an image strip across the page), then the 1400×800 RGB
// label image placed at ppiX × ppiY. tabEnd 0 leaves the doc tab blank.
func fixReturn(tabEnd int, ppiX, ppiY float64) []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.text(0.5, 0.75, 16, "UPS Electronic Return Label")
	p.paragraph(0.5, 1.2, 6, 80)
	dash := newBitmap(600, 4)
	for x := 0; x < 600; x += 10 {
		dash.fill(x, 0, x+5, 4)
	}
	p.image(dash, 0, 3.6, 8.5, 0.06, 0)
	im := docTabBitmap(tabEnd)
	im.kind = "rgb"
	p.image(im, 0.5, 4.5, 1400/ppiX, 800/ppiY, 0)
	return b.bytes()
}

// 9: a 4 × 6 in page with the 1400×800 indexed-colour image turned 90° and
// running 1 in past the bottom edge, so the page clips the blank doc tab.
func fixClippedTab() []byte {
	var b pdfBuilder
	p := b.page(inches(4, 6), 0)
	im := docTabBitmap(0)
	im.kind = "indexed"
	p.image(im, 0, 0, 4, 7, 1)
	return b.bytes()
}

// 13: a 6.6 × 4 in vector label whose end strip sits behind a full-height
// white band: oversize, but no bitmap, so it must not be tab-split.
func fixBand() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.vectorLabel(0.95, 1, 6, 4)
	p.fillRect(7.15, 1.5, 0.4, 3)
	return b.bytes()
}

// renderForTest rasterises page 1 of a PDF as displayed, straight through pdftoppm.
func renderForTest(t *testing.T, pdf []byte, dpi int) image.Image {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	cmd := exec.Command("pdftoppm", "-png", "-gray", "-cropbox", "-r", strconv.Itoa(dpi), "-singlefile", in, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pdftoppm: %v: %s", err, b)
	}
	f, err := os.Open(out + ".png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func isBlackAt(img image.Image, x, y int) bool {
	r, _, _, _ := img.At(x, y).RGBA()
	return r < 0x8000
}

// The builder's coordinates must land where they're drawn whatever the
// /Rotate and CropBox, or every fixture test below is testing nothing.
func TestBuilderFrames(t *testing.T) {
	requirePoppler(t)
	cases := []struct {
		name           string
		pdf            []byte
		w, h           int // displayed page at 50 dpi
		markX, markY   int // inside the top-left corner block
		blankX, blankY int // the mirrored corner, which must stay white
	}{
		// vector label at (0.75, 1.25): block at 0.9–1.3 in across, 1.4–1.8 in down
		{"rotate 90", fixLabelBesideText(), 550, 425, 55, 80, 225, 80},
		// bitmap label at (0.75, 1.25) of a 10.5 × 8 in crop: block 0.8–1.1, 1.3–1.6 in
		{"rotate 270 + cropbox", fixUPS2up(true, 200), 525, 400, 47, 72, 225, 72},
		{"rotate 270", fixUPS2up(false, 200), 550, 425, 47, 72, 225, 72},
		// 4 × 6 in page, image turned clockwise: its top-left block lands at the displayed top-right
		{"image turned clockwise", fixClippedTab(), 200, 300, 190, 10, 10, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := renderForTest(t, c.pdf, 50)
			if b := img.Bounds(); b.Dx() != c.w || b.Dy() != c.h {
				t.Fatalf("page is %dx%d, want %dx%d", b.Dx(), b.Dy(), c.w, c.h)
			}
			if !isBlackAt(img, c.markX, c.markY) {
				t.Errorf("corner block missing at (%d,%d)", c.markX, c.markY)
			}
			if isBlackAt(img, c.blankX, c.blankY) {
				t.Errorf("(%d,%d) should be white: mirrored or misplaced", c.blankX, c.blankY)
			}
		})
	}
}

// a label page followed by an instructions page with no label on it
func fixLabelThenText() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.vectorLabel(2.25, 2.5, 4, 6)
	p = b.page(letter, 0)
	for _, y := range []float64{1, 4.5, 8} {
		p.paragraph(1, y, 5, 70)
	}
	return b.bytes()
}

// a 3 × 5 in vector label on Letter: smaller than 4 × 6 media
func fixSmallLabel() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.vectorLabel(2.75, 3, 3, 5)
	return b.bytes()
}

// A store's return on Letter portrait: instructions, then the carrier's
// 1400×800 1-bit label image at 183 ppi with a 1 pt frame stroked round it,
// as an HTML page draws one, and an instructions page after it. The image
// has its doc tab first: bars at x 20–160, a gutter, then the label at x
// 320–1390 with a 30 px white band through it at x 1100.
func fixFramedTabFirst() []byte {
	var b pdfBuilder
	p := b.page(letter, 0)
	p.text(0.5, 0.75, 16, "Return Label")
	p.paragraph(0.5, 1.2, 8, 80)
	im := newBitmap(1400, 800)
	for x := 20; x+3 <= 160; x += 6 {
		im.fill(x, 200, x+3, 600)
	}
	im.fill(320, 20, 1100, 80) // the label: a heading, a corner block, address lines and bars
	im.fill(320, 120, 420, 220)
	for y := 260; y < 460; y += 40 {
		im.fill(460, y, 1000, y+16)
	}
	for i := range 40 {
		x := 340 + i*18
		im.fill(x, 520, x+4+i%3*2, 780)
	}
	im.fill(1130, 20, 1390, 300) // past the band: the label's own end (a box and a code)
	im.fill(1130, 500, 1390, 780)
	ppi := 183.0
	x, y, w, h := 0.4, 5.5, 1400/ppi, 800/ppi
	p.image(im, x, y, w, h, 0)
	p.strokeRect(x, y, w, h, 1)
	p = b.page(letter, 0)
	for _, y := range []float64{1, 4.5, 8} {
		p.paragraph(1, y, 5, 70)
	}
	return b.bytes()
}

// fixHugePages is a 100 × 100 in page and a 200 × 200 in one (the most a PDF
// page can be), each with one short line of text: cheap to build, far too
// big to render at the printer's resolution.
func fixHugePages() []byte {
	var b pdfBuilder
	for _, side := range []float64{100, 200} {
		b.page(inches(side, side), 0).text(1, 1, 12, "huge page")
	}
	return b.bytes()
}

// fixLabelThenOneLine is a label page, then a page with one short line of
// text: its fallback box is far under the 0.5 in a crop must be.
func fixLabelThenOneLine() []byte {
	var b pdfBuilder
	b.page(letter, 0).vectorLabel(2.25, 2.5, 4, 6)
	b.page(letter, 0).text(3, 5, 10, "keep this page for your records")
	return b.bytes()
}
