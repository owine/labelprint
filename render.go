package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pdfRenderer rasterises PDFs with poppler. Each page becomes an 8-bit
// grayscale PNG at the printer's resolution, which LPrint then dithers to the
// GK420t's 1-bit head. Anti-aliasing is off: smoothed edges would come out of
// that dither as fuzzy barcode bars, while hard edges at 203 dpi map 1:1 to
// printer dots.
type pdfRenderer struct {
	dpi     int
	timeout time.Duration // per poppler invocation
	slots   chan struct{} // caps concurrent renders on a small Pi
}

func newPDFRenderer(dpi int) *pdfRenderer {
	return &pdfRenderer{dpi: dpi, timeout: 60 * time.Second, slots: make(chan struct{}, 2)}
}

// renderedPage is one page's PNG, in page order.
type renderedPage struct {
	Page int
	PNG  []byte
}

// pdfDoc is a PDF written to scratch space (the tmpfs at /tmp in the
// container) for poppler to read. Close removes it.
type pdfDoc struct {
	r     *pdfRenderer
	dir   string
	path  string
	Pages int
}

func (r *pdfRenderer) open(ctx context.Context, data []byte) (*pdfDoc, error) {
	dir, err := os.MkdirTemp("", "labelprint-*")
	if err != nil {
		return nil, fmt.Errorf("scratch space: %w", err)
	}
	d := &pdfDoc{r: r, dir: dir, path: filepath.Join(dir, "in.pdf")}
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
	return d, nil
}

func (d *pdfDoc) Close() { _ = os.RemoveAll(d.dir) }

// render rasterises the given 1-based pages. Pages go one poppler call each
// (-singlefile gives a predictable output name); for label PDFs the re-parse
// per call costs milliseconds.
func (d *pdfDoc) render(ctx context.Context, pages []int) ([]renderedPage, error) {
	select {
	case d.r.slots <- struct{}{}:
		defer func() { <-d.r.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]renderedPage, 0, len(pages))
	for _, p := range pages {
		base := filepath.Join(d.dir, "page-"+strconv.Itoa(p))
		n := strconv.Itoa(p)
		_, err := d.r.run(ctx, "pdftoppm",
			"-png", "-gray", "-r", strconv.Itoa(d.r.dpi),
			"-aa", "no", "-aaVector", "no",
			"-cropbox", // the box viewers show, not the (sometimes larger) media box
			"-f", n, "-l", n, "-singlefile",
			d.path, base)
		if err != nil {
			return nil, fmt.Errorf("rendering page %d: %w", p, err)
		}
		png, err := os.ReadFile(base + ".png")
		if err != nil {
			return nil, fmt.Errorf("rendering page %d: %w", p, err)
		}
		_ = os.Remove(base + ".png")
		out = append(out, renderedPage{Page: p, PNG: png})
	}
	return out, nil
}

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
