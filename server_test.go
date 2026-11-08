package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// fakeLPrint answers the operations the app uses and records every request it
// gets, decoded.
type fakeLPrint struct {
	t        *testing.T
	mu       sync.Mutex
	requests []*ippMessage
	nextJob  int
	// printStatus, if set, fails Print-Job with that status and message.
	printStatus  uint16
	printMessage string
	reasons      []string
}

// cancelStates are the jobs Cancel-Job knows, by id: the two Get-Jobs lists
// (completed, aborted) plus a held one. Any other id isn't found, as in PAPPL.
var cancelStates = map[int]int{1: 9, 2: 8, 3: 4}

func (f *fakeLPrint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("Content-Type") != "application/ipp" {
		f.t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
	}
	m, err := parseIPP(body)
	if err != nil {
		f.t.Errorf("fake lprint got an unparseable request: %v", err)
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, m)
	f.mu.Unlock()

	var resp *ippResponseBuilder
	switch m.Code {
	case opGetPrinterAttributes:
		resp = newIPPResponse(0, m.RequestID)
		resp.group(tagPrinterGroup)
		resp.integer(tagEnum, "printer-state", 3)
		resp.str(tagText, "printer-state-message", "Idle.")
		reasons := f.reasons
		if len(reasons) == 0 {
			reasons = []string{"none"}
		}
		resp.strs(tagKeyword, "printer-state-reasons", reasons...)
		resp.strs(tagKeyword, "media-supported", "oe_2x1-label_2x1in", "na_index-4x6_4x6in", "roll_max_4x39.6in")
		resp.strs(tagKeyword, "media-ready", "na_index-4x6_4x6in")
		resp.str(tagKeyword, "media-default", "na_index-4x6_4x6in")
	case opPrintJob:
		if f.printStatus != 0 {
			resp = newIPPResponse(f.printStatus, m.RequestID)
			resp.str(tagText, "status-message", f.printMessage)
			break
		}
		f.mu.Lock()
		f.nextJob++
		id := f.nextJob
		f.mu.Unlock()
		resp = newIPPResponse(0, m.RequestID)
		resp.group(tagJobGroup)
		resp.integer(tagInteger, "job-id", int32(id))
		resp.integer(tagEnum, "job-state", 3)
		resp.str(tagKeyword, "job-state-reasons", "job-incoming")
	case opGetJobs:
		resp = newIPPResponse(0, m.RequestID)
		for id, st := range []int32{9, 8} {
			resp.group(tagJobGroup)
			resp.integer(tagInteger, "job-id", int32(id+1))
			resp.str(tagName, "job-name", "label.pdf")
			resp.integer(tagEnum, "job-state", st)
			resp.str(tagText, "job-state-message", "Unable to connect to printer.")
		}
	case opCancelJob:
		// PAPPL's messages: client-ipp.c ("job-id %d not found.") and
		// job-ipp.c ipp_cancel_job ("Job #%d is already %s - can't cancel.").
		id := opAttr(m, "job-id").int()
		st, ok := cancelStates[id]
		switch {
		case !ok:
			resp = newIPPResponse(0x0406, m.RequestID)
			resp.str(tagText, "status-message", fmt.Sprintf("job-id %d not found.", id))
		case st >= 7:
			resp = newIPPResponse(0x0404, m.RequestID)
			resp.str(tagText, "status-message", fmt.Sprintf("Job #%d is already %s - can't cancel.", id, jobStates[st]))
		default:
			resp = newIPPResponse(0, m.RequestID)
		}
	default:
		resp = newIPPResponse(0x0501, m.RequestID)
	}
	w.Header().Set("Content-Type", "application/ipp")
	_, _ = w.Write(resp.mustBytes(f.t))
}

func (f *fakeLPrint) printJobs() []*ippMessage { return f.ops(opPrintJob) }

func (f *fakeLPrint) ops(op uint16) []*ippMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ippMessage
	for _, m := range f.requests {
		if m.Code == op {
			out = append(out, m)
		}
	}
	return out
}

func newTestServer(t *testing.T, mutate ...func(*config)) (*fakeLPrint, http.Handler) {
	t.Helper()
	fake, s := newTestServerS(t, mutate...)
	return fake, s.routes()
}

func newTestServerS(t *testing.T, mutate ...func(*config)) (*fakeLPrint, *server) {
	t.Helper()
	fake := &fakeLPrint{t: t}
	lp := httptest.NewServer(fake)
	t.Cleanup(lp.Close)
	cfg := config{
		LPrintURI:    "ipp" + strings.TrimPrefix(lp.URL, "http") + "/ipp/print/Zebra-GX420t",
		MaxUpload:    20 << 20,
		MaxPages:     50,
		MaxCopies:    100,
		DefaultMedia: "na_index-4x6_4x6in",
		DPI:          203,
		UserHeader:   "Remote-User",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	ipp, err := newIPPClient(cfg.LPrintURI)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	s := &server{cfg: cfg, ipp: ipp, pdf: newPDFRenderer(cfg.DPI, log), zpl: newZPLRenderer(cfg.DPI),
		shares: newShareStore(cfg.MaxUpload), log: log}
	return fake, s
}

func multipartBody(t *testing.T, filename string, data []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func post(t *testing.T, h http.Handler, path, filename string, data []byte, fields map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, ct := multipartBody(t, filename, data, fields)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func jobAttr(m *ippMessage, name string) *ippAttr { return m.group(tagJobGroup).get(name) }
func opAttr(m *ippMessage, name string) *ippAttr  { return m.group(tagOperationGroup).get(name) }

func requirePoppler(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"pdfinfo", "pdftoppm", "pdfimages", "pdftohtml"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not installed: CI must run the PDF tests", tool)
			}
			t.Skipf("%s not installed (poppler-utils)", tool)
		}
	}
}

// ---------------------------------------------------------------- print

func TestPrintZPLRepeatsForCopies(t *testing.T) {
	fake, h := newTestServer(t)
	zpl := []byte("^XA^FO50,50^ADN,36,20^FDhello^FS^XZ")
	rec, out := post(t, h, "/api/print", "ship.zpl", zpl, map[string]string{"copies": "3", "media": "oe_2x1-label_2x1in"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Print-Jobs, want 1", len(jobs))
	}
	m := jobs[0]
	if got := opAttr(m, "document-format").str(); got != "application/vnd.zebra-zpl" {
		t.Errorf("document-format = %q", got)
	}
	if got := opAttr(m, "requesting-user-name").str(); got != "alice" {
		t.Errorf("user = %q", got)
	}
	if got := opAttr(m, "job-name").str(); got != "ship.zpl ×3" {
		t.Errorf("job-name = %q", got)
	}
	if want := strings.Repeat(string(zpl)+"\n", 3); string(m.Data) != want {
		t.Errorf("document = %q", m.Data)
	}
	// LPrint ignores both for raw ZPL; sending them would only mislead.
	if g := m.group(tagJobGroup); g != nil && (g.get("copies") != nil || g.get("media") != nil) {
		t.Errorf("unexpected job attributes on a ZPL job: %+v", g.Attrs)
	}
	if n := len(out["jobs"].([]any)); n != 1 {
		t.Errorf("response lists %d jobs", n)
	}
}

func TestPrintPNG(t *testing.T) {
	fake, h := newTestServer(t)
	img := []byte("\x89PNG\r\n\x1a\n-not-really-but-sniffed")
	rec, _ := post(t, h, "/api/print", "label.png", img, map[string]string{"copies": "2", "media": "na_index-4x6_4x6in"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	m := fake.printJobs()[0]
	checks := map[string]string{
		"document-format": opAttr(m, "document-format").str(),
		"media":           jobAttr(m, "media").str(),
		"print-scaling":   jobAttr(m, "print-scaling").str(),
	}
	want := map[string]string{"document-format": "image/png", "media": "na_index-4x6_4x6in", "print-scaling": "fit"}
	for k, v := range want {
		if checks[k] != v {
			t.Errorf("%s = %q, want %q", k, checks[k], v)
		}
	}
	if got := jobAttr(m, "copies").int(); got != 2 {
		t.Errorf("copies = %d", got)
	}
	if !bytes.Equal(m.Data, img) {
		t.Error("png must be sent unchanged")
	}
}

func TestPrintPDFOneJobPerPage(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	rec, out := post(t, h, "/api/print", "carrier.pdf", makeLabelPDF(3), map[string]string{"pages": "1,3"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d Print-Jobs, want 2", len(jobs))
	}
	for i, wantName := range []string{"carrier.pdf · p1/3", "carrier.pdf · p3/3"} {
		m := jobs[i]
		if got := opAttr(m, "job-name").str(); got != wantName {
			t.Errorf("job-name = %q, want %q", got, wantName)
		}
		if got := opAttr(m, "document-format").str(); got != "image/png" {
			t.Errorf("document-format = %q", got)
		}
		if got := jobAttr(m, "media").str(); got != "na_index-4x6_4x6in" {
			t.Errorf("media = %q (default should apply)", got)
		}
		img, err := png.Decode(bytes.NewReader(m.Data))
		if err != nil {
			t.Fatalf("page %d isn't a png: %v", i, err)
		}
		// 4×6 in at 203 dpi
		if b := img.Bounds(); b.Dx() != 812 || b.Dy() != 1218 {
			t.Errorf("page %d is %dx%d, want 812x1218", i, b.Dx(), b.Dy())
		}
		for _, v := range toGray(img).Pix {
			if v != 0 && v != 255 {
				t.Fatalf("page %d has gray pixels", i)
			}
		}
	}
	if n := len(out["jobs"].([]any)); n != 2 {
		t.Errorf("response lists %d jobs", n)
	}
}

func TestPrintPDFRespectsPageLimit(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t, func(c *config) { c.MaxPages = 2 })
	rec, out := post(t, h, "/api/print", "big.pdf", makeLabelPDF(3), nil)
	if rec.Code != 400 || !strings.Contains(out["error"].(string), "limit is 2") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("nothing should be sent")
	}
}

func TestPrintRejects(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		data     []byte
		fields   map[string]string
		maxBytes int64
		status   int
	}{
		{"gif", "x.gif", []byte("GIF89a......"), nil, 0, 415},
		{"plain text", "notes.txt", []byte("hello"), nil, 0, 415},
		{"too big", "big.zpl", bytes.Repeat([]byte("^XA^XZ"), 400), nil, 1024, 413},
		{"copies 0", "a.zpl", []byte("^XA^XZ"), map[string]string{"copies": "0"}, 0, 400},
		{"copies junk", "a.zpl", []byte("^XA^XZ"), map[string]string{"copies": "2x"}, 0, 400},
		{"bad media", "a.png", []byte("\x89PNG\r\n\x1a\n"), map[string]string{"media": "Letter; rm -rf"}, 0, 400},
		{"empty", "a.zpl", nil, nil, 0, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake, h := newTestServer(t, func(cfg *config) {
				if c.maxBytes > 0 {
					cfg.MaxUpload = c.maxBytes
				}
			})
			rec, out := post(t, h, "/api/print", c.file, c.data, c.fields)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			if out["error"] == "" {
				t.Error("no error message")
			}
			if len(fake.printJobs()) != 0 {
				t.Error("a rejected upload reached lprint")
			}
		})
	}
}

func TestPrintShowsLPrintError(t *testing.T) {
	fake, h := newTestServer(t)
	fake.printStatus, fake.printMessage = 0x040A, "Unsupported format 'image/png'."
	rec, out := post(t, h, "/api/print", "a.png", []byte("\x89PNG\r\n\x1a\n"), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if msg := out["error"].(string); !strings.Contains(msg, "document-format-not-supported") || !strings.Contains(msg, "Unsupported format") {
		t.Fatalf("error = %q", msg)
	}
}

func TestPrintWarnsAboutPrinterState(t *testing.T) {
	fake, h := newTestServer(t)
	fake.reasons = []string{"media-empty-error", "offline-report"}
	rec, out := post(t, h, "/api/print", "a.zpl", []byte("^XA^XZ"), nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	w, _ := json.Marshal(out["warnings"])
	if !strings.Contains(string(w), "media-empty-error") || !strings.Contains(string(w), "offline-report") {
		t.Fatalf("warnings = %s", w)
	}
}

func TestPrintWhenLPrintDown(t *testing.T) {
	_, h := newTestServer(t, func(c *config) { c.LPrintURI = "ipp://127.0.0.1:1/ipp/print/x" })
	rec, out := post(t, h, "/api/print", "a.zpl", []byte("^XA^XZ"), nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "can't reach lprint at 127.0.0.1:1") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	fake, h := newTestServer(t)
	body, ct := multipartBody(t, "a.zpl", []byte("^XA^XZ"), nil)
	req := httptest.NewRequest(http.MethodPost, "/api/print", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("cross-site request printed")
	}
}

// ---------------------------------------------------------------- preview, printer, jobs, static

func TestPreviewPDF(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	rec, out := post(t, h, "/api/preview", "x.pdf", makeLabelPDF(2), nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if out["kind"] != "pdf" || out["pageCount"] != 2.0 || out["selected"] != 2.0 || out["labels"] != 2.0 {
		t.Fatalf("got %v", out)
	}
	if out["widthIn"] != 4.0 || out["heightIn"] != 6.0 {
		t.Errorf("media size %v × %v", out["widthIn"], out["heightIn"])
	}
	pages := out["pages"].([]any)
	p := pages[0].(map[string]any)
	if len(pages) != 2 || !strings.HasPrefix(p["image"].(string), "data:image/png;base64,") ||
		p["auto"] != true || p["widthIn"] != 4.0 || len(p["boxes"].([]any)) != 1 {
		t.Fatalf("page = %v", p)
	}
	li := p["labelImages"].([]any)[0].(map[string]any)
	if li["method"] != "render" || !strings.HasPrefix(li["image"].(string), "data:image/png;base64,") {
		t.Fatalf("label image = %v", li)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("preview printed")
	}

	rec, out = post(t, h, "/api/preview", "x.pdf", makeLabelPDF(2), map[string]string{"pages": "3"})
	if rec.Code != 400 || !strings.Contains(out["error"].(string), "outside") || out["field"] != "pages" {
		t.Fatalf("bad range: status %d: %s", rec.Code, rec.Body)
	}
}

func TestPreviewNativeAndCrops(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	rec, out := post(t, h, "/api/preview", "ups.pdf", fixUPS2up(true, 200), nil) // no media: default applies
	if rec.Code != 200 || out["labels"] != 2.0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, li := range out["pages"].([]any)[0].(map[string]any)["labelImages"].([]any) {
		if m := li.(map[string]any); m["method"] != "native" || m["ppi"] != 200.0 {
			t.Errorf("label %v", m)
		}
	}

	// 8 boxes written to 6 decimals is over 300 bytes: past the old 256-byte field cap
	var boxes []string
	for i := range 8 {
		boxes = append(boxes, fmt.Sprintf("[%.6f, 0.100000, 0.125000, 0.200000]", float64(i)*0.1))
	}
	if n := len(strings.Join(boxes, ",")); n <= 300 {
		t.Fatalf("crops is only %d bytes; the test needs more than 256", n)
	}
	crops := `{"1": [` + strings.Join(boxes, ",") + `]}`
	rec, out = post(t, h, "/api/preview", "x.pdf", makeLabelPDF(2), map[string]string{"crops": crops})
	if rec.Code != 200 || out["labels"] != 9.0 {
		t.Fatalf("crops: status %d labels %v: %s", rec.Code, out["labels"], rec.Body)
	}
	if p := out["pages"].([]any)[0].(map[string]any); p["auto"] == true || len(p["boxes"].([]any)) != 8 {
		t.Errorf("page 1 should use the crops: %v", p["boxes"])
	}

	rec, out = post(t, h, "/api/preview", "x.pdf", makeLabelPDF(1), map[string]string{"crops": `{"1": [[0,0,0.01,0.01]]}`})
	if rec.Code != 400 || out["field"] != "crops" {
		t.Fatalf("bad crops: status %d: %s", rec.Code, rec.Body)
	}

	rec, out = post(t, h, "/api/preview", "x.pdf", makeLabelPDF(1), map[string]string{"layout": "page"})
	b := out["pages"].([]any)[0].(map[string]any)["boxes"].([]any)[0].([]any)
	if rec.Code != 200 || b[0] != 0.0 || b[2] != 1.0 {
		t.Fatalf("layout=page: %v", b)
	}
}

func TestPreviewLabelScale(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	scales := func(pdf []byte) []float64 {
		t.Helper()
		rec, out := post(t, h, "/api/preview", "x.pdf", pdf, nil)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var s []float64
		for _, li := range out["pages"].([]any)[0].(map[string]any)["labelImages"].([]any) {
			v, ok := li.(map[string]any)["scale"].(float64)
			if !ok {
				t.Fatalf("label %v has no scale", li)
			}
			s = append(s, v)
		}
		return s
	}
	if s := scales(fixWide()); len(s) != 1 || s[0] >= 1 || s[0] < 0.9 {
		t.Errorf("wide label scales = %v, want one just under 1", s)
	}
	if s := scales(fixUPS2up(true, 200)); len(s) != 2 || s[0] != 1 || s[1] != 1 {
		t.Errorf("native label scales = %v, want 1 each", s)
	}
}

// "include" on a doc tab shows the scale before it's chosen: the figure the
// preview gives once the page sends the joined box as its crops.
func TestPreviewTabScale(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	pdf := fixReturn(1340, 206, 196)
	rec, out := post(t, h, "/api/preview", "x.pdf", pdf, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	p := out["pages"].([]any)[0].(map[string]any)
	ts, ok := p["tabScales"].([]any)
	if !ok || len(ts) != 1 {
		t.Fatalf("tabScales = %v", p["tabScales"])
	}
	want := ts[0].(float64)
	if want <= 0 || want >= 1 {
		t.Fatalf("include scale %v, want a shrink", want)
	}
	if s := p["labelImages"].([]any)[0].(map[string]any)["scale"]; s != 1.0 {
		t.Errorf("the label on its own is scaled to %v, want 1", s)
	}
	// as the page builds it: unionBox of the two boxes as sent, to 4 decimals
	f := func(v any) box {
		a := v.([]any)
		return box{a[0].(float64), a[1].(float64), a[2].(float64), a[3].(float64)}
	}
	u := f(p["boxes"].([]any)[0]).union(f(p["tabs"].([]any)[0])).round4()
	crops := fmt.Sprintf(`{"1": [[%v, %v, %v, %v]]}`, u.X, u.Y, u.W, u.H)
	rec, out = post(t, h, "/api/preview", "x.pdf", pdf, map[string]string{"crops": crops})
	if rec.Code != 200 {
		t.Fatalf("include: status %d: %s", rec.Code, rec.Body)
	}
	p = out["pages"].([]any)[0].(map[string]any)
	if got := p["labelImages"].([]any)[0].(map[string]any)["scale"]; got != want {
		t.Errorf("included label scaled to %v, but include said %v", got, want)
	}
	if p["tabScales"] != nil {
		t.Errorf("a page with crops has no detected tabs: %v", p["tabScales"])
	}

	// no doc tab, no tabScales
	_, out = post(t, h, "/api/preview", "x.pdf", makeLabelPDF(1), nil)
	if v := out["pages"].([]any)[0].(map[string]any)["tabScales"]; v != nil {
		t.Errorf("tabScales without a tab: %v", v)
	}
}

func TestPreviewAndPrintSkipPagesWithNoLabel(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	rec, out := post(t, h, "/api/preview", "x.pdf", fixLabelThenText(), nil)
	if rec.Code != 200 || out["labels"] != 1.0 {
		t.Fatalf("status %d labels %v: %s", rec.Code, out["labels"], rec.Body)
	}
	pages := out["pages"].([]any)
	if p := pages[0].(map[string]any); p["skipped"] != nil || len(p["labelImages"].([]any)) != 1 {
		t.Errorf("page 1 = skipped %v, %v label images", p["skipped"], p["labelImages"])
	}
	p := pages[1].(map[string]any)
	if p["skipped"] != true || p["labelImages"] != nil || len(p["boxes"].([]any)) != 1 {
		t.Errorf("page 2 should be skipped with its fallback box and no label images: %v", p)
	}

	if rec, _ := post(t, h, "/api/print", "x.pdf", fixLabelThenText(), nil); rec.Code != 200 {
		t.Fatalf("print: status %d: %s", rec.Code, rec.Body)
	}
	if n := len(fake.printJobs()); n != 1 {
		t.Fatalf("%d jobs, want 1: the skipped page sends none", n)
	}
	rec, _ = post(t, h, "/api/print", "x.pdf", fixLabelThenText(), map[string]string{"crops": `{"2": [[0,0,1,1]]}`})
	if rec.Code != 200 {
		t.Fatalf("print with crops: status %d: %s", rec.Code, rec.Body)
	}
	if n := len(fake.printJobs()); n != 3 {
		t.Fatalf("%d jobs in all, want 3: including page 2 sends it", n-1)
	}
}

func TestPrintSkipsPagesWithNoLabelWhenOnePageIsEdited(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	rec, _ := post(t, h, "/api/print", "x.pdf", fixLabelThenText(), map[string]string{"crops": `{"1": [[0.2,0.2,0.5,0.5]]}`})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if n := len(fake.printJobs()); n != 1 {
		t.Fatalf("%d jobs, want 1: page 2 has no label and page 1 was edited", n)
	}
}

func TestPrintPDFNamesEachLabel(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	rec, _ := post(t, h, "/api/print", "two.pdf", fix2up(), map[string]string{"copies": "3"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d jobs", len(jobs))
	}
	for i, want := range []string{"two.pdf · label 1/2", "two.pdf · label 2/2"} {
		if got := opAttr(jobs[i], "job-name").str(); got != want {
			t.Errorf("job-name = %q, want %q", got, want)
		}
		if got := jobAttr(jobs[i], "copies").int(); got != 3 {
			t.Errorf("copies = %d", got)
		}
	}
}

func TestPreviewRejectsBrokenPDF(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	rec, out := post(t, h, "/api/preview", "x.pdf", []byte("%PDF-1.4\ngarbage"), nil)
	if rec.Code != 400 || !strings.Contains(out["error"].(string), "couldn't read that pdf") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestPrinterEndpoint(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/printer", nil))
	var p printerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !p.Reachable || p.State != "idle" || p.Queue != "Zebra-GX420t" || len(p.Reasons) != 0 {
		t.Fatalf("got %+v", p)
	}
	if len(p.Media) != 2 || p.Media[1].Name != "na_index-4x6_4x6in" || !p.Media[1].Ready {
		t.Fatalf("media = %+v", p.Media)
	}
}

func TestPrinterEndpointWhenDown(t *testing.T) {
	_, h := newTestServer(t, func(c *config) { c.LPrintURI = "ipp://127.0.0.1:1/ipp/print/x" })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/printer", nil))
	var p printerResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || p.Reachable || p.Error == "" || len(p.Media) != 1 {
		t.Fatalf("status %d: %+v", rec.Code, p)
	}
}

func TestJobsEndpoint(t *testing.T) {
	fake, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	var out struct{ Jobs []jobInfo }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Jobs) != 2 || out.Jobs[0].State != "completed" || out.Jobs[1].State != "aborted" {
		t.Fatalf("jobs = %+v", out.Jobs)
	}
	req := fake.requests[0]
	if got := opAttr(req, "which-jobs").str(); got != "all" {
		t.Errorf("which-jobs = %q", got)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/", "/app.js", "/app.css", "/tokens.css", "/fonts/geist-latin.woff2", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Errorf("%s: %d", path, rec.Code)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s: csp = %q", path, csp)
		}
	}
}

// ---------------------------------------------------------------- cancel

func cancelJob(t *testing.T, h http.Handler, id, site string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+id+"/cancel", nil)
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestCancelJob(t *testing.T) {
	fake, h := newTestServer(t)
	rec, out := cancelJob(t, h, "3", "same-origin")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if out["id"] != 3.0 {
		t.Errorf("response = %v", out)
	}
	reqs := fake.ops(opCancelJob)
	if len(reqs) != 1 {
		t.Fatalf("%d Cancel-Jobs, want 1", len(reqs))
	}
	m := reqs[0]
	// RFC 8011 §4.1.5: charset, language, then the target (printer-uri +
	// job-id), then requesting-user-name.
	var names []string
	for _, a := range m.group(tagOperationGroup).Attrs {
		names = append(names, a.Name)
	}
	want := []string{"attributes-charset", "attributes-natural-language", "printer-uri", "job-id", "requesting-user-name"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("operation attributes = %v, want %v", names, want)
	}
	if a := opAttr(m, "job-id"); a == nil || a.Tag != tagInteger || a.int() != 3 {
		t.Errorf("job-id = %+v", a)
	}
	if got := opAttr(m, "printer-uri").str(); !strings.HasSuffix(got, "/ipp/print/Zebra-GX420t") {
		t.Errorf("printer-uri = %q", got)
	}
	// the same user the print path sends, so PAPPL sees the job's owner
	if a := opAttr(m, "requesting-user-name"); a == nil || a.Tag != tagName || a.str() != "alice" {
		t.Errorf("requesting-user-name = %+v", a)
	}
	if len(m.groups(tagJobGroup)) != 0 || len(m.Data) != 0 {
		t.Error("Cancel-Job carries no job attributes or document")
	}
}

func TestCancelJobRejectsBadIDs(t *testing.T) {
	for _, id := range []string{"0", "-1", "+3", "03", "3.0", "3e2", "abc", "%20", "0x3", "2147483648", "99999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			fake, h := newTestServer(t)
			rec, out := cancelJob(t, h, id, "same-origin")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if msg, _ := out["error"].(string); msg == "" {
				t.Error("no error message")
			}
			if len(fake.ops(opCancelJob)) != 0 {
				t.Error("a bad id reached lprint")
			}
		})
	}
}

func TestCancelJobOnlyPOST(t *testing.T) {
	fake, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs/3/cancel", nil))
	// a GET falls through to the static files ("GET /"), like GET /api/print
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET: status %d", rec.Code)
	}
	if len(fake.ops(opCancelJob)) != 0 {
		t.Fatal("a GET reached lprint")
	}
}

func TestCancelJobShowsLPrintError(t *testing.T) {
	cases := map[string][]string{
		"1":  {"client-error-not-possible", "Job #1 is already completed - can't cancel."},
		"2":  {"client-error-not-possible", "Job #2 is already aborted - can't cancel."},
		"42": {"client-error-not-found", "job-id 42 not found."},
	}
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			_, h := newTestServer(t)
			rec, out := cancelJob(t, h, id, "same-origin")
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			msg, _ := out["error"].(string)
			for _, w := range want {
				if !strings.Contains(msg, w) {
					t.Errorf("error = %q, want it to contain %q", msg, w)
				}
			}
		})
	}
}

func TestCancelJobWhenLPrintDown(t *testing.T) {
	_, h := newTestServer(t, func(c *config) { c.LPrintURI = "ipp://127.0.0.1:1/ipp/print/x" })
	rec, out := cancelJob(t, h, "3", "same-origin")
	if msg, _ := out["error"].(string); rec.Code != http.StatusBadGateway || !strings.Contains(msg, "can't reach lprint at 127.0.0.1:1") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestCancelJobCrossSiteRejected(t *testing.T) {
	fake, h := newTestServer(t)
	rec, _ := cancelJob(t, h, "3", "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	if len(fake.ops(opCancelJob)) != 0 {
		t.Fatal("cross-site request reached lprint")
	}
}

func TestErrorsNameTheirField(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t, func(c *config) { c.MaxPages = 2 })
	cases := []struct {
		name   string
		fields map[string]string
		field  string
	}{
		{"bad range", map[string]string{"pages": "9"}, "pages"},
		{"too many pages, preview too", nil, "pages"},
		{"field over its cap", map[string]string{"pages": strings.Repeat("1,", 200)}, "pages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, out := post(t, h, "/api/preview", "x.pdf", makeLabelPDF(3), c.fields)
			if rec.Code != 400 || out["field"] != c.field {
				t.Fatalf("status %d, field %v: %s", rec.Code, out["field"], rec.Body)
			}
		})
	}
}

func TestCropsFieldMayExceed256Bytes(t *testing.T) {
	_, h := newTestServer(t)
	// ZPL ignores crops; this only proves the field isn't cut or refused.
	rec, _ := post(t, h, "/api/preview", "a.zpl", []byte("^XA^XZ"), map[string]string{"crops": strings.Repeat(" ", 1000) + "{}"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestHugePageIsA422(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	shimTool(t, "pdftoppm", `echo "pdftoppm ran" >&2; exit 1`) // nothing may be drawn
	for _, c := range []struct {
		path, pages, layout string
		page                int
	}{
		{"/api/preview", "1", "page", 1}, // the 96 dpi page image
		{"/api/preview", "2", "auto", 2}, // the 50 dpi detection render
		{"/api/print", "1", "page", 1},   // the label at 203 dpi
	} {
		rec, out := post(t, h, c.path, "x.pdf", fixHugePages(), map[string]string{"pages": c.pages, "layout": c.layout})
		want := fmt.Sprintf("page %d is too large to print as a label", c.page)
		if rec.Code != 422 || out["error"] != want {
			t.Errorf("%s pages=%s layout=%s: %d %s, want 422 %q", c.path, c.pages, c.layout, rec.Code, rec.Body, want)
		}
	}
	if n := len(fake.printJobs()); n != 0 {
		t.Errorf("%d jobs sent", n)
	}
}
