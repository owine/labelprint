package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// pdfRenderer rasterises PDFs with poppler. Labels come out pure black/white
// at the printer's dots (see labels.go); anti-aliasing is off for them, since
// smoothed edges would be dithered into fuzzy barcode bars.
type pdfRenderer struct {
	dpi     int
	timeout time.Duration // per poppler invocation
	slots   chan struct{} // caps concurrent pipelines on a small Pi
	log     *slog.Logger
}

func newPDFRenderer(dpi int, log *slog.Logger) *pdfRenderer {
	return &pdfRenderer{dpi: dpi, timeout: 60 * time.Second, slots: make(chan struct{}, 2), log: log}
}

// acquire takes one of the two pipeline slots: everything one request does
// with a PDF runs inside it.
func (r *pdfRenderer) acquire(ctx context.Context) (release func(), err error) {
	select {
	case r.slots <- struct{}{}:
		return func() { <-r.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// renderedPage is one page's PNG, in page order.
type renderedPage struct {
	Page int
	PNG  []byte
}

// pdfDoc is a PDF written to scratch space (the tmpfs at /tmp in the
// container) for poppler to read. Close removes it.
type pdfDoc struct {
	r      *pdfRenderer
	dir    string
	path   string
	Pages  int
	geoms  []pageGeom          // per page, index page-1
	images map[int][]pageImage // per page, filled on first need (native.go)
	seq    int                 // names scratch files
}

func (d *pdfDoc) geom(page int) pageGeom { return d.geoms[page-1] }

func (r *pdfRenderer) open(ctx context.Context, data []byte) (*pdfDoc, error) {
	dir, err := os.MkdirTemp("", "labelprint-*")
	if err != nil {
		return nil, fmt.Errorf("scratch space: %w", err)
	}
	d := &pdfDoc{r: r, dir: dir, path: filepath.Join(dir, "in.pdf"), images: map[int][]pageImage{}}
	if err := os.WriteFile(d.path, data, 0o600); err != nil {
		d.Close()
		return nil, fmt.Errorf("scratch space: %w", err)
	}
	out, err := r.run(ctx, "pdfinfo", d.path)
	if err != nil {
		d.Close()
		return nil, badRequest("couldn't read that pdf: %s", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Pages:"); ok {
			d.Pages, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if d.Pages < 1 {
		d.Close()
		return nil, badRequest("that pdf has no pages")
	}
	out, err = r.run(ctx, "pdfinfo", "-box", "-f", "1", "-l", strconv.Itoa(d.Pages), d.path)
	if err != nil {
		d.Close()
		return nil, badRequest("couldn't read that pdf: %s", err)
	}
	if d.geoms, err = parsePageGeoms(out, d.Pages); err != nil {
		d.Close()
		return nil, badRequest("couldn't read that pdf's page sizes: %s", err)
	}
	return d, nil
}

func (d *pdfDoc) Close() { _ = os.RemoveAll(d.dir) }

// run executes a poppler tool and folds its stderr into the error.
func (r *pdfRenderer) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s took longer than %s", name, r.timeout)
		}
		if msg := lastLine(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// maxRenderPx caps any one raster poppler draws for us, checked before it's
// drawn. Letter at 203 dpi is about 3.9 Mpx and Tabloid 7.7 Mpx; a box or page
// over this would be decoded and copied in Go several times over, which a
// 512 MB container can't take. A PDF page can be 200 in on a side.
const maxRenderPx = 40_000_000

// checkRender refuses a w × h pixel render of page that's over maxRenderPx.
// Previews, detection, labels and path A's check render all come through it.
func checkRender(page int, w, h float64) error {
	if w*h > maxRenderPx {
		return &userError{Status: http.StatusUnprocessableEntity,
			Msg: fmt.Sprintf("page %d is too large to print as a label", page)}
	}
	return nil
}

// canvas is the label in printer dots; zero when the media size is unknown.
type canvas struct{ W, H int }

func canvasFor(m labelSize, ok bool, dpi int) canvas {
	if !ok {
		return canvas{}
	}
	return canvas{
		W: int(math.Round(m.WidthMm / 25.4 * float64(dpi))),
		H: int(math.Round(m.HeightMm / 25.4 * float64(dpi))),
	}
}

func (c canvas) known() bool     { return c.W > 0 && c.H > 0 }
func (c canvas) landscape() bool { return c.W > c.H }

// runs groups sorted page numbers into contiguous [first, last] runs.
func runs(pages []int) [][2]int {
	var out [][2]int
	for _, p := range pages {
		if n := len(out); n > 0 && out[n-1][1] == p-1 {
			out[n-1][1] = p
			continue
		}
		out = append(out, [2]int{p, p})
	}
	return out
}

func fmtDPI(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

func (d *pdfDoc) scratch(prefix string) string {
	d.seq++
	return filepath.Join(d.dir, fmt.Sprintf("%s-%d", prefix, d.seq))
}

// renderPages renders whole pages as displayed (crop box, /Rotate applied) as
// PNG, one pdftoppm call per contiguous run. aa false turns anti-aliasing off.
func (d *pdfDoc) renderPages(ctx context.Context, pages []int, dpi float64, aa bool) (map[int][]byte, error) {
	dir := d.scratch("pages")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for _, p := range pages {
		pw, ph := d.geom(p).size()
		if err := checkRender(p, math.Ceil(pw/72*dpi), math.Ceil(ph/72*dpi)); err != nil {
			return nil, err
		}
	}
	sorted := slices.Sorted(slices.Values(pages))
	for _, run := range runs(slices.Compact(sorted)) {
		args := []string{"-png", "-gray", "-cropbox", "-r", fmtDPI(dpi)}
		if !aa {
			args = append(args, "-aa", "no", "-aaVector", "no")
		}
		args = append(args, "-f", strconv.Itoa(run[0]), "-l", strconv.Itoa(run[1]), d.path, filepath.Join(dir, "p"))
		if _, err := d.r.run(ctx, "pdftoppm", args...); err != nil {
			return nil, fmt.Errorf("rendering pages %d–%d: %w", run[0], run[1], err)
		}
	}
	// Names are zero-padded to the document's page-count digits: read them back.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[int][]byte{}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".png")
		n, err := strconv.Atoi(name[strings.LastIndexByte(name, '-')+1:])
		if err != nil {
			continue
		}
		if out[n], err = os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
			return nil, err
		}
	}
	for _, p := range pages {
		if out[p] == nil {
			return nil, fmt.Errorf("page %d didn't render", p)
		}
	}
	return out, nil
}

// renderBoxAt rasterises one box of a page, grayscale, at dpiX × dpiY (they
// differ only for path A's check, which renders at the bitmap's own
// resolution on each axis).
func (d *pdfDoc) renderBoxAt(ctx context.Context, page int, b box, dpiX, dpiY float64, aa bool) (*image.Gray, error) {
	pw, ph := d.geom(page).size()
	fw, fh := pw/72*dpiX, ph/72*dpiY
	if err := checkRender(page, math.Ceil(b.W*fw)+1, math.Ceil(b.H*fh)+1); err != nil { // +1: pixels rounds outwards
		return nil, err
	}
	r := b.pixels(fw, fh)
	if r.Dx() < 1 || r.Dy() < 1 {
		return nil, fmt.Errorf("page %d: the box is empty", page)
	}
	base := d.scratch("box")
	args := []string{"-png", "-gray", "-cropbox", "-rx", fmtDPI(dpiX), "-ry", fmtDPI(dpiY)}
	if !aa {
		args = append(args, "-aa", "no", "-aaVector", "no")
	}
	n := strconv.Itoa(page)
	args = append(args,
		"-x", strconv.Itoa(r.Min.X), "-y", strconv.Itoa(r.Min.Y), "-W", strconv.Itoa(r.Dx()), "-H", strconv.Itoa(r.Dy()),
		"-f", n, "-l", n, "-singlefile", d.path, base)
	if _, err := d.r.run(ctx, "pdftoppm", args...); err != nil {
		return nil, fmt.Errorf("rendering page %d: %w", page, err)
	}
	defer os.Remove(base + ".png")
	data, err := os.ReadFile(base + ".png")
	if err != nil {
		return nil, fmt.Errorf("rendering page %d: %w", page, err)
	}
	img, err := decodePNGBytes(data)
	if err != nil {
		return nil, fmt.Errorf("rendering page %d: %w", page, err)
	}
	return toGray(img), nil // poppler may write -gray as RGB
}

// renderLabel is path B: the box rendered at the printer's resolution
// (anti-aliasing off), thresholded, cropped to its ink, turned to the
// canvas's orientation and centred on it. Carriers want labels at true size,
// so it's never enlarged, and it's shrunk only when the ink itself doesn't
// fit: the box's white padding is never a reason to shrink. scale is the
// render's size against true size: 1, or below 1 when it was shrunk.
func (d *pdfDoc) renderLabel(ctx context.Context, page int, b box, cv canvas) (g *image.Gray, scale float64, err error) {
	dpi := float64(d.r.dpi)
	if g, err = d.renderBoxAt(ctx, page, b, dpi, dpi, false); err != nil {
		return nil, 0, err
	}
	g = threshold(g, 128)
	if !cv.known() {
		return fitCanvas(g, cv), 1, nil
	}
	ink, ok := inkBounds(g, g.Rect, 128)
	if !ok { // a blank box: nothing to fit
		return fitCanvas(g, cv), 1, nil
	}
	if scale = inkFit(ink, cv); scale == 1 {
		// Centring the ink rather than the box: centre adds the white.
		return fitCanvas(subGray(g, ink), cv), 1, nil
	}
	if g, err = d.renderBoxAt(ctx, page, b, dpi*scale, dpi*scale, false); err != nil {
		return nil, 0, err
	}
	g = threshold(g, 128)
	if ink, ok = inkBounds(g, g.Rect, 128); ok { // a fractional dpi can give a dot too many: centre crops it
		g = subGray(g, ink)
	}
	return fitCanvas(g, cv), scale, nil
}

// inkFit is what path B scales ink of this size (at the printer's
// resolution) by: 1 when it fits the canvas as fitCanvas will turn it, else
// the largest scale that fits.
func inkFit(ink image.Rectangle, cv canvas) float64 {
	iw, ih := float64(ink.Dx()), float64(ink.Dy())
	if iw != ih && (iw > ih) != cv.landscape() {
		iw, ih = ih, iw // as fitCanvas will turn it
	}
	if iw <= float64(cv.W) && ih <= float64(cv.H) {
		return 1
	}
	return min(float64(cv.W)/iw, float64(cv.H)/ih)
}

// labelScale is the scale label gives b, without making the label: 1 when
// path A applies, else path B's first render measured as renderLabel
// measures it. The page shows it on "include" before the tab is included.
func (d *pdfDoc) labelScale(ctx context.Context, page int, b box, cv canvas) (float64, error) {
	if !cv.known() {
		return 1, nil
	}
	if g, _, _ := d.native(ctx, page, b, cv); g != nil {
		return 1, nil
	}
	dpi := float64(d.r.dpi)
	g, err := d.renderBoxAt(ctx, page, b, dpi, dpi, false)
	if err != nil {
		return 0, err
	}
	ink, ok := inkBounds(threshold(g, 128), g.Rect, 128)
	if !ok {
		return 1, nil
	}
	return inkFit(ink, cv), nil
}

// fitCanvas turns a label to the canvas's orientation and centres it on a
// white canvas, cropping any overflow (a fractional dpi can give one dot too
// many). Without a canvas it only turns landscape labels to portrait.
func fitCanvas(g *image.Gray, cv canvas) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	if !cv.known() {
		if w > h {
			return rotate(g, 1)
		}
		return g
	}
	if w != h && (w > h) != cv.landscape() {
		g = rotate(g, 1)
	}
	return centre(g, cv.W, cv.H)
}
