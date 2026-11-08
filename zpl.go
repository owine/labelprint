package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/Gobbo89/zebrash"
	"github.com/Gobbo89/zebrash/drawers"
)

// zplRenderer draws ZPL as PNG for the preview using zebrash, a pure-Go
// emulation of the ZPL engine (no outside service, unlike Labelary). It's a
// preview only: the printer always gets the original ZPL, so a label zebrash
// draws imperfectly still prints right.
type zplRenderer struct {
	dpi     int
	dpmm    int // dots per mm, what zebrash works in; 203 dpi ≈ 8 dpmm
	timeout time.Duration
	slots   chan struct{}
}

func newZPLRenderer(dpi int) *zplRenderer {
	return &zplRenderer{dpi: dpi, dpmm: max(1, (dpi+12)/25), timeout: 20 * time.Second, slots: make(chan struct{}, 2)}
}

// labelSize is a preview canvas in millimetres.
type labelSize struct{ WidthMm, HeightMm float64 }

// previewSize is the canvas plus how to describe it: inches straight from the
// media name, or from ^PW/^LL dots at the printer's real dpi. (Converting the
// canvas back would be off by a hair: zebrash only does whole dots per mm.)
type previewSize struct {
	labelSize
	WidthIn, HeightIn float64
}

// Bounds that keep a hostile ^PW/^LL from asking for a gigantic canvas. The
// widest Zebra desktop head is ~4.1 in; LPrint's longest roll entry is ~39 in.
const (
	maxPreviewWidthMm  = 120.0
	maxPreviewHeightMm = 1000.0
)

var (
	zplPrintWidth  = regexp.MustCompile(`(?i)\^PW\s*(\d+)`)
	zplLabelLength = regexp.MustCompile(`(?i)\^LL\s*(\d+)`)
)

// sizeFor picks the preview canvas: the label's own ^PW (width) and ^LL
// (length) in dots when it sets them — that's what the printer will use — and
// the selected media for whatever it leaves out.
func (z *zplRenderer) sizeFor(data []byte, media labelSize) previewSize {
	size := previewSize{labelSize: media, WidthIn: media.WidthMm / 25.4, HeightIn: media.HeightMm / 25.4}
	if dots, ok := lastNumber(zplPrintWidth, data); ok {
		size.WidthMm, size.WidthIn = float64(dots)/float64(z.dpmm), float64(dots)/float64(z.dpi)
	}
	if dots, ok := lastNumber(zplLabelLength, data); ok {
		size.HeightMm, size.HeightIn = float64(dots)/float64(z.dpmm), float64(dots)/float64(z.dpi)
	}
	if size.WidthMm > maxPreviewWidthMm || size.WidthMm < 10 {
		size.WidthMm = clamp(size.WidthMm, 10, maxPreviewWidthMm)
		size.WidthIn = size.WidthMm / 25.4
	}
	if size.HeightMm > maxPreviewHeightMm || size.HeightMm < 5 {
		size.HeightMm = clamp(size.HeightMm, 5, maxPreviewHeightMm)
		size.HeightIn = size.HeightMm / 25.4
	}
	size.WidthIn, size.HeightIn = round2(size.WidthIn), round2(size.HeightIn)
	return size
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func lastNumber(re *regexp.Regexp, data []byte) (int, bool) {
	m := re.FindAllSubmatch(data, -1)
	if len(m) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[len(m)-1][1]))
	return n, err == nil && n > 0
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

// largestNumber is the biggest value a command sets anywhere in the file, so
// a file of several labels is judged by its largest.
func largestNumber(re *regexp.Regexp, data []byte) (int, bool) {
	best := 0
	for _, m := range re.FindAllSubmatch(data, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > best {
			best = n
		}
	}
	return best, best > 0
}

// fitMargin is how far ^PW/^LL may exceed the selected labels before the
// preview warns: 2%, which absorbs the rounding in "4 in = 812 or 813 dots"
// and stock that's nominally 6 in, yet is far below the 12% of a 4×6.75 doc
// tab label or the 33% of a 4×8 one.
const fitMargin = 0.02

// fitWarning says when the zpl's own ^PW/^LL are bigger than the selected
// labels, so it would be cut off at the side or print across the gap onto
// the next label. It only warns: the zpl is always sent unchanged. No
// ^PW/^LL, or no media size, means nothing to compare.
func (z *zplRenderer) fitWarning(data []byte, media labelSize) string {
	if media.WidthMm <= 0 || media.HeightMm <= 0 {
		return ""
	}
	dpi := float64(z.dpi)
	mediaW, mediaH := media.WidthMm/25.4, media.HeightMm/25.4 // inches
	pw, hasW := largestNumber(zplPrintWidth, data)
	ll, hasL := largestNumber(zplLabelLength, data)
	w, h := float64(pw)/dpi, float64(ll)/dpi
	wide := hasW && w > mediaW*(1+fitMargin)
	long := hasL && h > mediaH*(1+fitMargin)
	if !wide && !long {
		return ""
	}
	in := func(v float64) string { return strconv.FormatFloat(round2(v), 'f', -1, 64) }
	stock := in(mediaW) + " × " + in(mediaH) + " in"
	labels := stock + " labels"
	at := fmt.Sprintf(" at %d dpi", z.dpi)

	// A 300 dpi label read by a 203 dpi head: ^PW comes out about 300/203
	// times the label's width (a 4 in label's ^PW1200–1230 is 5.9–6.1 in).
	if wide && dpi < 300*0.9 && math.Abs(w*dpi/300/mediaW-1) <= 0.05 {
		size := in(w)
		if hasL {
			size += " × " + in(h)
		}
		return "this zpl looks made for a 300 dpi printer:" + at + " it's " + size + " in, too big for the " + labels +
			" selected. choose " + stock + at + " in your shipping tool"
	}

	effect := map[[2]bool]string{
		{true, false}: "it will be cut off at the edge",
		{false, true}: "it will run onto the next label",
		{true, true}:  "it will be cut off at the edge and run onto the next label",
	}[[2]bool{wide, long}]
	switch {
	case hasW && hasL:
		return "this zpl is " + in(w) + " × " + in(h) + " in" + at + ", bigger than the " + labels + " selected: " + effect
	case hasW:
		return "this zpl is " + in(w) + " in wide" + at + ", wider than the " + labels + " selected: " + effect
	default:
		return "this zpl is " + in(h) + " in long" + at + ", longer than the " + labels + " selected: " + effect
	}
}

type zplPreview struct {
	Labels int            // ^XA…^XZ formats zebrash found
	Size   previewSize    // canvas used
	Pages  []renderedPage // the first `limit` labels, as PNG
}

// render parses and draws up to limit labels. zebrash has no panic recovery
// of its own, so a malformed upload is caught here and becomes an error
// instead of a crashed request; a stuck render is abandoned after the timeout.
func (z *zplRenderer) render(ctx context.Context, data []byte, media labelSize, limit int) (*zplPreview, error) {
	select {
	case z.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	type result struct {
		p   *zplPreview
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-z.slots }() // held until the render really stops
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("the zpl renderer gave up on this label (%v)", r)}
			}
		}()
		p, err := z.draw(data, media, limit)
		done <- result{p, err}
	}()

	ctx, cancel := context.WithTimeout(ctx, z.timeout)
	defer cancel()
	select {
	case r := <-done:
		return r.p, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("rendering took longer than %s", z.timeout)
	}
}

func (z *zplRenderer) draw(data []byte, media labelSize, limit int) (*zplPreview, error) {
	labels, err := zebrash.NewParser().Parse(data)
	if err != nil {
		return nil, fmt.Errorf("couldn't parse the zpl: %w", err)
	}
	p := &zplPreview{Labels: len(labels), Size: z.sizeFor(data, media)}
	opts := drawers.DrawerOptions{
		LabelWidthMm:  p.Size.WidthMm,
		LabelHeightMm: p.Size.HeightMm,
		Dpmm:          z.dpmm,
		// ^POI labels print rotated 180° so they read correctly coming out of
		// the printer; drawn upright here so the preview is readable.
		EnableInvertedLabels: false,
		// 1-bit like the thermal head, not anti-aliased grayscale.
		GrayscaleOutput: false,
	}
	drawer := zebrash.NewDrawer()
	for i, label := range labels[:min(len(labels), limit)] {
		var buf bytes.Buffer
		if err := drawer.DrawLabelAsPng(label, &buf, opts); err != nil {
			return nil, fmt.Errorf("drawing label %d: %w", i+1, err)
		}
		p.Pages = append(p.Pages, renderedPage{Page: i + 1, PNG: buf.Bytes()})
	}
	return p, nil
}
