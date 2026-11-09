package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed web
var webFS embed.FS

type server struct {
	cfg     config
	ipp     *ippClient
	pdf     *pdfRenderer
	zpl     *zplRenderer
	shares  *shareStore
	checks  *checkStore
	log     *slog.Logger
	printMu sync.Mutex // one submission at a time, so multi-page jobs don't interleave
	// baseCtx ends at shutdown; every barcode check's context derives from it.
	baseCtx context.Context
}

func (s *server) routes() http.Handler {
	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /api/printer", s.handlePrinter)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/preview", s.handlePreview)
	mux.HandleFunc("POST /api/print", s.handlePrint)
	mux.HandleFunc("POST /api/printer/maintenance", s.handleMaintenance)
	mux.HandleFunc("POST /share", s.handleShare)
	mux.HandleFunc("GET /api/share/{token}", s.handleSharedFile)
	mux.HandleFunc("GET /api/check/{token}", s.handleCheck)
	mux.Handle("GET /labelprint.shortcut", shortcutHandler(static))

	// An auth proxy's session cookie rides along on any request to this host,
	// so a cross-site form POST could otherwise print. CrossOriginProtection rejects
	// unsafe methods whose Sec-Fetch-Site/Origin say another site sent them;
	// requests with neither header (curl, scripts) pass.
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; "+
			"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- printer + jobs

type printerResponse struct {
	Queue        string        `json:"queue"`
	URI          string        `json:"uri"`
	Reachable    bool          `json:"reachable"`
	Error        string        `json:"error,omitempty"`
	State        string        `json:"state,omitempty"`
	Message      string        `json:"message,omitempty"`
	Reasons      []string      `json:"reasons,omitempty"`
	Media        []mediaOption `json:"media"`
	DefaultMedia string        `json:"defaultMedia"`
	MaxUploadMB  int64         `json:"maxUploadMB"`
	MaxCopies    int           `json:"maxCopies"`
	MaxPages     int           `json:"maxPages"`
}

func (s *server) handlePrinter(w http.ResponseWriter, r *http.Request) {
	resp := printerResponse{
		Queue:        queueName(s.cfg.LPrintURI),
		URI:          s.cfg.LPrintURI,
		DefaultMedia: s.cfg.DefaultMedia,
		MaxUploadMB:  s.cfg.MaxUpload >> 20,
		MaxCopies:    s.cfg.MaxCopies,
		MaxPages:     s.cfg.MaxPages,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	info, err := s.ipp.printerAttributes(ctx)
	if err != nil {
		// Still return the default size so the page stays usable; printing
		// will surface the real error.
		resp.Error = err.Error()
		resp.Media = []mediaOption{{Name: s.cfg.DefaultMedia, Label: s.cfg.DefaultMedia}}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Reachable = true
	resp.State, resp.Message, resp.Reasons = info.State, info.StateMessage, info.Reasons
	resp.Media = mediaOptions(info.MediaSupported, info.MediaReady)
	if !hasMedia(resp.Media, resp.DefaultMedia) && info.MediaDefault != "" {
		resp.DefaultMedia = info.MediaDefault
	}
	writeJSON(w, http.StatusOK, resp)
}

func hasMedia(opts []mediaOption, name string) bool {
	for _, o := range opts {
		if o.Name == name {
			return true
		}
	}
	return false
}

func queueName(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	return uri
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	jobs, err := s.ipp.jobs(ctx, 15)
	if err != nil {
		s.fail(w, gateway(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": nonNil(jobs)})
}

// handleCancel sends Cancel-Job for a job in LPrint's list, so a held label
// doesn't print when the printer comes back. LPrint decides what can be
// canceled; its refusal (already completed, not found) is passed on as is.
func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseJobID(r.PathValue("id"))
	if !ok {
		s.fail(w, badRequest("a job id is a whole number from 1 up"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := s.ipp.cancelJob(ctx, id, s.requestUser(r))
	s.log.Info("cancel", "job", id, "error", errString(err))
	if err != nil {
		s.fail(w, gateway(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"id": id})
}

// parseJobID accepts only a plain positive integer that fits IPP's job-id
// (1 to 2³¹−1): no sign, no leading zeros, no spaces.
func parseJobID(v string) (int, bool) {
	if v == "" || v[0] < '1' || v[0] > '9' {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// ---------------------------------------------------------------- uploads

type upload struct {
	Filename string
	Data     []byte
	Kind     docKind
	Fields   map[string]string
}

// Form fields other than the file are small. crops can carry up to 8 boxes
// for each previewed page; every other field fits in defaultFieldCap. Going
// over a cap is an error, never a silent cut.
var fieldCaps = map[string]int{"crops": 16 << 10}

const defaultFieldCap = 256

// readUpload streams the multipart body into memory (no temp files: the root
// filesystem is read-only and 20 MB fits comfortably in RAM) and sniffs the
// document type.
func (s *server) readUpload(w http.ResponseWriter, r *http.Request) (*upload, error) {
	tooBig := &userError{Status: http.StatusRequestEntityTooLarge,
		Msg: fmt.Sprintf("that file is over the %d MB limit", s.cfg.MaxUpload>>20)}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload+64<<10) // + room for the form fields
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, badRequest("expected a multipart/form-data upload")
	}
	up := &upload{Fields: map[string]string{}}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if isMaxBytes(err) {
				return nil, tooBig
			}
			return nil, badRequest("couldn't read the upload: %v", err)
		}
		if part.FormName() == "file" {
			data, err := io.ReadAll(io.LimitReader(part, s.cfg.MaxUpload+1))
			if isMaxBytes(err) || int64(len(data)) > s.cfg.MaxUpload {
				return nil, tooBig
			}
			if err != nil {
				return nil, badRequest("couldn't read the upload: %v", err)
			}
			up.Filename, up.Data = cleanFilename(part.FileName()), data
			continue
		}
		name := truncateUTF8(part.FormName(), 40)
		limit := defaultFieldCap
		if c, ok := fieldCaps[name]; ok {
			limit = c
		}
		v, err := io.ReadAll(io.LimitReader(part, int64(limit)+1))
		if isMaxBytes(err) {
			return nil, tooBig
		}
		if len(v) > limit {
			return nil, fieldError(name, "%s is too long", name)
		}
		up.Fields[name] = strings.TrimSpace(string(v))
	}
	if up.Data == nil {
		return nil, badRequest("choose a file first")
	}
	if up.Kind, err = detectKind(up.Filename, up.Data); err != nil {
		return nil, err
	}
	return up, nil
}

func isMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// cleanFilename keeps the base name (browsers may send a path) minus control
// characters; it becomes the IPP job-name.
func cleanFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name = strings.TrimSpace(name); name == "" {
		return "upload"
	}
	return truncateUTF8(name, 200)
}

// ---------------------------------------------------------------- preview

type previewPage struct {
	Page  int    `json:"page"`
	Image string `json:"image"` // data: URL
	// pdf only
	WidthIn  float64 `json:"widthIn,omitempty"` // the page as displayed
	HeightIn float64 `json:"heightIn,omitempty"`
	Auto     bool    `json:"auto,omitempty"`
	Fallback bool    `json:"fallback,omitempty"`
	Skipped  bool    `json:"skipped,omitempty"` // no label here while other pages have one: not printing
	Boxes    []box   `json:"boxes,omitempty"`
	Tabs     []*box  `json:"tabs,omitempty"`
	// parallel to Boxes: the scale its label would print at with its doc tab
	// included (0: no tab, or not previewed), so "include" shows the figure
	// it will get before it's chosen
	TabScales   []float64      `json:"tabScales,omitempty"`
	LabelImages []previewLabel `json:"labelImages,omitempty"`
}

type previewLabel struct {
	Image  string  `json:"image"`  // exactly what LPrint will receive
	Method string  `json:"method"` // native or render
	PPI    int     `json:"ppi,omitempty"`
	Scale  float64 `json:"scale"` // against true size: 1, or below 1 when shrunk to fit
}

const (
	maxPreviewPages  = 12
	maxPreviewLabels = 12
	previewDPI       = 96
)

type previewResponse struct {
	Kind      docKind       `json:"kind"`
	PageCount int           `json:"pageCount,omitempty"` // pages in the pdf
	Selected  int           `json:"selected,omitempty"`  // pages the range picks
	Pages     []previewPage `json:"pages,omitempty"`     // first maxPreviewPages of those
	Labels    int           `json:"labels,omitempty"`    // labels in the zpl, or label boxes across the pdf's selected pages
	// The canvas the labels were drawn on (zpl, and pdf when the media size
	// is known).
	WidthIn  float64 `json:"widthIn,omitempty"`
	HeightIn float64 `json:"heightIn,omitempty"`
	// zpl only: why there's no picture if zebrash couldn't draw one. A failed
	// preview never blocks printing: the printer interprets the zpl itself.
	PreviewError string `json:"previewError,omitempty"`
	// zpl only: its ^PW/^LL are bigger than the selected labels, or look made
	// for 300 dpi. A warning, never a block: the zpl still prints unchanged.
	FitWarning string `json:"fitWarning,omitempty"`
	// png, jpeg and gif: the image's pixels and the ppi it's sized by. The
	// rest of the reply is a one-page pdf's.
	Image *imageInfo `json:"image,omitempty"`
	// pdf and images: the token to poll at /api/check/{token} for the label
	// images' barcode check. Absent when there's nothing to check or the
	// labels are too big to hold.
	Check string `json:"check,omitempty"`
	// zpl only: tracking numbers read from the ^BC/^B3 field data, not
	// decoded from a picture.
	Tracking []zplTrackingEntry `json:"tracking,omitempty"`
}

func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	up, err := s.readUpload(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := previewResponse{Kind: up.Kind}
	switch up.Kind {
	case kindZPL:
		resp.Labels = countZPLLabels(up.Data)
		resp.Tracking = zplTracking(up.Data)
		media, ok := mediaSize(up.Fields["media"])
		if !ok {
			media, _ = mediaSize(s.cfg.DefaultMedia)
		}
		resp.FitWarning = s.zpl.fitWarning(up.Data, media) // from ^PW/^LL alone, so it holds when zebrash fails
		p, err := s.zpl.render(r.Context(), up.Data, media, maxPreviewPages)
		if err != nil {
			// zebrash's drawer errors can quote field data (e.g. "can not
			// encode %q"), so the log gets no error text; the page still
			// shows the uploader why, since it's their own file.
			s.log.Warn("zpl preview failed")
			resp.PreviewError = err.Error()
			break
		}
		resp.Labels = max(resp.Labels, p.Labels)
		resp.WidthIn, resp.HeightIn = p.Size.WidthIn, p.Size.HeightIn
		resp.Pages = dataURLs(p.Pages)
	case kindPDF, kindPNG, kindJPEG, kindGIF:
		labels, err := s.previewPDF(r.Context(), up, &resp)
		if err != nil {
			s.fail(w, err)
			return
		}
		// only once the preview has worked: an error path holds nothing
		if tok, ok := s.checks.put(s.baseCtx, labels); ok {
			resp.Check = tok
		}
		if im := resp.Image; im != nil {
			s.log.Info("preview", "kind", up.Kind, "bytes", len(up.Data), "ppi", im.PPI, "ppi_source", im.PPISource,
				"labels", resp.Labels)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func dataURLs(pages []renderedPage) []previewPage {
	out := make([]previewPage, 0, len(pages))
	for _, p := range pages {
		out = append(out, previewPage{Page: p.Page, Image: dataURL(p.PNG)})
	}
	return out
}

func dataURL(png []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// mediaFor is the label size to detect and draw for: the chosen media, or the
// default when it's missing or has no size in its name.
func (s *server) mediaFor(name string) (labelSize, bool) {
	if m, ok := mediaSize(name); ok {
		return m, true
	}
	return mediaSize(s.cfg.DefaultMedia)
}

func (s *server) labelOpts(fields map[string]string, doc *pdfDoc, pages []int, mediaName string) (labelOpts, error) {
	layout, err := parseLayout(fields["layout"])
	if err != nil {
		return labelOpts{}, err
	}
	crops, flips, err := parseCrops(fields["crops"], pages, func(p int) (float64, float64) { return doc.geom(p).inches() }, doc.noun)
	if err != nil {
		return labelOpts{}, err
	}
	media, ok := s.mediaFor(mediaName)
	return labelOpts{layout: layout, crops: crops, flips: flips, media: media, known: ok,
		canvas: canvasFor(media, ok, s.cfg.DPI)}, nil
}

// docSource is what openPDF opens: an uploaded pdf, or an uploaded image to
// wrap as a one-page pdf. media is the label size the request resolved
// through mediaFor, as labelOpts resolves it: an image's ppi can be matched to
// it, so preview and print must pass the same.
type docSource struct {
	up      *upload
	media   labelSize
	mediaOK bool
}

// openPDF opens the upload and checks its page range against MAX_PAGES,
// before anything is rendered. An image is decoded and wrapped first (so call
// it inside the pipeline slot), is always page 1 whatever pages says, and
// comes with its imageInfo.
func (s *server) openPDF(ctx context.Context, src docSource) (*pdfDoc, []int, *imageInfo, error) {
	up := src.up
	if up.Kind.isImage() {
		pdf, info, err := wrapImage(ctx, up.Data, up.Kind, src.media, src.mediaOK, s.cfg.DPI)
		if err != nil {
			return nil, nil, nil, err
		}
		doc, err := s.pdf.open(ctx, pdf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, nil, ctx.Err()
			}
			// labelprint's own pdf: a bug, not the upload, so a 500
			return nil, nil, nil, errors.New("couldn't open the wrapped image")
		}
		doc.noun = docNoun{image: true}
		return doc, []int{1}, &info, nil
	}
	doc, err := s.pdf.open(ctx, up.Data)
	if err != nil {
		return nil, nil, nil, err
	}
	pages, err := parsePageRange(up.Fields["pages"], doc.Pages)
	if err == nil && len(pages) > s.cfg.MaxPages {
		err = tooManyPages(len(pages), s.cfg.MaxPages)
	}
	if err != nil {
		doc.Close()
		return nil, nil, nil, err
	}
	return doc, pages, nil, nil
}

// previewPDF fills in a pdf's (or a wrapped image's) pages, their label
// boxes and, for the first maxPreviewLabels boxes, the label images exactly
// as they'd be sent. Detection runs on every selected page so labels is the
// full count; pages and label images are capped. It returns those label
// images' PNGs, keyed by page and index into that page's labelImages, for
// the barcode check.
func (s *server) previewPDF(ctx context.Context, up *upload, resp *previewResponse) (checks []checkInput, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.pdf.deadline)
	defer cancel()
	defer func() { err = tookTooLong(ctx, err, up.Kind) }() // deferred after cancel, so it runs first

	release, err := s.pdf.acquire(ctx) // held for every poppler call, pdfinfo included, and an image's decode
	if err != nil {
		return nil, err
	}
	defer release()
	media, mediaOK := s.mediaFor(up.Fields["media"])
	doc, pages, info, err := s.openPDF(ctx, docSource{up: up, media: media, mediaOK: mediaOK})
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	resp.Image = info
	o, err := s.labelOpts(up.Fields, doc, pages, up.Fields["media"])
	if err != nil {
		return nil, err
	}

	resp.PageCount, resp.Selected = doc.Pages, len(pages)
	if o.known {
		resp.WidthIn, resp.HeightIn = round2(o.media.WidthMm/25.4), round2(o.media.HeightMm/25.4)
	}
	plans, err := doc.plan(ctx, pages, o)
	if err != nil {
		return nil, err
	}
	for _, pl := range plans {
		if !pl.Skip {
			resp.Labels += len(pl.Boxes)
		}
	}
	shown := plans[:min(len(plans), maxPreviewPages)]
	shownPages := make([]int, len(shown))
	for i, pl := range shown {
		shownPages[i] = pl.Page
	}
	images, err := doc.renderPages(ctx, shownPages, previewDPI, true)
	if err != nil {
		return nil, &userError{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
	}
	budget := maxPreviewLabels
	for _, pl := range shown {
		pw, ph := doc.geom(pl.Page).inches()
		pp := previewPage{Page: pl.Page, Image: dataURL(images[pl.Page]), WidthIn: round2(pw), HeightIn: round2(ph),
			Auto: pl.Auto, Fallback: pl.Fallback, Skipped: pl.Skip, Boxes: pl.Boxes, Tabs: pl.Tabs}
		for i, b := range pl.Boxes {
			if budget == 0 || pl.Skip { // a skipped page's box isn't printing, so it gets no image
				break
			}
			l, err := doc.label(ctx, pl.Page, b, pl.flip(i), o)
			if err != nil {
				return nil, &userError{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
			}
			checks = append(checks, checkInput{Page: pl.Page, Index: len(pp.LabelImages), PNG: l.PNG})
			pp.LabelImages = append(pp.LabelImages, previewLabel{Image: dataURL(l.PNG), Method: l.Method,
				PPI: int(math.Round(l.PPI)), Scale: round2(l.Scale)})
			budget--
			if i < len(pl.Tabs) && pl.Tabs[i] != nil && o.canvas.known() {
				if pp.TabScales == nil {
					pp.TabScales = make([]float64, len(pl.Boxes))
				}
				// the box "include" sends: both as sent, joined, rounded
				inc := b.round4().union(pl.Tabs[i].round4()).round4()
				sc, err := doc.labelScale(ctx, pl.Page, inc, o.canvas)
				if err != nil {
					return nil, &userError{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
				}
				pp.TabScales[i] = round2(sc)
			}
		}
		resp.Pages = append(resp.Pages, pp)
	}
	return checks, nil
}

func pdfJobName(file string, page, pages, i, n int) string {
	name := file
	if pages > 1 {
		name += fmt.Sprintf(" · p%d/%d", page, pages)
	}
	if n > 1 {
		name += fmt.Sprintf(" · label %d/%d", i+1, n)
	}
	return name
}

// handleCheck is the barcode check's long poll: it answers when every label
// is done or after 8 s, whichever is first, always 200 with done and the
// labels so far (the page reads done; api() doesn't expose the status). A
// token that's unknown, expired or cancelled is a 404.
func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	reply, ok := s.checks.poll(r.Context(), r.PathValue("token"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "that barcode check has expired"})
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// ---------------------------------------------------------------- print

type printResponse struct {
	Jobs     []jobInfo `json:"jobs"`
	Warnings []string  `json:"warnings,omitempty"`
	Error    string    `json:"error,omitempty"`
}

func (s *server) handlePrint(w http.ResponseWriter, r *http.Request) {
	up, err := s.readUpload(w, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	copies, err := parseCopies(up.Fields["copies"], s.cfg.MaxCopies)
	if err != nil {
		s.fail(w, err)
		return
	}
	media := up.Fields["media"]
	if media == "" {
		media = s.cfg.DefaultMedia
	}
	if !validMediaKeyword.MatchString(media) {
		s.fail(w, badRequest("%q isn't a media size name", media))
		return
	}

	reqs, info, err := s.buildJobs(r.Context(), up, copies, media, s.requestUser(r))
	if err != nil {
		s.fail(w, err)
		return
	}

	s.printMu.Lock()
	resp := printResponse{Jobs: []jobInfo{}}
	var sendErr error
	for _, pr := range reqs {
		job, warning, err := s.ipp.printJob(r.Context(), pr)
		if err != nil {
			sendErr = err
			break
		}
		resp.Jobs = append(resp.Jobs, job)
		if warning != "" {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("job %d: %s", job.ID, warning))
		}
	}
	s.printMu.Unlock()

	ids := make([]int, len(resp.Jobs))
	for i, j := range resp.Jobs {
		ids[i] = j.ID
	}
	notes := make([]string, 0, len(reqs))
	for _, pr := range reqs {
		if pr.Note != "" {
			notes = append(notes, pr.Note)
		}
	}
	attrs := []any{"kind", up.Kind, "bytes", len(up.Data), "copies", copies, "media", media,
		"labels", len(reqs), "methods", notes, "jobs", ids, "error", errString(sendErr)}
	if info != nil {
		attrs = append(attrs, "ppi", info.PPI, "ppi_source", info.PPISource)
	}
	s.log.Info("print", attrs...)

	if sendErr != nil {
		if len(resp.Jobs) > 0 {
			sendErr = fmt.Errorf("sent %d of %d jobs, then: %w", len(resp.Jobs), len(reqs), sendErr)
		}
		resp.Error = sendErr.Error()
		writeJSON(w, http.StatusBadGateway, resp)
		return
	}

	// LPrint accepts jobs even when the printer is offline or out of media and
	// just holds them, so surface the printer's own complaints alongside success.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if info, err := s.ipp.printerAttributes(ctx); err == nil {
		for _, reason := range info.Reasons {
			resp.Warnings = append(resp.Warnings, "printer reports "+reason)
		}
		if info.State == "stopped" {
			resp.Warnings = append(resp.Warnings, "printer is stopped: "+info.StateMessage)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildJobs turns an upload into the Print-Job requests to send, and for an
// image its imageInfo (for the log). Nothing is sent until every page has
// rendered, so a bad page can't leave half a job.
func (s *server) buildJobs(ctx context.Context, up *upload, copies int, media, user string) ([]printRequest, *imageInfo, error) {
	base := printRequest{User: user, Media: media, Copies: copies, JobName: up.Filename}
	switch up.Kind {
	case kindZPL:
		// LPrint's ZPL driver passes raw data straight to the printer and
		// ignores `copies`, so copies are made here: the same ZPL, repeated.
		// Media isn't sent either — the ZPL itself sets the label size.
		base.Format, base.Media, base.Copies = kindZPL.mimeType(), "", 1
		base.Data = repeatZPL(up.Data, copies)
		if copies > 1 {
			base.JobName = fmt.Sprintf("%s ×%d", up.Filename, copies)
		}
		return []printRequest{base}, nil, nil

	case kindPDF, kindPNG, kindJPEG, kindGIF:
		// An image is wrapped as a one-page pdf: every job is one of the
		// pipeline's canvas-sized PNG labels, never the upload itself.
		return s.pdfJobs(ctx, up, base, media)
	}
	return nil, nil, badRequest("unsupported file")
}

// tookTooLong turns any failure after a PDF request's deadline passed into
// one clear reply, whatever poppler or the code under it made of it.
func tookTooLong(ctx context.Context, err error, kind docKind) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg := "that pdf took too long to prepare — try fewer pages"
		if kind.isImage() {
			msg = "that image took too long to prepare"
		}
		return &userError{Status: http.StatusGatewayTimeout, Msg: msg}
	}
	return err
}

// pdfJobs is buildJobs for a PDF or an image: one job per label, within the
// PDF request deadline.
func (s *server) pdfJobs(ctx context.Context, up *upload, base printRequest, media string) (out []printRequest, info *imageInfo, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.pdf.deadline)
	defer cancel()
	defer func() { err = tookTooLong(ctx, err, up.Kind) }() // deferred after cancel, so it runs first

	release, err := s.pdf.acquire(ctx) // held for every poppler call, pdfinfo included, and an image's decode
	if err != nil {
		return nil, nil, err
	}
	defer release()
	m, mOK := s.mediaFor(media) // as labelOpts resolves it, so the ppi is the preview's
	doc, pages, info, err := s.openPDF(ctx, docSource{up: up, media: m, mediaOK: mOK})
	if err != nil {
		return nil, nil, err
	}
	defer doc.Close()
	o, err := s.labelOpts(up.Fields, doc, pages, media)
	if err != nil {
		return nil, nil, err
	}
	// One job per label: LPrint takes a single document per job
	// (multiple-document-jobs-supported = false). Skipped pages (no
	// label, beside pages with one) send nothing.
	plans, err := doc.plan(ctx, pages, o)
	if err != nil {
		return nil, nil, err
	}
	// The preview falls back to the whole page when detection fails; print
	// refuses instead, since what it would send (and skip) isn't what the
	// preview showed.
	for _, pl := range plans {
		if pl.Failed {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			msg := fmt.Sprintf("couldn't find the labels on page %d this time — try again, or choose whole page", pl.Page)
			if doc.noun.image {
				msg = "couldn't find the labels in the image this time — try again, or choose whole image"
			}
			return nil, nil, &userError{Status: http.StatusUnprocessableEntity, Msg: msg}
		}
	}
	for _, pl := range plans {
		if pl.Skip {
			continue
		}
		for i, b := range pl.Boxes {
			l, err := doc.label(ctx, pl.Page, b, pl.flip(i), o)
			if err != nil {
				return nil, nil, &userError{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
			}
			pr := base
			pr.Format, pr.Scaling, pr.Data, pr.Note = kindPNG.mimeType(), "fit", l.PNG, l.Method
			pr.JobName = pdfJobName(up.Filename, pl.Page, doc.Pages, i, len(pl.Boxes))
			out = append(out, pr)
		}
	}
	return out, info, nil
}

func repeatZPL(data []byte, n int) []byte {
	if n <= 1 {
		return data
	}
	unit := data
	if !bytes.HasSuffix(unit, []byte("\n")) {
		unit = append(append([]byte(nil), unit...), '\n')
	}
	return bytes.Repeat(unit, n)
}

func tooManyPages(n, limit int) error {
	return fieldError("pages", "that's %d pages; the limit is %d per print — pick a page range", n, limit)
}

func parseCopies(s string, maxCopies int) (int, error) {
	if s == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxCopies {
		return 0, badRequest("copies must be a whole number from 1 to %d", maxCopies)
	}
	return n, nil
}

// requestUser labels jobs in LPrint's list with the user your auth proxy
// names in USER_HEADER (e.g. Authelia's Remote-User). It's only a label, not
// an authorisation decision — but it's opt-in, since a client can set any
// header the proxy doesn't overwrite.
func (s *server) requestUser(r *http.Request) string {
	if s.cfg.UserHeader != "" {
		if u := cleanFilename(r.Header.Get(s.cfg.UserHeader)); u != "upload" {
			return truncateUTF8(u, 64)
		}
	}
	return "labelprint"
}

// ---------------------------------------------------------------- responses

// gatewayError marks a failure talking to LPrint (502), as opposed to a bad
// upload (4xx) or a local fault (500).
type gatewayError struct{ err error }

func (e *gatewayError) Error() string { return e.err.Error() }
func (e *gatewayError) Unwrap() error { return e.err }

func gateway(err error) error { return &gatewayError{err} }

func (s *server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := map[string]string{"error": err.Error()}
	var ue *userError
	var ge *gatewayError
	switch {
	case errors.As(err, &ue):
		status = ue.Status
		if ue.Field != "" {
			body["field"] = ue.Field
		}
	case errors.As(err, &ge):
		status = http.StatusBadGateway
	default:
		s.log.Error("request failed", "error", err)
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
