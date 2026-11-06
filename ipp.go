package main

// A minimal IPP/2.0 client (RFC 8010 encoding, RFC 8011 semantics): just enough
// to send Print-Job, Get-Printer-Attributes and Get-Jobs to LPrint.
//
// The wire format is a fixed 8-byte header followed by attribute groups. Each
// group opens with a one-byte delimiter tag, and each attribute is a TLV:
//
//	value-tag(1) name-length(2) name value-length(2) value
//
// A TLV with an empty name is an additional value of the previous attribute
// (that's how 1setOf is encoded). The document data, if any, follows the
// end-of-attributes tag in the same HTTP POST body.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Operation ids.
const (
	opPrintJob             uint16 = 0x0002
	opGetJobs              uint16 = 0x000A
	opGetPrinterAttributes uint16 = 0x000B
)

// Delimiter tags (group starts).
const (
	tagOperationGroup   byte = 0x01
	tagJobGroup         byte = 0x02
	tagEndOfAttributes  byte = 0x03
	tagPrinterGroup     byte = 0x04
	tagUnsupportedGroup byte = 0x05
)

// Value tags.
const (
	tagInteger    byte = 0x21
	tagBoolean    byte = 0x22
	tagEnum       byte = 0x23
	tagDateTime   byte = 0x31
	tagText       byte = 0x41
	tagName       byte = 0x42
	tagKeyword    byte = 0x44
	tagURI        byte = 0x45
	tagCharset    byte = 0x47
	tagLanguage   byte = 0x48
	tagMimeType   byte = 0x49
	tagExtension  byte = 0x7F
	maxIPPValue        = 0xFFFF
	maxResponseSz      = 4 << 20
)

// ---------------------------------------------------------------- encoding

type ippEncoder struct {
	buf bytes.Buffer
	err error
}

// newIPPRequest writes the header and the operation-attributes every request
// needs (RFC 8011 §4.1.4–4.1.5), in the order the RFC requires.
func newIPPRequest(op uint16, requestID uint32, printerURI, user string) *ippEncoder {
	e := &ippEncoder{}
	e.buf.Write([]byte{2, 0}) // version 2.0
	_ = binary.Write(&e.buf, binary.BigEndian, op)
	_ = binary.Write(&e.buf, binary.BigEndian, requestID)
	e.group(tagOperationGroup)
	e.str(tagCharset, "attributes-charset", "utf-8")
	e.str(tagLanguage, "attributes-natural-language", "en")
	e.str(tagURI, "printer-uri", printerURI)
	if user != "" {
		e.str(tagName, "requesting-user-name", truncateUTF8(user, 255))
	}
	return e
}

func (e *ippEncoder) group(tag byte) { e.buf.WriteByte(tag) }

func (e *ippEncoder) raw(tag byte, name string, value []byte) {
	if len(name) > maxIPPValue || len(value) > maxIPPValue {
		e.err = fmt.Errorf("ipp: attribute %q too long", name)
		return
	}
	e.buf.WriteByte(tag)
	_ = binary.Write(&e.buf, binary.BigEndian, uint16(len(name)))
	e.buf.WriteString(name)
	_ = binary.Write(&e.buf, binary.BigEndian, uint16(len(value)))
	e.buf.Write(value)
}

func (e *ippEncoder) str(tag byte, name, value string) { e.raw(tag, name, []byte(value)) }

// strs writes a 1setOf: the first value carries the name, the rest have none.
func (e *ippEncoder) strs(tag byte, name string, values ...string) {
	for i, v := range values {
		if i > 0 {
			name = ""
		}
		e.str(tag, name, v)
	}
}

func (e *ippEncoder) integer(tag byte, name string, v int32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	e.raw(tag, name, b)
}

func (e *ippEncoder) bytes() ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	out := append([]byte(nil), e.buf.Bytes()...)
	return append(out, tagEndOfAttributes), nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---------------------------------------------------------------- decoding

type ippAttr struct {
	Name   string
	Tag    byte
	Values [][]byte
}

type ippGroup struct {
	Tag   byte
	Attrs []*ippAttr
}

type ippMessage struct {
	Version   [2]byte
	Code      uint16 // operation id in a request, status code in a response
	RequestID uint32
	Groups    []*ippGroup
	Data      []byte // anything after end-of-attributes (the document, in a request)
}

var errShortIPP = errors.New("ipp: message truncated")

// parseIPP decodes a request or a response. Collections (begCollection …
// endCollection) aren't interpreted: their member TLVs all have empty names,
// so they fold into the collection attribute's value list and are ignored.
func parseIPP(b []byte) (*ippMessage, error) {
	if len(b) < 9 {
		return nil, errShortIPP
	}
	m := &ippMessage{
		Version:   [2]byte{b[0], b[1]},
		Code:      binary.BigEndian.Uint16(b[2:4]),
		RequestID: binary.BigEndian.Uint32(b[4:8]),
	}
	p := 8
	var cur *ippGroup
	var last *ippAttr
	for {
		if p >= len(b) {
			return nil, errShortIPP
		}
		tag := b[p]
		p++
		if tag == tagEndOfAttributes {
			m.Data = b[p:]
			return m, nil
		}
		if tag < 0x10 { // delimiter: a new group
			cur = &ippGroup{Tag: tag}
			m.Groups = append(m.Groups, cur)
			last = nil
			continue
		}
		if tag == tagExtension { // 4-byte extended tag follows; never used by LPrint
			if p+4 > len(b) {
				return nil, errShortIPP
			}
			p += 4
		}
		if cur == nil {
			return nil, errors.New("ipp: attribute outside a group")
		}
		if p+2 > len(b) {
			return nil, errShortIPP
		}
		nl := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+nl+2 > len(b) {
			return nil, errShortIPP
		}
		name := string(b[p : p+nl])
		p += nl
		vl := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+vl > len(b) {
			return nil, errShortIPP
		}
		val := b[p : p+vl]
		p += vl

		if name == "" {
			if last == nil {
				return nil, errors.New("ipp: additional value without an attribute")
			}
			last.Values = append(last.Values, val)
			continue
		}
		last = &ippAttr{Name: name, Tag: tag, Values: [][]byte{val}}
		cur.Attrs = append(cur.Attrs, last)
	}
}

func (g *ippGroup) get(name string) *ippAttr {
	if g == nil {
		return nil
	}
	for _, a := range g.Attrs {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func (m *ippMessage) group(tag byte) *ippGroup {
	for _, g := range m.Groups {
		if g.Tag == tag {
			return g
		}
	}
	return nil
}

func (m *ippMessage) groups(tag byte) []*ippGroup {
	var out []*ippGroup
	for _, g := range m.Groups {
		if g.Tag == tag {
			out = append(out, g)
		}
	}
	return out
}

func (a *ippAttr) str() string {
	if a == nil || len(a.Values) == 0 {
		return ""
	}
	return string(a.Values[0])
}

func (a *ippAttr) strs() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.Values))
	for _, v := range a.Values {
		out = append(out, string(v))
	}
	return out
}

func (a *ippAttr) int() int {
	if a == nil || len(a.Values) == 0 || len(a.Values[0]) != 4 {
		return 0
	}
	return int(int32(binary.BigEndian.Uint32(a.Values[0])))
}

// time decodes an RFC 2579 DateAndTime (11 octets) to a UTC time.
func (a *ippAttr) time() time.Time {
	if a == nil || len(a.Values) == 0 || len(a.Values[0]) != 11 {
		return time.Time{}
	}
	v := a.Values[0]
	t := time.Date(int(binary.BigEndian.Uint16(v[0:2])), time.Month(v[2]), int(v[3]),
		int(v[4]), int(v[5]), int(v[6]), int(v[7])*100_000_000, time.UTC)
	off := time.Duration(v[9])*time.Hour + time.Duration(v[10])*time.Minute
	switch v[8] {
	case '+':
		t = t.Add(-off)
	case '-':
		t = t.Add(off)
	}
	return t
}

// ---------------------------------------------------------------- status codes

// ippStatusNames covers the codes LPrint/PAPPL actually return.
var ippStatusNames = map[uint16]string{
	0x0000: "successful-ok",
	0x0001: "successful-ok-ignored-or-substituted-attributes",
	0x0002: "successful-ok-conflicting-attributes",
	0x0400: "client-error-bad-request",
	0x0401: "client-error-forbidden",
	0x0402: "client-error-not-authenticated",
	0x0403: "client-error-not-authorized",
	0x0404: "client-error-not-possible",
	0x0405: "client-error-timeout",
	0x0406: "client-error-not-found",
	0x0407: "client-error-gone",
	0x0408: "client-error-request-entity-too-large",
	0x040A: "client-error-document-format-not-supported",
	0x040B: "client-error-attributes-or-values-not-supported",
	0x040C: "client-error-uri-scheme-not-supported",
	0x040E: "client-error-conflicting-attributes",
	0x0500: "server-error-internal-error",
	0x0501: "server-error-operation-not-supported",
	0x0502: "server-error-service-unavailable",
	0x0503: "server-error-version-not-supported",
	0x0504: "server-error-device-error",
	0x0505: "server-error-temporary-error",
	0x0506: "server-error-not-accepting-jobs",
	0x0507: "server-error-busy",
	0x0508: "server-error-job-canceled",
}

func ippStatusName(code uint16) string {
	if n, ok := ippStatusNames[code]; ok {
		return n
	}
	return fmt.Sprintf("0x%04x", code)
}

// ippError is an IPP-level failure reported by LPrint (as opposed to a
// transport failure reaching it).
type ippError struct {
	Status  uint16
	Message string // LPrint's status-message, if it sent one
}

func (e *ippError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("lprint said %s: %s", ippStatusName(e.Status), e.Message)
	}
	return "lprint said " + ippStatusName(e.Status)
}

// ---------------------------------------------------------------- client

type ippClient struct {
	printerURI string // as configured: ipp://host:port/path
	endpoint   string // the http(s):// URL the request is POSTed to
	host       string // host:port, for error messages
	hc         *http.Client
	nextID     atomic.Uint32
}

func newIPPClient(printerURI string) (*ippClient, error) {
	u, err := url.Parse(printerURI)
	if err != nil {
		return nil, fmt.Errorf("LPRINT_URI: %w", err)
	}
	httpURL := *u
	switch u.Scheme {
	case "ipp", "http":
		httpURL.Scheme = "http"
	case "ipps", "https":
		httpURL.Scheme = "https"
	default:
		return nil, fmt.Errorf("LPRINT_URI: scheme must be ipp:// or ipps://, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("LPRINT_URI: missing host")
	}
	if u.Port() == "" { // IPP's registered port
		httpURL.Host = net.JoinHostPort(u.Hostname(), "631")
	}
	c := &ippClient{
		printerURI: printerURI,
		endpoint:   httpURL.String(),
		host:       httpURL.Host,
		hc:         &http.Client{Timeout: 90 * time.Second},
	}
	return c, nil
}

// do POSTs one IPP request (plus optional document) and returns the decoded
// response. Transport failures and IPP error statuses both come back as errors.
func (c *ippClient) do(ctx context.Context, req *ippEncoder, doc []byte) (*ippMessage, error) {
	head, err := req.bytes()
	if err != nil {
		return nil, err
	}
	body := io.MultiReader(bytes.NewReader(head), bytes.NewReader(doc))
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, body)
	if err != nil {
		return nil, err
	}
	hreq.ContentLength = int64(len(head) + len(doc))
	hreq.Header.Set("Content-Type", "application/ipp")

	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("can't reach lprint at %s: %w", c.host, unwrapNetErr(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSz))
	if err != nil {
		return nil, fmt.Errorf("reading lprint's reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lprint at %s answered HTTP %s — check the queue path in LPRINT_URI", c.host, resp.Status)
	}
	m, err := parseIPP(data)
	if err != nil {
		return nil, fmt.Errorf("lprint's reply: %w", err)
	}
	if m.Code >= 0x0100 {
		return m, &ippError{Status: m.Code, Message: m.group(tagOperationGroup).get("status-message").str()}
	}
	return m, nil
}

// unwrapNetErr drops the "Post \"http://…\":" prefix url.Error adds.
func unwrapNetErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *ippClient) id() uint32 { return c.nextID.Add(1) }

// ---------------------------------------------------------------- operations

type printerInfo struct {
	State          string // idle | processing | stopped
	StateMessage   string
	Reasons        []string
	MediaSupported []string
	MediaReady     []string
	MediaDefault   string
	Formats        []string
}

var printerStates = map[int]string{3: "idle", 4: "processing", 5: "stopped"}

func (c *ippClient) printerAttributes(ctx context.Context) (*printerInfo, error) {
	req := newIPPRequest(opGetPrinterAttributes, c.id(), c.printerURI, "")
	req.strs(tagKeyword, "requested-attributes",
		"printer-state", "printer-state-message", "printer-state-reasons",
		"media-supported", "media-ready", "media-default", "document-format-supported")
	m, err := c.do(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	g := m.group(tagPrinterGroup)
	if g == nil {
		return nil, errors.New("lprint returned no printer attributes")
	}
	state := printerStates[g.get("printer-state").int()]
	if state == "" {
		state = "unknown"
	}
	return &printerInfo{
		State:          state,
		StateMessage:   g.get("printer-state-message").str(),
		Reasons:        dropNone(g.get("printer-state-reasons").strs()),
		MediaSupported: g.get("media-supported").strs(),
		MediaReady:     g.get("media-ready").strs(),
		MediaDefault:   g.get("media-default").str(),
		Formats:        g.get("document-format-supported").strs(),
	}, nil
}

type printRequest struct {
	JobName string
	User    string
	Format  string // document-format MIME type
	Media   string // media keyword; empty = printer default
	Copies  int
	Scaling string // print-scaling keyword; empty = printer default
	Data    []byte
}

type jobInfo struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	User        string    `json:"user,omitempty"`
	State       string    `json:"state"`
	Reasons     []string  `json:"reasons,omitempty"`
	Message     string    `json:"message,omitempty"`
	Created     time.Time `json:"created,omitzero"`
	Completed   time.Time `json:"completed,omitzero"`
	Impressions int       `json:"impressions"`
}

var jobStates = map[int]string{
	3: "pending", 4: "held", 5: "processing", 6: "stopped",
	7: "canceled", 8: "aborted", 9: "completed",
}

func jobFromGroup(g *ippGroup) jobInfo {
	state := jobStates[g.get("job-state").int()]
	if state == "" {
		state = "unknown"
	}
	return jobInfo{
		ID:          g.get("job-id").int(),
		Name:        g.get("job-name").str(),
		User:        g.get("job-originating-user-name").str(),
		State:       state,
		Reasons:     dropNone(g.get("job-state-reasons").strs()),
		Message:     g.get("job-state-message").str(),
		Created:     g.get("date-time-at-creation").time(),
		Completed:   g.get("date-time-at-completed").time(),
		Impressions: g.get("job-impressions-completed").int(),
	}
}

// printJob sends one Print-Job. warning is non-empty when LPrint accepted the
// job but ignored or substituted some attribute (status 0x0001/0x0002).
func (c *ippClient) printJob(ctx context.Context, p printRequest) (job jobInfo, warning string, err error) {
	req := newIPPRequest(opPrintJob, c.id(), c.printerURI, p.User)
	req.str(tagName, "job-name", truncateUTF8(p.JobName, 255))
	req.str(tagMimeType, "document-format", p.Format)
	req.group(tagJobGroup)
	if p.Copies > 1 {
		req.integer(tagInteger, "copies", int32(p.Copies))
	}
	if p.Media != "" {
		req.str(tagKeyword, "media", p.Media)
	}
	if p.Scaling != "" {
		req.str(tagKeyword, "print-scaling", p.Scaling)
	}
	m, err := c.do(ctx, req, p.Data)
	if err != nil {
		return jobInfo{}, "", err
	}
	job = jobFromGroup(m.group(tagJobGroup))
	job.Name = p.JobName
	if m.Code != 0 {
		warning = ippStatusName(m.Code)
		if u := m.group(tagUnsupportedGroup); u != nil {
			var names []string
			for _, a := range u.Attrs {
				names = append(names, a.Name)
			}
			warning += " (" + strings.Join(names, ", ") + ")"
		}
	}
	return job, warning, nil
}

func (c *ippClient) jobs(ctx context.Context, limit int) ([]jobInfo, error) {
	req := newIPPRequest(opGetJobs, c.id(), c.printerURI, "")
	req.str(tagKeyword, "which-jobs", "all")
	req.integer(tagInteger, "limit", int32(limit))
	req.strs(tagKeyword, "requested-attributes",
		"job-id", "job-name", "job-originating-user-name", "job-state", "job-state-reasons",
		"job-state-message", "date-time-at-creation", "date-time-at-completed",
		"job-impressions-completed")
	m, err := c.do(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	var out []jobInfo
	for _, g := range m.groups(tagJobGroup) {
		out = append(out, jobFromGroup(g))
	}
	return out, nil
}

func dropNone(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		if s != "none" && s != "" {
			out = append(out, s)
		}
	}
	return out
}
