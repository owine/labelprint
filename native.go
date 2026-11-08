package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	maxPageImages   = 16   // pdfimages gate: rows on one page
	maxImageSide    = 4000 // pdfimages gate: pixels, either side
	nativeTolerance = 0.10 // effective resolution vs the printer's, per axis
	nativeMarginPx  = 4    // white kept around the crop's ink
	verifyMaxDiff   = 0.03 // share of the union of ink that may differ
	verifyTile      = 32   // px: misses are also counted per tile ...
	verifyTileMax   = 16   // ... and no tile may have more
	verifyStrayPx   = 2    // ink this far outside the crop's place still counts as the crop's
)

// pageImage is an embedded image as pdftohtml reports it.
type pageImage struct {
	rect box         // where it's drawn: fractions of the displayed crop box; may run past 0–1 (clipped)
	path string      // the decoded bitmap file
	size image.Point // its pixels, from the file's header (checked against the gate)
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

// extractImages runs the gate, then pdftohtml. Its errors are fixed
// categories: poppler's own messages can quote the document.
func (d *pdfDoc) extractImages(ctx context.Context, page int) ([]pageImage, error) {
	n := strconv.Itoa(page)
	out, err := d.r.run(ctx, "pdfimages", "-list", "-f", n, "-l", n, d.path)
	if err != nil {
		return nil, errors.New("pdfimages failed")
	}
	sizes, err := imageListSizes(out)
	if err != nil || len(sizes) == 0 {
		return nil, err
	}
	dir := d.scratch("html")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, errors.New("no scratch space")
	}
	if _, err := d.r.run(ctx, "pdftohtml", "-xml", "-q", "-nodrm", "-noroundcoord", "-zoom", "3",
		"-f", n, "-l", n, d.path, filepath.Join(dir, "p")); err != nil {
		return nil, errors.New("pdftohtml failed")
	}
	xml, err := os.ReadFile(filepath.Join(dir, "p.xml"))
	if err != nil {
		return nil, errors.New("pdftohtml wrote no xml")
	}
	imgs, err := parseHTMLImages(xml, d.geom(page), dir)
	if err != nil {
		return nil, err
	}
	// pdftohtml copies DCT streams out unchanged, so a JPEG's own header can
	// claim far more pixels than the dictionary the gate saw, and decoding it
	// would exhaust memory. Keep only files whose header matches a gate row.
	kept := imgs[:0]
	for _, im := range imgs {
		sz, err := imageFileSize(im.path)
		if err != nil || !slices.Contains(sizes, sz) {
			d.r.log.Debug("page image skipped", "page", page, "reason", "its size isn't one the gate passed")
			continue
		}
		im.size = sz
		kept = append(kept, im)
	}
	return kept, nil
}

// imageListSizes reads `pdfimages -list` and applies the gate: more than
// maxPageImages rows (any type, smasks too), any side over maxImageSide, or
// anything it can't read refuses, before anything is decompressed onto the
// tmpfs. It returns each row's width × height (image space, as the
// dictionary says).
func imageListSizes(out []byte) ([]image.Point, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "page") ||
		!strings.HasPrefix(strings.TrimSpace(lines[1]), "---") {
		return nil, errors.New("unreadable image list")
	}
	var sizes []image.Point
	for _, l := range lines[2:] {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if len(f) < 5 {
			return nil, errors.New("unreadable image list")
		}
		_, errP := strconv.Atoi(f[0])
		w, errW := strconv.Atoi(f[3])
		h, errH := strconv.Atoi(f[4])
		if errP != nil || errW != nil || errH != nil || w < 1 || h < 1 {
			return nil, errors.New("unreadable image list")
		}
		if w > maxImageSide || h > maxImageSide {
			return nil, fmt.Errorf("an image is %d×%d", w, h)
		}
		sizes = append(sizes, image.Pt(w, h))
	}
	if len(sizes) > maxPageImages {
		return nil, fmt.Errorf("%d images", len(sizes))
	}
	return sizes, nil
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
	best, accepted := math.Inf(1), math.Inf(1) // lowest diff seen, and accepted
	reason = "no embedded image covers the box"
	for _, im := range d.pageImages(ctx, page) {
		if ctx.Err() != nil {
			return nil, 0, "cancelled"
		}
		inter, ok := b.intersect(im.rect)
		if !ok || inter.area() < 0.9*b.area() {
			continue
		}
		// Cheap checks first, on the header's size alone: which quarter turns
		// put the image at the printer's resolution on both axes.
		rw, rh := im.rect.W*pwIn, im.rect.H*phIn // inches the image spans
		var turnsOK []int
		for turns := range 4 {
			ow, oh := float64(im.size.X), float64(im.size.Y)
			if turns%2 == 1 {
				ow, oh = oh, ow
			}
			if nearDPI(ow/rw, dpi) && nearDPI(oh/rh, dpi) {
				turnsOK = append(turnsOK, turns)
			}
		}
		if len(turnsOK) == 0 {
			reason = "the image isn't at the printer's resolution"
			continue
		}
		src, err := decodeImageFile(im.path) // its size was checked against the gate
		if err != nil {
			reason = "can't decode the image"
			continue
		}
		if !isBilevel(src) {
			reason = "the image isn't black and white"
			continue
		}
		bits := threshold(toGray(src), 128)
		var refs [2]*image.Gray // the box rendered at the image's resolution, per turn parity
		for _, flip := range []bool{false, true} {
			base := bits
			if flip {
				base = mirror(bits)
			}
			for _, turns := range turnsOK {
				if ctx.Err() != nil {
					return nil, 0, "cancelled"
				}
				o := rotate(base, turns)
				ow, oh := float64(o.Rect.Dx()), float64(o.Rect.Dy())
				ppiX, ppiY := ow/rw, oh/rh
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
				// Render the box at the image's own resolution on each axis, so
				// the crop compares 1:1, unresampled.
				ref := refs[turns%2]
				if ref == nil {
					if ref, err = d.renderBoxAt(ctx, page, b, ppiX, ppiY, true); err != nil {
						return nil, 0, "the check render failed"
					}
					refs[turns%2] = ref
				}
				// The crop's top-left in ref. Poppler starts the image at the
				// pixel holding its edge and may stretch it by one pixel (below),
				// so this is right to within a pixel, which the ±1 px test allows.
				pageW, pageH := pwIn*ppiX, phIn*ppiY
				br := b.pixels(pageW, pageH)
				at := image.Pt(
					int(math.Round(im.rect.X*pageW))+cr.Min.X-br.Min.X,
					int(math.Round(im.rect.Y*pageH))+cr.Min.Y-br.Min.Y,
				)
				// Poppler widens an image whose far edge lands on a pixel
				// boundary by one device pixel (Splash rounds an image's upper
				// bound to floor+1) and resamples it. Inside the page the ±1 px
				// test absorbs that; at the page's right or bottom edge the
				// extra pixel falls off the raster, so the render never shows
				// the bitmap's last column or row there. Leave it out.
				cmp := ref.Rect
				if b.X+b.W >= 1-1e-9 {
					cmp.Max.X--
				}
				if b.Y+b.H >= 1-1e-9 {
					cmp.Max.Y--
				}
				m := compareRender(ref.SubImage(cmp).(*image.Gray), crop, at)
				d.r.log.Debug("passthrough check", "page", page, "turns", turns, "mirror", flip,
					"differs", m.diff, "tile", m.tile, "stray", m.stray)
				if m.diff < best {
					best, reason = m.diff, m.refusal()
				}
				if m.refusal() == "" && m.diff < accepted {
					accepted, g, ppi = m.diff, crop, (ppiX+ppiY)/2
				}
			}
		}
	}
	if g == nil {
		return nil, 0, reason
	}
	return fitCanvas(g, cv), ppi, ""
}

// renderMatch is how a crop, drawn where it sits, compares with the box's
// render, both thresholded. A miss is an ink pixel (in either) with no ink
// within ±1 px in the other: poppler spreads bitmap edges by about half a
// pixel, so exact-pixel and gray comparisons fail real matches, while a stamp,
// extra text or a turned or mirrored placement still misses.
type renderMatch struct {
	diff  float64 // misses ÷ the union of ink; +Inf when neither has ink
	tile  int     // the most misses in one verifyTile × verifyTile tile
	stray bool    // the render has ink outside the crop's place (grown by verifyStrayPx)
}

// refusal is why m fails verification, or "". A global share alone would let
// a small difference through: a line of text beside the bitmap, or a white
// box drawn over part of it (hidden content that would print). The stray and
// tile checks catch those.
func (m renderMatch) refusal() string {
	switch {
	case m.stray:
		return "the box has ink beside the image"
	case !(m.diff < verifyMaxDiff):
		return fmt.Sprintf("doesn't match the render (%.1f%% differs)", m.diff*100)
	case m.tile > verifyTileMax:
		return fmt.Sprintf("doesn't match the render in one spot (%d px)", m.tile)
	}
	return ""
}

// compareRender draws crop 1:1 into a white copy of ref's frame with its
// top-left at at, and compares the two. ref's origin must be 0,0.
func compareRender(ref, crop *image.Gray, at image.Point) renderMatch {
	w, h := ref.Rect.Dx(), ref.Rect.Dy()
	place := crop.Rect.Add(at)
	cand := newWhite(w, h)
	dst := place.Intersect(cand.Rect)
	for y := dst.Min.Y; y < dst.Max.Y; y++ {
		i := (y-at.Y)*crop.Stride + dst.Min.X - at.X
		copy(cand.Pix[y*cand.Stride+dst.Min.X:], crop.Pix[i:i+dst.Dx()])
	}
	a, c := threshold(ref, 128), threshold(cand, 128)
	own := place.Inset(-verifyStrayPx)
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
	tw := (w + verifyTile - 1) / verifyTile
	tiles := make([]int, tw*((h+verifyTile-1)/verifyTile))
	var m renderMatch
	miss, union := 0, 0
	for y := range h {
		for x := range w {
			ia, ic := a.Pix[y*a.Stride+x] == 0, c.Pix[y*c.Stride+x] == 0
			if !ia && !ic {
				continue
			}
			if ia && !image.Pt(x, y).In(own) {
				m.stray = true
			}
			union++
			if (ia && !inkNear(c, x, y)) || (ic && !inkNear(a, x, y)) {
				miss++
				t := &tiles[(y/verifyTile)*tw+x/verifyTile]
				*t++
				m.tile = max(m.tile, *t)
			}
		}
	}
	if union == 0 {
		m.diff = math.Inf(1)
		return m
	}
	m.diff = float64(miss) / float64(union)
	return m
}
