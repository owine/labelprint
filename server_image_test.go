package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// label200ppi is a synthetic 800 × 1200 black/white label PNG with pHYs
// 7874 ppm (200 ppi): a 4 × 6 in label, as FedEx sends them.
func label200ppi(t testing.TB) []byte {
	t.Helper()
	return withPHYs(encodePNG(t, labelBitmap(800, 1200).gray()), 7874)
}

// upsTabPNG is a synthetic 1400 × 800 UPS-style label at 200 ppi: a 1202 px
// label, then a doc tab carrying bars.
func upsTabPNG(t testing.TB) []byte {
	t.Helper()
	return withPHYs(encodePNG(t, docTabBitmap(1340).gray()), 7874)
}

// screenshotJPEG is a 1400 × 1800 RGB "screenshot" with no density: a
// 700 × 1050 label at the top left and, well clear of it, a 300 × 200
// mid-gray block: 2.4% of the pixels, so it can't pass as black and white
// (isBilevel allows 0.5%).
func screenshotJPEG(t testing.TB) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1400, 1800))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	lbl := labelBitmap(700, 1050).gray()
	draw.Draw(img, lbl.Rect.Add(image.Pt(50, 50)), lbl, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(1050, 1550, 1350, 1750), image.NewUniform(color.Gray{160}), image.Point{}, draw.Src)
	return encodeJPEG(t, img)
}

func labelGIF(t testing.TB) []byte {
	t.Helper()
	g := labelBitmap(800, 1200).gray()
	p := image.NewPaletted(g.Rect, color.Palette{color.Black, color.White})
	for i, v := range g.Pix {
		p.Pix[i] = v / 255
	}
	return encodeGIF(t, &gif.GIF{Image: []*image.Paletted{p}, Delay: []int{0}})
}

func firstPage(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	pages, _ := out["pages"].([]any)
	if len(pages) != 1 {
		t.Fatalf("%d pages, want 1: %v", len(pages), out)
	}
	return pages[0].(map[string]any)
}

func decodeJob(t *testing.T, m *ippMessage) *image.Gray {
	t.Helper()
	if got := opAttr(m, "document-format").str(); got != "image/png" {
		t.Fatalf("document-format = %q", got)
	}
	img, err := png.Decode(bytes.NewReader(m.Data))
	if err != nil {
		t.Fatal(err)
	}
	return toGray(img)
}

func TestPreviewAndPrintNativeImage(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	data := label200ppi(t)
	rec, out := post(t, h, "/api/preview", "label.png", data, map[string]string{"media": "na_index-4x6_4x6in"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if out["kind"] != "png" || out["labels"] != 1.0 || out["widthIn"] != 4.0 || out["heightIn"] != 6.0 {
		t.Fatalf("got %v", out)
	}
	im, _ := json.Marshal(out["image"])
	var info imageInfo
	_ = json.Unmarshal(im, &info)
	if info != (imageInfo{W: 800, H: 1200, PPI: 200, PPISource: "phys"}) {
		t.Errorf("image = %+v", info)
	}
	p := firstPage(t, out)
	if p["widthIn"] != 4.0 || p["heightIn"] != 6.0 || len(p["boxes"].([]any)) != 1 {
		t.Fatalf("page = %v", p)
	}
	li := p["labelImages"].([]any)[0].(map[string]any)
	if li["method"] != "native" || li["ppi"] != 200.0 || li["scale"] != 1.0 {
		t.Fatalf("label = %v", li)
	}

	rec, _ = post(t, h, "/api/print", "label.png", data, nil) // the default media: 4 × 6
	if rec.Code != 200 {
		t.Fatalf("print: status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	if got := opAttr(jobs[0], "job-name").str(); got != "label.png" {
		t.Errorf("job-name = %q", got)
	}
	g := decodeJob(t, jobs[0])
	assertPureLabel(t, g, canvas46)
	// the ink (the whole bitmap: its border reaches every edge) plus its 4 px
	// margin, cut to the image, centred 1:1 on the 812 × 1218 canvas
	assertSourcePixels(t, g, labelBitmap(800, 1200), 6, 9)
}

func TestImageDocTab(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	data := upsTabPNG(t)
	rec, out := post(t, h, "/api/preview", "ups.png", data, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	p := firstPage(t, out)
	tabs, _ := p["tabs"].([]any)
	ts, _ := p["tabScales"].([]any)
	if len(p["boxes"].([]any)) != 1 || len(tabs) != 1 || tabs[0] == nil || len(ts) != 1 {
		t.Fatalf("boxes %v, tabs %v, tabScales %v", p["boxes"], p["tabs"], p["tabScales"])
	}
	include := ts[0].(float64)
	if include < 0.85 || include >= 1 {
		t.Errorf("include scale %v, want about 0.92", include)
	}
	if li := p["labelImages"].([]any)[0].(map[string]any); li["method"] != "native" || li["scale"] != 1.0 {
		t.Errorf("label on its own = %v", li)
	}
	f := func(v any) box {
		a := v.([]any)
		return box{a[0].(float64), a[1].(float64), a[2].(float64), a[3].(float64)}
	}
	b, tab := f(p["boxes"].([]any)[0]), f(tabs[0])
	// include: the joined box, as the page sends it
	u := b.union(tab).round4()
	crops := fmt.Sprintf(`{"1": [[%v, %v, %v, %v]]}`, u.X, u.Y, u.W, u.H)
	rec, out = post(t, h, "/api/preview", "ups.png", data, map[string]string{"crops": crops})
	if rec.Code != 200 {
		t.Fatalf("include: status %d: %s", rec.Code, rec.Body)
	}
	if got := firstPage(t, out)["labelImages"].([]any)[0].(map[string]any)["scale"]; got != include {
		t.Errorf("included label scaled to %v, but include said %v", got, include)
	}
	// print separately: the label, then the tab grown to 0.5 in, as two jobs
	pw, ph := 7.0, 4.0
	gw, gh := max(tab.W, 0.5/pw), max(tab.H, 0.5/ph)
	gt := box{tab.X + tab.W/2 - gw/2, tab.Y + tab.H/2 - gh/2, gw, gh}.round4()
	b = b.round4()
	crops = fmt.Sprintf(`{"1": [[%v, %v, %v, %v], [%v, %v, %v, %v]]}`, b.X, b.Y, b.W, b.H, gt.X, gt.Y, gt.W, gt.H)
	if rec, _ := post(t, h, "/api/print", "ups.png", data, map[string]string{"crops": crops}); rec.Code != 200 {
		t.Fatalf("separately: status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want the label and its tab", len(jobs))
	}
	for i, want := range []string{"ups.png · label 1/2", "ups.png · label 2/2"} {
		if got := opAttr(jobs[i], "job-name").str(); got != want {
			t.Errorf("job-name = %q, want %q", got, want)
		}
		assertPureLabel(t, decodeJob(t, jobs[i]), canvas46)
	}
}

// A screenshot: no density, an aspect no media matches, and gray in it.
// Detection still finds the label, which is rendered and thresholded.
func TestImageScreenshotRenders(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	data := screenshotJPEG(t)
	rec, out := post(t, h, "/api/preview", "shot.jpg", data, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if im := out["image"].(map[string]any); im["ppiSource"] != "default" || im["ppi"] != 203.0 {
		t.Errorf("image = %v", im)
	}
	p := firstPage(t, out)
	boxes := p["boxes"].([]any)
	if p["auto"] != true || p["fallback"] == true || len(boxes) != 1 {
		t.Fatalf("page = auto %v fallback %v boxes %v", p["auto"], p["fallback"], boxes)
	}
	// the label is 700 × 1050 px at (50, 50) on 1400 × 1800: within 0.15 in
	b := boxes[0].([]any)
	for i, want := range []float64{50.0 / 1400, 50.0 / 1800, 700.0 / 1400, 1050.0 / 1800} {
		side := 1400.0
		if i%2 == 1 {
			side = 1800
		}
		if d := (b[i].(float64) - want) * side / 203; d < -0.15 || d > 0.15 {
			t.Errorf("box %v, want about [%.3f %.3f %.3f %.3f]", b, 50.0/1400, 50.0/1800, 0.5, 1050.0/1800)
			break
		}
	}
	if li := p["labelImages"].([]any)[0].(map[string]any); li["method"] != "render" {
		t.Errorf("label = %v", li)
	}
	if rec, _ := post(t, h, "/api/print", "shot.jpg", data, nil); rec.Code != 200 {
		t.Fatalf("print: status %d: %s", rec.Code, rec.Body)
	}
	assertPureLabel(t, decodeJob(t, fake.printJobs()[0]), canvas46)
}

func TestPrintGIF(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	rec, out := post(t, h, "/api/preview", "label.gif", labelGIF(t), nil)
	if rec.Code != 200 || out["kind"] != "gif" {
		t.Fatalf("preview: status %d: %s", rec.Code, rec.Body)
	}
	if im := out["image"].(map[string]any); im["ppiSource"] != "media" || im["ppi"] != 200.0 {
		t.Errorf("image = %v", im) // 800 × 1200 on 4 × 6
	}
	if rec, _ := post(t, h, "/api/print", "label.gif", labelGIF(t), map[string]string{"copies": "2"}); rec.Code != 200 {
		t.Fatalf("print: status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) == 0 {
		t.Fatal("no jobs")
	}
	for _, m := range jobs {
		assertPureLabel(t, decodeJob(t, m), canvas46)
		if got := jobAttr(m, "copies").int(); got != 2 {
			t.Errorf("copies = %d", got)
		}
	}
}

func TestImageLayoutsAndPages(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	data := label200ppi(t)
	// whole image
	rec, out := post(t, h, "/api/preview", "label.png", data, map[string]string{"layout": "page"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := firstPage(t, out)["boxes"].([]any)
	if len(b) != 1 || fmt.Sprint(b[0]) != "[0 0 1 1]" {
		t.Errorf("layout=page boxes = %v", b)
	}
	if rec, _ := post(t, h, "/api/print", "label.png", data, map[string]string{"layout": "page"}); rec.Code != 200 {
		t.Fatalf("print: status %d: %s", rec.Code, rec.Body)
	}
	if n := len(fake.printJobs()); n != 1 {
		t.Errorf("layout=page: %d jobs", n)
	}
	// pages means nothing for an image
	rec, out = post(t, h, "/api/preview", "label.png", data, map[string]string{"pages": "2"})
	if rec.Code != 200 || out["pageCount"] != 1.0 {
		t.Fatalf("pages=2: status %d: %s", rec.Code, rec.Body)
	}
	firstPage(t, out)
	if rec, _ := post(t, h, "/api/print", "label.png", data, map[string]string{"pages": "2-9"}); rec.Code != 200 {
		t.Fatalf("print pages=2-9: status %d: %s", rec.Code, rec.Body)
	}
	// crops are page 1's, worded for the image
	rec, out = post(t, h, "/api/preview", "label.png", data, map[string]string{"crops": `{"1": [[0, 0, 0.05, 0.5]]}`})
	if rec.Code != 400 || out["field"] != "crops" || out["error"] != "box 1 on the image is too small (under 0.5 in)" {
		t.Errorf("tiny crop: %d %s", rec.Code, rec.Body)
	}
	rec, out = post(t, h, "/api/preview", "label.png", data, map[string]string{"crops": `{"1": [[0.6, 0, 0.5, 0.5]]}`})
	if rec.Code != 400 || out["error"] != "box 1 on the image runs off the image" {
		t.Errorf("crop off the image: %d %s", rec.Code, rec.Body)
	}
}

func TestImageWithNoLabelFallsBack(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	g := newWhite(1000, 1000) // 5 × 5 in at 200 ppi with one small block
	for y := 400; y < 500; y++ {
		for x := 400; x < 600; x++ {
			g.Pix[y*g.Stride+x] = 0
		}
	}
	rec, out := post(t, h, "/api/preview", "x.png", withPHYs(encodePNG(t, g), 7874), nil)
	if rec.Code != 200 || out["labels"] != 1.0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if p := firstPage(t, out); p["fallback"] != true || p["skipped"] != nil || len(p["labelImages"].([]any)) != 1 {
		t.Errorf("page = fallback %v skipped %v", p["fallback"], p["skipped"])
	}
}

func TestImageDetectionFailsAtPrint(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	breakDetection(t)
	rec, out := post(t, h, "/api/print", "label.png", label200ppi(t), nil)
	want := "couldn't find the labels in the image this time — try again, or choose whole image"
	if rec.Code != 422 || out["error"] != want {
		t.Fatalf("%d %s, want 422 %q", rec.Code, rec.Body, want)
	}
	if n := len(fake.printJobs()); n != 0 {
		t.Fatalf("%d jobs sent", n)
	}
}

func TestImageErrorsThroughTheAPI(t *testing.T) {
	_, h := newTestServer(t) // every one is refused before poppler runs
	for _, c := range []struct {
		name, file string
		data       []byte
		status     int
		msg        string
	}{
		{"undecodable", "x.png", []byte("\x89PNG\r\n\x1a\nnot really"), 415, "couldn't read that image"},
		{"empty gif", "x.gif", []byte("GIF89a\x00\x00\x00\x00\x00\x00\x00\x3b"), 422, "that image is empty"},
		{"too large", "x.png", pngHeader(100000, 100000, 8, 0, 0), 422, "that image is too large to print as a label"},
		{"too small", "x.png", encodePNG(t, image.NewGray(image.Rect(0, 0, 40, 40))), 422, "that image is too small to print as a label"},
	} {
		for _, path := range []string{"/api/preview", "/api/print"} {
			rec, out := post(t, h, path, c.file, c.data, nil)
			if rec.Code != c.status || out["error"] != c.msg {
				t.Errorf("%s %s: %d %s, want %d %q", c.name, path, rec.Code, rec.Body, c.status, c.msg)
			}
		}
	}
}

// The decode runs inside a pipeline slot: with both held, an image waits
// (here until its deadline) and is never decoded outside one.
func TestImageDecodeWaitsForASlot(t *testing.T) {
	_, s := newTestServerS(t)
	s.pdf.deadline = 300 * time.Millisecond
	h := s.routes()
	var releases []func()
	for range 2 {
		release, err := s.pdf.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	// undecodable: decoded outside a slot it would fail at once with a 415
	bad := []byte("\x89PNG\r\n\x1a\nnot really")
	var wg sync.WaitGroup
	for _, path := range []string{"/api/preview", "/api/print"} {
		wg.Go(func() {
			rec, out := post(t, h, path, "x.png", bad, nil)
			if rec.Code != http.StatusGatewayTimeout || out["error"] != "that image took too long to prepare" {
				t.Errorf("%s with the slots held: %d %s", path, rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
	for _, r := range releases {
		r()
	}
	if rec, _ := post(t, h, "/api/preview", "x.png", bad, nil); rec.Code != 415 {
		t.Errorf("with a slot free: %d %s", rec.Code, rec.Body)
	}
}

func TestShareGIF(t *testing.T) {
	_, h := newTestServer(t)
	data := labelGIF(t)
	rec := sharePost(t, h, "label.gif", data, map[string]string{"Accept": "application/json"})
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out["url"] == "" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := shareGet(h, tokenFrom(t, out["url"]))
	if got.Code != 200 || got.Header().Get("Content-Type") != "image/gif" || !bytes.Equal(got.Body.Bytes(), data) {
		t.Fatalf("status %d, type %q", got.Code, got.Header().Get("Content-Type"))
	}
}

// The preview and the print each log the image's ppi and where it came
// from, and never the filename.
func TestImageLogs(t *testing.T) {
	requirePoppler(t)
	_, s := newTestServerS(t)
	var buf bytes.Buffer
	s.log = slog.New(slog.NewJSONHandler(&buf, nil))
	h := s.routes()
	for _, path := range []string{"/api/preview", "/api/print"} {
		if rec, _ := post(t, h, path, "Jane Doe return.png", label200ppi(t), nil); rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	logs := buf.String()
	for _, want := range []string{`"msg":"preview"`, `"msg":"print"`, `"ppi":200`, `"ppi_source":"phys"`, `"kind":"png"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "Jane") {
		t.Errorf("a filename was logged:\n%s", logs)
	}
}
