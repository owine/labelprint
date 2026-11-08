package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxPageImages   = 16   // pdfimages gate: rows on one page
	maxImageSide    = 4000 // pdfimages gate: pixels, either side
	nativeTolerance = 0.10 // effective resolution vs the printer's, per axis
	nativeMarginPx  = 4    // white kept around the crop's ink
	verifyDPI       = 100
	verifyMaxDiff   = 0.03
)

// pageImage is an embedded image as pdftohtml reports it.
type pageImage struct {
	rect box    // where it's drawn: fractions of the displayed crop box; may run past 0–1 (clipped)
	path string // the decoded bitmap file
}

// pageImages returns a page's embedded images, extracted on first need and
// cached for both detection (doc tabs) and path A. nil means there are none,
// or they're unavailable (the gate refused, or a tool failed); callers treat
// those alike, and neither is an error.
func (d *pdfDoc) pageImages(ctx context.Context, page int) []pageImage {
	if imgs, ok := d.images[page]; ok {
		return imgs
	}
	imgs, err := d.extractImages(ctx, page)
	if err != nil {
		d.r.log.Debug("page images unavailable", "page", page, "reason", err)
	}
	d.images[page] = imgs
	return imgs
}

func (d *pdfDoc) extractImages(ctx context.Context, page int) ([]pageImage, error) {
	n := strconv.Itoa(page)
	out, err := d.r.run(ctx, "pdfimages", "-list", "-f", n, "-l", n, d.path)
	if err != nil {
		return nil, err
	}
	if rows, err := countImageRows(out); err != nil || rows == 0 {
		return nil, err
	}
	dir := d.scratch("html")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := d.r.run(ctx, "pdftohtml", "-xml", "-q", "-nodrm", "-noroundcoord", "-zoom", "3",
		"-f", n, "-l", n, d.path, filepath.Join(dir, "p")); err != nil {
		return nil, err
	}
	xml, err := os.ReadFile(filepath.Join(dir, "p.xml"))
	if err != nil {
		return nil, err
	}
	return parseHTMLImages(xml, d.geom(page), dir)
}

// countImageRows reads `pdfimages -list` and applies the gate: more than
// maxPageImages rows (any type, smasks too) or any side over maxImageSide
// refuses, before anything is decompressed onto the tmpfs.
func countImageRows(out []byte) (int, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	rows := 0
	for _, l := range lines[min(2, len(lines)):] { // two header lines
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		w, errW := strconv.Atoi(f[3])
		h, errH := strconv.Atoi(f[4])
		if errW != nil || errH != nil {
			continue
		}
		if w > maxImageSide || h > maxImageSide {
			return 0, fmt.Errorf("an image is %d×%d", w, h)
		}
		rows++
	}
	if rows > maxPageImages {
		return 0, fmt.Errorf("%d images", rows)
	}
	return rows, nil
}

var (
	htmlTagRe  = regexp.MustCompile(`<(page|image)\s[^>]*>`)
	htmlAttrRe = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// parseHTMLImages reads the <image> elements of pdftohtml's XML with a regex:
// the XML also carries the document's text, which needn't be well-formed.
func parseHTMLImages(xml []byte, g pageGeom, dir string) ([]pageImage, error) {
	mw, _ := g.mediaSize()
	cw, ch := g.size()
	offX, offY := g.htmlOffset()
	zoom := 0.0 // pdftohtml clamps -zoom to 3; read the real scale off <page>
	var out []pageImage
	for _, m := range htmlTagRe.FindAllSubmatch(xml, -1) {
		attrs := map[string]string{}
		for _, a := range htmlAttrRe.FindAllSubmatch(m[0], -1) {
			attrs[string(a[1])] = string(a[2])
		}
		num := func(k string) (float64, bool) {
			v, err := strconv.ParseFloat(attrs[k], 64)
			return v, err == nil
		}
		if string(m[1]) == "page" {
			if w, ok := num("width"); ok && mw > 0 {
				zoom = w / mw
			}
			continue
		}
		if zoom <= 0 {
			return nil, errors.New("pdftohtml: an image before the page size")
		}
		top, ok1 := num("top")
		left, ok2 := num("left")
		w, ok3 := num("width")
		h, ok4 := num("height")
		if !(ok1 && ok2 && ok3 && ok4) || attrs["src"] == "" {
			continue
		}
		if w < 0 { // a turned or flipped placement
			left, w = left+w, -w
		}
		if h < 0 {
			top, h = top+h, -h
		}
		if w == 0 || h == 0 {
			continue
		}
		out = append(out, pageImage{
			rect: box{X: (left/zoom - offX) / cw, Y: (top/zoom - offY) / ch, W: w / zoom / cw, H: h / zoom / ch},
			path: filepath.Join(dir, filepath.Base(attrs["src"])),
		})
	}
	return out, nil
}

func nearDPI(v, dpi float64) bool { return math.Abs(v/dpi-1) <= nativeTolerance }

// native is path A: when the box is the visible part of one embedded
// black/white bitmap at about the printer's resolution, the label is that
// bitmap's own pixels, unscaled, after checking them against a render of the
// box. g is nil (with a reason for the debug log) whenever it doesn't apply.
func (d *pdfDoc) native(ctx context.Context, page int, b box, cv canvas) (g *image.Gray, ppi float64, reason string) {
	if !cv.known() {
		return nil, 0, "no media size"
	}
	pwIn, phIn := d.geom(page).inches()
	dpi := float64(d.r.dpi)
	var ref *image.Gray // the box rendered at verifyDPI, made on first need
	best := math.Inf(1)
	reason = "no embedded image covers the box"
	for _, im := range d.pageImages(ctx, page) {
		inter, ok := b.intersect(im.rect)
		if !ok || inter.area() < 0.9*b.area() {
			continue
		}
		src, err := decodeImageFile(im.path)
		if err != nil {
			reason = "can't decode the image"
			continue
		}
		if !isBilevel(src) {
			reason = "the image isn't black and white"
			continue
		}
		bits := threshold(toGray(src), 128)
		rw, rh := im.rect.W*pwIn, im.rect.H*phIn // inches the image spans
		for turns := range 4 {
			for _, flip := range []bool{false, true} {
				o := bits
				if flip {
					o = mirror(o)
				}
				o = rotate(o, turns)
				ow, oh := float64(o.Rect.Dx()), float64(o.Rect.Dy())
				ppiX, ppiY := ow/rw, oh/rh
				if !nearDPI(ppiX, dpi) || !nearDPI(ppiY, dpi) {
					reason = "the image isn't at the printer's resolution"
					continue
				}
				// The box's part of the image in its pixels, trimmed to ink.
				cr := image.Rect(
					int(math.Floor((inter.X-im.rect.X)/im.rect.W*ow)), int(math.Floor((inter.Y-im.rect.Y)/im.rect.H*oh)),
					int(math.Ceil((inter.X+inter.W-im.rect.X)/im.rect.W*ow)), int(math.Ceil((inter.Y+inter.H-im.rect.Y)/im.rect.H*oh)),
				).Intersect(o.Rect)
				ink, ok := inkBounds(o, cr, 128)
				if !ok {
					continue
				}
				cr = ink.Inset(-nativeMarginPx).Intersect(cr)
				fw, fh := cr.Dx(), cr.Dy()
				if fw != fh && (fw > fh) != cv.landscape() {
					fw, fh = fh, fw
				}
				if fw > cv.W || fh > cv.H {
					reason = "the image is bigger than the label"
					continue
				}
				crop := subGray(o, cr)
				if ref == nil {
					if ref, err = d.renderBoxAt(ctx, page, b, verifyDPI, true); err != nil {
						return nil, 0, "the check render failed"
					}
				}
				place := box{
					X: im.rect.X + float64(cr.Min.X)/ow*im.rect.W, Y: im.rect.Y + float64(cr.Min.Y)/oh*im.rect.H,
					W: float64(cr.Dx()) / ow * im.rect.W, H: float64(cr.Dy()) / oh * im.rect.H,
				}
				score := compareRender(ref, crop, b, place, pwIn*verifyDPI, phIn*verifyDPI)
				d.r.log.Debug("passthrough check", "page", page, "turns", turns, "mirror", flip, "differs", score)
				if score < best {
					best = score
					if score < verifyMaxDiff {
						g, ppi = crop, (ppiX+ppiY)/2
					}
				}
			}
		}
	}
	if g == nil {
		if !math.IsInf(best, 1) {
			reason = fmt.Sprintf("doesn't match the render (%.1f%% differs)", best*100)
		}
		return nil, 0, reason
	}
	return fitCanvas(g, cv), ppi, ""
}

// compareRender scores the crop, drawn where it sits in the box, against the
// box's own render, both thresholded: the share of ink pixels (in either)
// with no ink within ±1 px in the other. Poppler spreads bitmap edges by about
// half a pixel, so exact-pixel and gray comparisons fail real matches; a 1 px
// neighbourhood absorbs that, while a stamp, extra text or a turned or
// mirrored placement still differs by far more than verifyMaxDiff.
func compareRender(ref, crop *image.Gray, b, place box, pageW, pageH float64) float64 {
	br := b.pixels(pageW, pageH)
	pr := place.pixels(pageW, pageH).Sub(br.Min)
	w, h := ref.Rect.Dx(), ref.Rect.Dy()
	cand := newWhite(w, h)
	if pr.Dx() > 0 && pr.Dy() > 0 {
		scaled := scaleArea(crop, pr.Dx(), pr.Dy())
		dst := pr.Intersect(cand.Rect)
		draw.Draw(cand, dst, scaled, dst.Min.Sub(pr.Min), draw.Src)
	}
	a, c := threshold(ref, 128), threshold(cand, 128)
	inkNear := func(g *image.Gray, x, y int) bool {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				if xx, yy := x+dx, y+dy; xx >= 0 && yy >= 0 && xx < w && yy < h && g.Pix[yy*g.Stride+xx] == 0 {
					return true
				}
			}
		}
		return false
	}
	miss, union := 0, 0
	for y := range h {
		for x := range w {
			ia, ic := a.Pix[y*a.Stride+x] == 0, c.Pix[y*c.Stride+x] == 0
			if !ia && !ic {
				continue
			}
			union++
			if (ia && !inkNear(c, x, y)) || (ic && !inkNear(a, x, y)) {
				miss++
			}
		}
	}
	if union == 0 {
		return math.Inf(1)
	}
	return float64(miss) / float64(union)
}
