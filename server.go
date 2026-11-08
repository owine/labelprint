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
	log     *slog.Logger
	printMu sync.Mutex // one submission at a time, so multi-page jobs don't interleave
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
		v, err := io.ReadAll(io.LimitReader(part, 256))
		if isMaxBytes(err) {
			return nil, tooBig
		}
		up.Fields[part.FormName()] = strings.TrimSpace(string(v))
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
}

type previewResponse struct {
	Kind      docKind       `json:"kind"`
	PageCount int           `json:"pageCount,omitempty"` // pages in the pdf
	Selected  int           `json:"selected,omitempty"`  // pages the range picks
	Pages     []previewPage `json:"pages,omitempty"`     // first maxPreviewPages of those
	Labels    int           `json:"labels,omitempty"`    // labels in the zpl
	// zpl only: the canvas the preview was drawn on, and why there's no
	// picture if zebrash couldn't draw one. A failed preview never blocks
	// printing: the printer interprets the zpl itself.
	WidthIn      float64 `json:"widthIn,omitempty"`
	HeightIn     float64 `json:"heightIn,omitempty"`
	PreviewError string  `json:"previewError,omitempty"`
}

const maxPreviewPages = 12

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
		media, ok := mediaSize(up.Fields["media"])
		if !ok {
			media, _ = mediaSize(s.cfg.DefaultMedia)
		}
		p, err := s.zpl.render(r.Context(), up.Data, media, maxPreviewPages)
		if err != nil {
			s.log.Warn("zpl preview failed", "error", err)
			resp.PreviewError = err.Error()
			break
		}
		resp.Labels = max(resp.Labels, p.Labels)
		resp.WidthIn, resp.HeightIn = p.Size.WidthIn, p.Size.HeightIn
		resp.Pages = dataURLs(p.Pages)
	case kindPDF:
		doc, err := s.pdf.open(r.Context(), up.Data)
		if err != nil {
			s.fail(w, err)
			return
		}
		defer doc.Close()
		pages, err := parsePageRange(up.Fields["pages"], doc.Pages)
		if err != nil {
			s.fail(w, err)
			return
		}
		resp.PageCount, resp.Selected = doc.Pages, len(pages)
		rendered, err := doc.render(r.Context(), pages[:min(len(pages), maxPreviewPages)])
		if err != nil {
			s.fail(w, &userError{Status: http.StatusUnprocessableEntity, Msg: err.Error()})
			return
		}
		resp.Pages = dataURLs(rendered)
	}
	// PNG/JPEG: the browser already has the bytes and shows them itself.
	writeJSON(w, http.StatusOK, resp)
}

func dataURLs(pages []renderedPage) []previewPage {
	out := make([]previewPage, 0, len(pages))
	for _, p := range pages {
		out = append(out, previewPage{Page: p.Page, Image: "data:image/png;base64," + base64.StdEncoding.EncodeToString(p.PNG)})
	}
	return out
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

	reqs, err := s.buildJobs(r.Context(), up, copies, media, s.requestUser(r))
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
	s.log.Info("print", "kind", up.Kind, "bytes", len(up.Data), "copies", copies, "media", media,
		"jobs", ids, "error", errString(sendErr))

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

// buildJobs turns an upload into the Print-Job requests to send. Nothing is
// sent until every page has rendered, so a bad page can't leave half a job.
func (s *server) buildJobs(ctx context.Context, up *upload, copies int, media, user string) ([]printRequest, error) {
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
		return []printRequest{base}, nil

	case kindPNG, kindJPEG:
		base.Format, base.Scaling, base.Data = up.Kind.mimeType(), "fit", up.Data
		return []printRequest{base}, nil

	case kindPDF:
		doc, err := s.pdf.open(ctx, up.Data)
		if err != nil {
			return nil, err
		}
		defer doc.Close()
		pages, err := parsePageRange(up.Fields["pages"], doc.Pages)
		if err != nil {
			return nil, err
		}
		if len(pages) > s.cfg.MaxPages {
			return nil, badRequest("that's %d pages; the limit is %d per print — pick a page range", len(pages), s.cfg.MaxPages)
		}
		rendered, err := doc.render(ctx, pages)
		if err != nil {
			return nil, &userError{Status: http.StatusUnprocessableEntity, Msg: err.Error()}
		}
		// One job per page: LPrint takes a single document per job
		// (multiple-document-jobs-supported = false).
		out := make([]printRequest, 0, len(rendered))
		for _, p := range rendered {
			pr := base
			pr.Format, pr.Scaling, pr.Data = kindPNG.mimeType(), "fit", p.PNG
			if doc.Pages > 1 {
				pr.JobName = fmt.Sprintf("%s · p%d/%d", up.Filename, p.Page, doc.Pages)
			}
			out = append(out, pr)
		}
		return out, nil
	}
	return nil, badRequest("unsupported file")
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
	var ue *userError
	var ge *gatewayError
	switch {
	case errors.As(err, &ue):
		status = ue.Status
	case errors.As(err, &ge):
		status = http.StatusBadGateway
	default:
		s.log.Error("request failed", "error", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
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
