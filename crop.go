package main

import (
	"image"
	"math"
)

const (
	detectDPI       = 50
	maxBoxesPerPage = 8
	inkBelow        = 240 // detection counts anything visibly non-white as ink
)

// mask is a page's detection render as ink / no ink.
type mask struct {
	w, h int
	ink  []bool
}

func newMask(img image.Image) *mask {
	g := toGray(img)
	m := &mask{w: g.Rect.Dx(), h: g.Rect.Dy(), ink: make([]bool, g.Rect.Dx()*g.Rect.Dy())}
	for y := range m.h {
		for x := range m.w {
			m.ink[y*m.w+x] = g.Pix[y*g.Stride+x] < inkBelow
		}
	}
	return m
}

// found is one label box in mask pixels, padded, and its doc tab, if any.
type found struct {
	box image.Rectangle
	tab *image.Rectangle
}

// detector finds label boxes on one page (the design doc's Detection section).
type detector struct {
	m              *mask
	dpi            float64
	mediaW, mediaH float64                  // inches; zero when the media size is unknown
	imageRects     func() []image.Rectangle // embedded images in mask pixels, fetched on first need
	imgs           []image.Rectangle
	fetched        bool
	out            []found
}

type span struct{ a, b int } // [a, b) along one axis

// detect returns the page's label boxes. fallback is true when nothing
// label-sized was found and the one box is the page trimmed to its ink.
func (d *detector) detect() (boxes []found, fallback bool) {
	full := image.Rect(0, 0, d.m.w, d.m.h)
	if d.mediaW > 0 && d.mediaH > 0 {
		d.cut(full)
	}
	if n := len(d.out); n > 0 && n <= maxBoxesPerPage {
		return d.out, false
	}
	t, ok := d.trim(full)
	if !ok {
		return []found{{box: full}}, true
	}
	return []found{{box: d.pad(t, d.padPx())}}, true
}

func (d *detector) inches(px int) float64 { return float64(px) / d.dpi }
func (d *detector) padPx() int            { return int(math.Ceil(0.03 * d.dpi)) }

func (d *detector) pad(r image.Rectangle, px int) image.Rectangle {
	return r.Inset(-px).Intersect(image.Rect(0, 0, d.m.w, d.m.h))
}

func (d *detector) at(x, y int) bool { return d.m.ink[y*d.m.w+x] }

// trim is the bounding box of the ink inside r.
func (d *detector) trim(r image.Rectangle) (image.Rectangle, bool) {
	x0, y0, x1, y1 := r.Max.X, r.Max.Y, r.Min.X, r.Min.Y
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if d.at(x, y) {
				x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x+1), max(y1, y+1)
			}
		}
	}
	return image.Rect(x0, y0, x1, y1), x1 > x0
}

// lineHasInk looks along one column (vertical) or row of r.
func (d *detector) lineHasInk(r image.Rectangle, i int, vertical bool) bool {
	if vertical {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			if d.at(i, y) {
				return true
			}
		}
		return false
	}
	for x := r.Min.X; x < r.Max.X; x++ {
		if d.at(x, i) {
			return true
		}
	}
	return false
}

// gutters are the empty bands at least minPx wide running fully across r:
// vertical bands (that split r into columns) or horizontal ones. r is
// trimmed, so every band found lies between ink.
func (d *detector) gutters(r image.Rectangle, vertical bool, minPx int) (out []span, widest int) {
	lo, hi := r.Min.Y, r.Max.Y
	if vertical {
		lo, hi = r.Min.X, r.Max.X
	}
	start := -1
	for i := lo; i < hi; i++ {
		empty := !d.lineHasInk(r, i, vertical)
		switch {
		case empty && start < 0:
			start = i
		case !empty && start >= 0:
			if i-start >= minPx {
				out = append(out, span{start, i})
				widest = max(widest, i-start)
			}
			start = -1
		}
	}
	return out, widest
}

// along is the part of r between s's ends on the given axis.
func along(r image.Rectangle, s span, vertical bool) image.Rectangle {
	if vertical {
		return image.Rect(s.a, r.Min.Y, s.b, r.Max.Y)
	}
	return image.Rect(r.Min.X, s.a, r.Max.X, s.b)
}

func split(r image.Rectangle, gaps []span, vertical bool) []image.Rectangle {
	lo := r.Min.Y
	if vertical {
		lo = r.Min.X
	}
	var out []image.Rectangle
	for _, g := range gaps {
		out = append(out, along(r, span{lo, g.a}, vertical))
		lo = g.b
	}
	hi := r.Max.Y
	if vertical {
		hi = r.Max.X
	}
	return append(out, along(r, span{lo, hi}, vertical))
}

// cut is the recursive XY-cut: accept a block as soon as it's a label (with
// or without a doc tab), else split it at its gutters.
func (d *detector) cut(r image.Rectangle) {
	t, ok := d.trim(r)
	if !ok {
		return
	}
	if f, ok := d.tabSplit(t); ok {
		d.out = append(d.out, f)
		return
	}
	if d.labelSized(t) {
		d.out = append(d.out, found{box: d.pad(t, d.padPx())})
		return
	}
	if minPx := int(0.5 * d.dpi); t.Dx() < minPx || t.Dy() < minPx {
		return
	}
	gap := int(math.Round(0.2 * d.dpi))
	cols, wc := d.gutters(t, true, gap)
	rows, wr := d.gutters(t, false, gap)
	var pieces []image.Rectangle
	switch {
	case len(cols) > 0 && (wc >= wr || len(rows) == 0):
		pieces = split(t, cols, true)
	case len(rows) > 0:
		pieces = split(t, rows, false)
	default:
		// No gutters: a frame round the block may be what's crossing them.
		if u := d.unframe(t); u != t {
			d.cut(u)
		}
		return
	}
	for _, p := range pieces {
		d.cut(p)
	}
}

// labelSized: 0.6–1.2× the media on each axis, in either orientation.
func (d *detector) labelSized(t image.Rectangle) bool {
	w, h := d.inches(t.Dx()), d.inches(t.Dy())
	in := func(v float64) bool { return v >= 0.6 && v <= 1.2 }
	return (in(w/d.mediaW) && in(h/d.mediaH)) || (in(w/d.mediaH) && in(h/d.mediaW))
}

// coveredByImage: at least 90% of t lies inside one embedded image.
func (d *detector) coveredByImage(t image.Rectangle) bool {
	if !d.fetched {
		d.fetched = true
		if d.imageRects != nil {
			d.imgs = d.imageRects()
		}
	}
	area := t.Dx() * t.Dy()
	for _, r := range d.imgs {
		i := t.Intersect(r)
		if 10*i.Dx()*i.Dy() >= 9*area {
			return true
		}
	}
	return false
}

// tabSplit splits a bitmap label from its UPS-style doc tab (the design doc's
// "Doc tab split"): an oversize block whose main part, before or after a
// gutter of at least 0.1 in, is the media's length.
func (d *detector) tabSplit(t image.Rectangle) (found, bool) {
	if d.mediaW <= 0 || d.mediaH <= 0 {
		return found{}, false
	}
	vertical := t.Dx() >= t.Dy() // long side along x: split with a vertical gutter
	long, short := d.inches(t.Dx()), d.inches(t.Dy())
	if !vertical {
		long, short = short, long
	}
	mLong, mShort := max(d.mediaW, d.mediaH), min(d.mediaW, d.mediaH)
	if s := short / mShort; s < 0.6 || s > 1.2 {
		return found{}, false
	}
	if l := long / mLong; l <= 1.05 || l > 1.3 {
		return found{}, false
	}
	if !d.coveredByImage(t) {
		return found{}, false
	}
	lo, hi := t.Min.Y, t.Max.Y
	if vertical {
		lo, hi = t.Min.X, t.Max.X
	}
	gaps, _ := d.gutters(t, vertical, int(math.Round(0.1*d.dpi)))
	best, bestErr := found{}, math.Inf(1)
	for _, g := range gaps {
		for _, mainFirst := range []bool{true, false} {
			main, tab := span{lo, g.a}, span{g.b, hi}
			if !mainFirst {
				main, tab = span{g.b, hi}, span{lo, g.a}
			}
			l := d.inches(main.b-main.a) / mLong
			if l < 0.9 || l > 1.05 || math.Abs(l-1) >= bestErr {
				continue
			}
			pad := min(d.padPx(), (g.b-g.a)/2)
			mr, _ := d.trim(along(t, main, vertical))
			tr, _ := d.trim(along(t, tab, vertical))
			// A doc tab is about an inch: a longer "tab" is the label's own
			// end, cut off at one of its internal white bands.
			tl := tr.Dy()
			if vertical {
				tl = tr.Dx()
			}
			if d.inches(tl) > 0.2*mLong {
				continue
			}
			tr = d.pad(tr, pad)
			best, bestErr = found{box: d.pad(mr, pad), tab: &tr}, math.Abs(l-1)
		}
	}
	return best, !math.IsInf(bestErr, 1)
}

// unframe strips a frame drawn round a block: when both opposite edges
// (top and bottom, or left and right) are lines at most 0.06 in thick whose
// ink spans at least 90% of the block, it drops them. A frame (an HTML
// page's border round an embedded label image, say) otherwise crosses every
// gutter, so the block can't be split. The result is trimmed; it's t itself
// when there's no frame.
func (d *detector) unframe(t image.Rectangle) image.Rectangle {
	maxPx := int(math.Ceil(0.06 * d.dpi))
	full := func(r image.Rectangle, i int, vertical bool) bool {
		n, total := 0, r.Dx()
		if vertical {
			total = r.Dy()
			for y := r.Min.Y; y < r.Max.Y; y++ {
				if d.at(i, y) {
					n++
				}
			}
		} else {
			for x := r.Min.X; x < r.Max.X; x++ {
				if d.at(x, i) {
					n++
				}
			}
		}
		return total > 0 && 10*n >= 9*total
	}
	// line is the thickness of the edge line starting at i and stepping by
	// step, or 0 when there's none or it's thicker than a line.
	line := func(r image.Rectangle, i, step int, vertical bool) int {
		for n := 0; n <= maxPx; n++ {
			if !full(r, i+n*step, vertical) {
				return n
			}
		}
		return 0
	}
	r := t
	if a, b := line(r, r.Min.Y, 1, false), line(r, r.Max.Y-1, -1, false); a > 0 && b > 0 && a+b < r.Dy() {
		r.Min.Y, r.Max.Y = r.Min.Y+a, r.Max.Y-b
	}
	if a, b := line(r, r.Min.X, 1, true), line(r, r.Max.X-1, -1, true); a > 0 && b > 0 && a+b < r.Dx() {
		r.Min.X, r.Max.X = r.Min.X+a, r.Max.X-b
	}
	if r == t {
		return t
	}
	if u, ok := d.trim(r); ok {
		return u
	}
	return t
}
