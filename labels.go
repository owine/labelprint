package main

import (
	"context"
	"errors"
	"image"
	"slices"
)

// pagePlan is how one page becomes labels.
type pagePlan struct {
	Page     int
	Auto     bool   // boxes came from detection
	Fallback bool   // detection found nothing label-sized
	Boxes    []box  // in print order
	Tabs     []*box // auto only, parallel to Boxes; nil when no box has a doc tab
	Skip     bool   // auto fallback page while another selected page has labels: left out unless the user includes it
	Failed   bool   // auto: detection couldn't run (its render failed or was unreadable), so this is the fallback
}

// labelOpts is how boxes are chosen and what they're drawn onto.
type labelOpts struct {
	layout string        // "auto" or "page"
	crops  map[int][]box // the user's boxes per page; they win over layout
	media  labelSize     // for detection
	known  bool          // media is a real size
	canvas canvas
}

// labelImg is one finished label.
type labelImg struct {
	PNG    []byte
	Method string  // "native" or "render"
	PPI    float64 // native: the bitmap's resolution
	Scale  float64 // against true size: 1, or below 1 when a render was shrunk to fit
}

// plan picks every page's boxes: the user's crops, the whole page, or
// detection. A page detection can't analyse gets the whole page, marked
// Failed (print refuses it, the preview shows it); only a page too large to
// render (checkRender) fails here.
func (d *pdfDoc) plan(ctx context.Context, pages []int, o labelOpts) ([]pagePlan, error) {
	var detect []int
	for _, p := range pages {
		if _, ok := o.crops[p]; !ok && o.layout != "page" {
			detect = append(detect, p)
		}
	}
	var renders map[int][]byte
	if len(detect) > 0 {
		var err error
		if renders, err = d.renderPages(ctx, detect, detectDPI, true); err != nil {
			var ue *userError
			if errors.As(err, &ue) {
				return nil, err
			}
			// A fixed reason: poppler's messages can quote the document.
			reason := "pdftoppm failed"
			if ctx.Err() != nil {
				reason = "cancelled"
			}
			d.r.log.Warn("detection render failed", "reason", reason)
		}
	}
	out := make([]pagePlan, 0, len(pages))
	for _, p := range pages {
		if c, ok := o.crops[p]; ok {
			out = append(out, pagePlan{Page: p, Boxes: c})
			continue
		}
		if o.layout == "page" {
			out = append(out, pagePlan{Page: p, Boxes: []box{{0, 0, 1, 1}}})
			continue
		}
		out = append(out, d.detectPage(ctx, p, renders[p], o))
	}
	// A page with no label (instructions, say) beside pages with labels is
	// left out; when nothing in the selection has one, it prints whole. A
	// page with the user's own boxes counts as having labels.
	if slices.ContainsFunc(out, func(p pagePlan) bool { return !p.Auto || !p.Fallback }) {
		for i := range out {
			out[i].Skip = out[i].Auto && out[i].Fallback
		}
	}
	return out, nil
}

// detectPage runs detection on a page's 50 dpi render. Embedded image
// positions, which the doc-tab split needs, are fetched only if it asks.
func (d *pdfDoc) detectPage(ctx context.Context, page int, png []byte, o labelOpts) pagePlan {
	failed := pagePlan{Page: page, Auto: true, Fallback: true, Failed: true, Boxes: []box{{0, 0, 1, 1}}}
	if png == nil {
		return failed
	}
	img, err := decodePNGBytes(png)
	if err != nil {
		d.r.log.Warn("detection failed", "page", page, "reason", "unreadable render")
		return failed
	}
	m := newMask(img)
	det := &detector{m: m, dpi: detectDPI}
	if o.known {
		det.mediaW, det.mediaH = o.media.WidthMm/25.4, o.media.HeightMm/25.4
	}
	det.imageRects = func() []image.Rectangle {
		var rs []image.Rectangle
		for _, im := range d.pageImages(ctx, page) {
			rs = append(rs, im.rect.pixels(float64(m.w), float64(m.h)))
		}
		return rs
	}
	found, fallback := det.detect()
	frac := func(r image.Rectangle) box {
		w, h := float64(m.w), float64(m.h)
		return box{float64(r.Min.X) / w, float64(r.Min.Y) / h, float64(r.Dx()) / w, float64(r.Dy()) / h}
	}
	pl := pagePlan{Page: page, Auto: true, Fallback: fallback}
	hasTab := false
	for _, f := range found {
		hasTab = hasTab || f.tab != nil
	}
	for _, f := range found {
		pl.Boxes = append(pl.Boxes, frac(f.box))
		if hasTab {
			var t *box
			if f.tab != nil {
				b := frac(*f.tab)
				t = &b
			}
			pl.Tabs = append(pl.Tabs, t)
		}
	}
	return pl
}

// label makes one box's label: path A when it applies, path B otherwise.
func (d *pdfDoc) label(ctx context.Context, page int, b box, o labelOpts) (labelImg, error) {
	g, ppi, reason := d.native(ctx, page, b, o.canvas)
	if g != nil {
		png, err := encodeGray(g)
		return labelImg{PNG: png, Method: "native", PPI: ppi, Scale: 1}, err
	}
	d.r.log.Debug("passthrough not used", "page", page, "reason", reason)
	g, scale, err := d.renderLabel(ctx, page, b, o.canvas)
	if err != nil {
		return labelImg{}, err
	}
	png, err := encodeGray(g)
	return labelImg{PNG: png, Method: "render", Scale: scale}, err
}
