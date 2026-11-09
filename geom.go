package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// box is a rectangle as fractions (0–1) of a page as displayed: after
// /Rotate, within the crop box, origin top-left. The browser, every render and
// every crop share it, whatever their resolution.
type box struct{ X, Y, W, H float64 }

// MarshalJSON writes [x, y, w, h], to 4 decimals (1/10000 of an 11 in page
// is about a thousandth of an inch).
func (b box) MarshalJSON() ([]byte, error) {
	r := b.round4()
	return json.Marshal([4]float64{r.X, r.Y, r.W, r.H})
}

// round4 is b to the 4 decimals it's sent with, so the server can work out a
// box the page will build from the boxes it was sent.
func (b box) round4() box {
	r := func(v float64) float64 { return math.Round(v*1e4) / 1e4 }
	return box{r(b.X), r(b.Y), r(b.W), r(b.H)}
}

// union is the smallest box holding b and o, worked out as the page's
// unionBox does it (web/app.js), so a box sent back from it is the same box.
func (b box) union(o box) box {
	x, y := min(b.X, o.X), min(b.Y, o.Y)
	return box{x, y, max(b.X+b.W, o.X+o.W) - x, max(b.Y+b.H, o.Y+o.H) - y}
}

// pixels is b on a page rendered w×h pixels, rounded outwards.
func (b box) pixels(w, h float64) image.Rectangle {
	return image.Rect(
		int(math.Floor(b.X*w)), int(math.Floor(b.Y*h)),
		int(math.Ceil((b.X+b.W)*w)), int(math.Ceil((b.Y+b.H)*h)),
	)
}

func (b box) area() float64 { return b.W * b.H }

func (b box) intersect(o box) (box, bool) {
	x0, y0 := max(b.X, o.X), max(b.Y, o.Y)
	x1, y1 := min(b.X+b.W, o.X+o.W), min(b.Y+b.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return box{}, false
	}
	return box{x0, y0, x1 - x0, y1 - y0}, true
}

// valid reports whether b is a finite, non-empty rectangle on the page. The
// slack is for the page's 4-decimal rounding, which can put x + w at 1.0001;
// parseCrops clamps what it lets through.
func (b box) valid() bool {
	const eps = 2e-4
	for _, v := range []float64{b.X, b.Y, b.W, b.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b.X >= -eps && b.Y >= -eps && b.W > 0 && b.H > 0 && b.X+b.W <= 1+eps && b.Y+b.H <= 1+eps
}

// pageGeom is one page's boxes from pdfinfo, in PDF user space points.
type pageGeom struct {
	Media, Crop [4]float64 // x0 y0 x1 y1
	Rot         int        // 0, 90, 180 or 270
}

func (g pageGeom) turned() bool { return g.Rot == 90 || g.Rot == 270 }

func displayedSize(r [4]float64, turned bool) (w, h float64) {
	w, h = r[2]-r[0], r[3]-r[1]
	if turned {
		w, h = h, w
	}
	return w, h
}

// size is the page as displayed (the crop box, turned), in points.
func (g pageGeom) size() (w, h float64) { return displayedSize(g.Crop, g.turned()) }

// inches is size in inches.
func (g pageGeom) inches() (w, h float64) {
	w, h = g.size()
	return w / 72, h / 72
}

// mediaSize is the MediaBox as displayed, in points: what pdftohtml lays out.
func (g pageGeom) mediaSize() (w, h float64) { return displayedSize(g.Media, g.turned()) }

// htmlOffset is the displayed crop box's origin within the displayed
// MediaBox. pdftohtml positions are relative to the MediaBox, so
// crop-relative = pdftohtml position (points) − offset. No axis swap: its
// coordinates are already turned. Checked against pdftoppm -cropbox renders.
func (g pageGeom) htmlOffset() (x, y float64) {
	m, c := g.Media, g.Crop
	switch g.Rot {
	case 90:
		return c[1] - m[1], c[0] - m[0]
	case 180:
		return m[2] - c[2], c[1] - m[1]
	case 270:
		return m[3] - c[3], m[2] - c[2]
	}
	return c[0] - m[0], m[3] - c[3]
}

var pageBoxRe = regexp.MustCompile(`^Page\s+(\d+)\s+(MediaBox|CropBox|rot):\s+(.+)$`)

// parsePageGeoms reads `pdfinfo -box -f 1 -l n`.
func parsePageGeoms(out []byte, n int) ([]pageGeom, error) {
	g := make([]pageGeom, n)
	seen := make([]int, n) // 1 MediaBox, 2 CropBox
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		m := pageBoxRe.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		p, _ := strconv.Atoi(m[1])
		if p < 1 || p > n {
			continue
		}
		if m[2] == "rot" {
			if r, err := strconv.Atoi(strings.TrimSpace(m[3])); err == nil {
				g[p-1].Rot = ((r % 360) + 360) % 360
			}
			continue
		}
		f := strings.Fields(m[3])
		if len(f) != 4 {
			continue
		}
		var r [4]float64
		for i := range f {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return nil, fmt.Errorf("page %d %s: %w", p, m[2], err)
			}
			r[i] = v
		}
		if m[2] == "MediaBox" {
			g[p-1].Media, seen[p-1] = r, seen[p-1]|1
		} else {
			g[p-1].Crop, seen[p-1] = r, seen[p-1]|2
		}
	}
	for i := range g {
		if seen[i] != 3 {
			return nil, fmt.Errorf("no page boxes for page %d", i+1)
		}
		if w, h := g[i].size(); w <= 0 || h <= 0 {
			return nil, fmt.Errorf("page %d has no area", i+1)
		}
	}
	return g, nil
}

// docNoun is how messages name a page: "page 3" (and "the page") in a pdf,
// "the image" for an uploaded image, which is wrapped as a one-page pdf.
type docNoun struct{ image bool }

func (n docNoun) page(p int) string {
	if n.image {
		return "the image"
	}
	return fmt.Sprintf("page %d", p)
}

// whole is what a box can run off: "the page", or "the image".
func (n docNoun) whole() string {
	if n.image {
		return "the image"
	}
	return "the page"
}

// minBoxIn is the smallest side a crop may have: 0.5 in, less slack for
// fractions sent with 4 decimals.
const minBoxIn = 0.49

func parseLayout(v string) (string, error) {
	switch v {
	case "", "auto":
		return "auto", nil
	case "page":
		return "page", nil
	}
	return "", fieldError("layout", "label area must be auto or page")
}

// parseCrops reads the crops field: page number → boxes, for selected pages
// only (other entries are ignored, unchecked). An image's one page is 1; n
// names it in messages.
func parseCrops(v string, selected []int, inches func(page int) (w, h float64), n docNoun) (map[int][]box, error) {
	if v == "" {
		return nil, nil
	}
	var raw map[string][][]float64
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fieldError("crops", "the label boxes couldn't be read: %v", err)
	}
	sel := map[int]bool{}
	for _, p := range selected {
		sel[p] = true
	}
	out := map[int][]box{}
	for k, list := range raw {
		// Only the plain spelling: Atoi also takes "01" and "+1", which would
		// let one page carry several lists.
		p, err := strconv.Atoi(k)
		if err != nil || strconv.Itoa(p) != k {
			return nil, fieldError("crops", "%q isn't a page number", k)
		}
		if !sel[p] {
			continue
		}
		if len(list) == 0 {
			return nil, fieldError("crops", "%s has no label boxes", n.page(p))
		}
		// Counted with what the page already has, in case a key ever aliases.
		if k := len(out[p]) + len(list); k > maxBoxesPerPage {
			return nil, fieldError("crops", "%s has %d label boxes; the most is %d", n.page(p), k, maxBoxesPerPage)
		}
		pw, ph := inches(p)
		for i, a := range list {
			if len(a) != 4 {
				return nil, fieldError("crops", "box %d on %s needs 4 numbers", i+1, n.page(p))
			}
			b := box{a[0], a[1], a[2], a[3]}
			if !b.valid() {
				return nil, fieldError("crops", "box %d on %s runs off %s", i+1, n.page(p), n.whole())
			}
			if b.W*pw < minBoxIn || b.H*ph < minBoxIn {
				return nil, fieldError("crops", "box %d on %s is too small (under 0.5 in)", i+1, n.page(p))
			}
			b.X, b.Y = max(b.X, 0), max(b.Y, 0)
			b.W, b.H = min(b.W, 1-b.X), min(b.H, 1-b.Y)
			out[p] = append(out[p], b)
		}
	}
	return out, nil
}
