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
	g, scale, err := doc.renderLabel(context.Background(), 1, boxIn(8.5, 11, 1.2, 0.7, 6.1, 4.1), canvas46)
	if err != nil {
		t.Fatal(err)
	}
	// 4.1 in across onto 4 in (812 dots)
	if !near(scale, 812/4.1/203, 0.002) {
		t.Errorf("scale %g, want about %.3f", scale, 812/4.1/203)
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
	g, scale, err := doc.renderLabel(context.Background(), 1, box{0, 0, 1, 1}, canvas46)
	if err != nil {
		t.Fatal(err)
	}
	assertPureLabel(t, g, canvas46)
	// 6.25 in onto 6 in (1218 dots): 1218 / 6.25 / 203
	if !near(scale, 0.96, 0.005) {
		t.Errorf("scale %g, want about 0.96", scale)
	}
}

func TestRenderLabelNeverEnlarges(t *testing.T) {
	ctx := context.Background()
	doc := openTest(t, fixSmallLabel()) // 3 × 5 in on 4 × 6 media
	pl := doc.plan(ctx, []int{1}, opts46)[0]
	if pl.Fallback || len(pl.Boxes) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	l, err := doc.label(ctx, 1, pl.Boxes[0], opts46)
	if err != nil || l.Method != "render" {
		t.Fatalf("%v %s", err, l.Method)
	}
	if l.Scale != 1 {
		t.Errorf("scale %g: a label smaller than the media prints at true size", l.Scale)
	}
	img, err := decodePNGBytes(l.PNG)
	if err != nil {
		t.Fatal(err)
	}
	g := toGray(img)
	assertPureLabel(t, g, canvas46)
	ink, ok := inkBounds(g, g.Rect, 128)
	if !ok {
		t.Fatal("no ink")
	}
	// the border's ink is 2.93 × 4.93 in: about 594 × 1000 dots at 203 dpi,
	// where fitting the box to the canvas would make it about 700 dots wide
	if !near(float64(ink.Dx()), 2.93*203, 10) || !near(float64(ink.Dy()), 4.93*203, 10) {
		t.Errorf("ink is %dx%d dots, want about %.0fx%.0f", ink.Dx(), ink.Dy(), 2.93*203, 4.93*203)
	}
	// centred, to within the detection box's padding asymmetry
	if l, r := ink.Min.X, canvas46.W-ink.Max.X; !near(float64(l), float64(r), 6) {
		t.Errorf("not centred across: %d left, %d right", l, r)
	}
	if t0, b := ink.Min.Y, canvas46.H-ink.Max.Y; !near(float64(t0), float64(b), 6) {
		t.Errorf("not centred down: %d top, %d bottom", t0, b)
	}
}

var opts46 = func() labelOpts {
	m, _ := mediaSize("na_index-4x6_4x6in")
	return labelOpts{layout: "auto", media: m, known: true, canvas: canvas46}
}()

func wantBoxIn(t *testing.T, doc *pdfDoc, page int, got box, x, y, w, h float64) {
	t.Helper()
	pw, ph := doc.geom(page).inches()
	// ±0.15 in: boxes carry ~0.04 in of padding a side (see Refinement 5)
	const tol = 0.15
	if !near(got.X*pw, x, tol) || !near(got.Y*ph, y, tol) || !near(got.W*pw, w, tol) || !near(got.H*ph, h, tol) {
		t.Errorf("box at %.2f,%.2f %.2fx%.2f in, want %.2f,%.2f %.2fx%.2f",
			got.X*pw, got.Y*ph, got.W*pw, got.H*ph, x, y, w, h)
	}
}

func TestPlanFixtures(t *testing.T) {
	ctx := context.Background()
	type want struct{ x, y, w, h float64 }
	cases := []struct {
		name     string
		pdf      []byte
		boxes    []want
		fallback bool
	}{
		{"1 label beside text", fixLabelBesideText(), []want{{0.75, 1.25, 4, 6}}, false},
		{"3 hairline", fixHairline(), []want{{2.25, 2.5, 4, 6}}, false},
		{"4 two-up", fix2up(), []want{{1.25, 0.75, 6, 4}, {1.25, 5.75, 6, 4}}, false},
		{"6 text only", fixTextOnly(), nil, true},
		{"8 return, blank tab", fixReturn(0, 206, 196), []want{{0.5, 4.5, 5.83, 4.08}}, false},
		{"13 vector band", fixBand(), []want{{0.95, 1, 6.6, 4}}, false},
		// a 4 pt border stroked at 12 pt: ink from 10 pt (0.14 in), then 0.04 in padding
		{"existing 4x6 fixture", makeLabelPDF(1), []want{{0.1, 0.1, 3.8, 5.8}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := openTest(t, c.pdf)
			pl := doc.plan(ctx, []int{1}, opts46)[0]
			if !pl.Auto || pl.Fallback != c.fallback {
				t.Fatalf("auto %v fallback %v", pl.Auto, pl.Fallback)
			}
			if c.fallback {
				return
			}
			if len(pl.Boxes) != len(c.boxes) {
				t.Fatalf("%d boxes, want %d", len(pl.Boxes), len(c.boxes))
			}
			for i, w := range c.boxes {
				wantBoxIn(t, doc, 1, pl.Boxes[i], w.x, w.y, w.w, w.h)
			}
			if pl.Tabs != nil {
				t.Errorf("unexpected tabs %v", pl.Tabs)
			}
		})
	}
}

func TestPlanDocTabs(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name       string
		pdf        []byte
		ppiX, ppiY float64
		tabEnd     int
	}{
		{"11 retail return", fixReturn(1340, 206, 196), 206, 196, 1340},
		{"12 at 193 ppi", fixReturn(1395, 193, 193), 193, 193, 1395},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := openTest(t, c.pdf)
			pl := doc.plan(ctx, []int{1}, opts46)[0]
			if len(pl.Boxes) != 1 || len(pl.Tabs) != 1 || pl.Tabs[0] == nil {
				t.Fatalf("boxes %v tabs %v", pl.Boxes, pl.Tabs)
			}
			// the label is the image's first 1202 px; the tab's bars run from x 1240 to tabEnd, y 200–600
			wantBoxIn(t, doc, 1, pl.Boxes[0], 0.5, 4.5, 1202/c.ppiX, 800/c.ppiY)
			wantBoxIn(t, doc, 1, *pl.Tabs[0], 0.5+1240/c.ppiX, 4.5+200/c.ppiY, float64(c.tabEnd-1240)/c.ppiX, 400/c.ppiY)
			l, err := doc.label(ctx, 1, pl.Boxes[0], opts46)
			if err != nil || l.Method != "native" {
				t.Fatalf("label: %v %s", err, l.Method)
			}

			// "print separately": the tab grown to 0.5 in, as the page does, is its own native label
			pw, _ := doc.geom(1).inches()
			tab := *pl.Tabs[0]
			if tab.W*pw < 0.5 {
				tab.X -= (0.5/pw - tab.W) / 2
				tab.W = 0.5 / pw
			}
			if l, err := doc.label(ctx, 1, tab, opts46); err != nil || l.Method != "native" {
				t.Fatalf("separate tab: %v %s", err, l.Method)
			}
			// "include": label and tab together don't fit 4×6 unscaled, so they're rendered
			inc := box{X: pl.Boxes[0].X, Y: pl.Boxes[0].Y, W: tab.X + tab.W - pl.Boxes[0].X, H: pl.Boxes[0].H}
			if l, err := doc.label(ctx, 1, inc, opts46); err != nil || l.Method != "render" {
				t.Fatalf("include: %v %s", err, l.Method)
			}
		})
	}
}

func TestPlanOverrides(t *testing.T) {
	ctx := context.Background()
	doc := openTest(t, makeLabelPDF(2))
	o := opts46
	o.crops = map[int][]box{1: {{0, 0, 0.5, 0.5}}}
	pl := doc.plan(ctx, []int{1, 2}, o)
	if pl[0].Auto || len(pl[0].Boxes) != 1 || pl[0].Boxes[0] != (box{0, 0, 0.5, 0.5}) {
		t.Errorf("crops must win: %+v", pl[0])
	}
	if !pl[1].Auto {
		t.Errorf("page 2 has no crops: it's detected")
	}
	o = opts46
	o.layout = "page"
	if pl := doc.plan(ctx, []int{1}, o); pl[0].Auto || pl[0].Boxes[0] != (box{0, 0, 1, 1}) {
		t.Errorf("layout=page: %+v", pl[0])
	}
}

func TestLabelMethods(t *testing.T) {
	ctx := context.Background()
	doc := openTest(t, fixUPS2up(true, 200))
	for _, b := range doc.plan(ctx, []int{1}, opts46)[0].Boxes {
		l, err := doc.label(ctx, 1, b, opts46)
		if err != nil || l.Method != "native" {
			t.Fatalf("%v %s", err, l.Method)
		}
		if l.Scale != 1 {
			t.Errorf("native scale %g: path A is always 1:1", l.Scale)
		}
		img, _ := decodePNGBytes(l.PNG)
		g := toGray(img)
		assertPureLabel(t, g, canvas46)
		assertSourcePixels(t, g, labelBitmap(800, 1200), 6, 9)
	}
	doc = openTest(t, fixBand())
	b := doc.plan(ctx, []int{1}, opts46)[0].Boxes[0]
	if l, err := doc.label(ctx, 1, b, opts46); err != nil || l.Method != "render" {
		t.Fatalf("vector label: %v %s", err, l.Method)
	}
}

func TestPlanSkipsPagesWithoutLabels(t *testing.T) {
	ctx := context.Background()
	doc := openTest(t, fixLabelThenText())
	pl := doc.plan(ctx, []int{1, 2}, opts46)
	if pl[0].Skip || !pl[1].Fallback || !pl[1].Skip {
		t.Fatalf("p1 skip=%v; p2 fallback=%v skip=%v; want p2 skipped", pl[0].Skip, pl[1].Fallback, pl[1].Skip)
	}
	if pl := doc.plan(ctx, []int{2}, opts46); pl[0].Skip {
		t.Error("with no label anywhere in the selection, the fallback page still prints")
	}
	o := opts46
	o.layout = "page"
	for _, p := range doc.plan(ctx, []int{1, 2}, o) {
		if p.Skip {
			t.Error("layout=page never skips")
		}
	}
	o = opts46
	o.crops = map[int][]box{2: {{0, 0, 1, 1}}}
	if pl := doc.plan(ctx, []int{1, 2}, o); pl[1].Skip {
		t.Error("a page with crops is never skipped")
	}
}
