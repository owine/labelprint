package main

import (
	"slices"
	"testing"
	"time"
)

func TestRequestRoundTrip(t *testing.T) {
	req := newIPPRequest(opPrintJob, 42, "ipp://h:8000/ipp/print/q", "alice")
	req.str(tagName, "job-name", "label.pdf")
	req.strs(tagKeyword, "requested-attributes", "a", "b", "c")
	req.group(tagJobGroup)
	req.integer(tagInteger, "copies", 3)
	head, err := req.bytes()
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte("^XA^XZ")
	m, err := parseIPP(append(head, doc...))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != [2]byte{2, 0} || m.Code != opPrintJob || m.RequestID != 42 {
		t.Fatalf("header = %v %x %d", m.Version, m.Code, m.RequestID)
	}
	op := m.group(tagOperationGroup)
	// RFC 8011 §4.1.4: charset and language must lead, in that order.
	if op.Attrs[0].Name != "attributes-charset" || op.Attrs[1].Name != "attributes-natural-language" {
		t.Errorf("first attributes = %s, %s", op.Attrs[0].Name, op.Attrs[1].Name)
	}
	if got := op.get("requesting-user-name").str(); got != "alice" {
		t.Errorf("user = %q", got)
	}
	if got := op.get("requested-attributes").strs(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("1setOf = %v", got)
	}
	if got := m.group(tagJobGroup).get("copies").int(); got != 3 {
		t.Errorf("copies = %d", got)
	}
	if string(m.Data) != string(doc) {
		t.Errorf("document = %q", m.Data)
	}
}

func TestParseTruncatedNeverPanics(t *testing.T) {
	req := newIPPRequest(opGetJobs, 1, "ipp://h/q", "u")
	req.strs(tagKeyword, "requested-attributes", "job-id", "job-name")
	full, _ := req.bytes()
	for n := range len(full) {
		if _, err := parseIPP(full[:n]); err == nil {
			t.Fatalf("prefix of %d/%d bytes parsed without error", n, len(full))
		}
	}
	if _, err := parseIPP(full); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejectsOrphanValue(t *testing.T) {
	// header, operation group, then a value with an empty name and no attribute before it
	b := []byte{2, 0, 0, 0, 0, 0, 0, 1, tagOperationGroup, tagKeyword, 0, 0, 0, 1, 'x', tagEndOfAttributes}
	if _, err := parseIPP(b); err == nil {
		t.Fatal("want error")
	}
}

func TestDateTime(t *testing.T) {
	// 2026-11-06 09:49:28.5 -05:00 == 14:49:28.5Z
	a := &ippAttr{Values: [][]byte{{0x07, 0xEA, 11, 6, 9, 49, 28, 5, '-', 5, 0}}}
	want := time.Date(2026, 11, 6, 14, 49, 28, 500_000_000, time.UTC)
	if got := a.time(); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "naïve" // ï is 2 bytes at offset 2
	if got := truncateUTF8(s, 3); got != "na" {
		t.Fatalf("got %q", got)
	}
	if got := truncateUTF8(s, 10); got != s {
		t.Fatalf("got %q", got)
	}
}

func TestClientEndpoint(t *testing.T) {
	cases := map[string]string{
		"ipp://192.0.2.10:8000/ipp/print/Zebra-GX420t": "http://192.0.2.10:8000/ipp/print/Zebra-GX420t",
		"ipp://printer.lan/ipp/print/x":                "http://printer.lan:631/ipp/print/x",
		"ipps://printer.lan:443/ipp/print/x":           "https://printer.lan:443/ipp/print/x",
	}
	for in, want := range cases {
		c, err := newIPPClient(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if c.endpoint != want {
			t.Errorf("%s → %s, want %s", in, c.endpoint, want)
		}
		if c.printerURI != in {
			t.Errorf("printer-uri must be sent as configured, got %s", c.printerURI)
		}
	}
	for _, bad := range []string{"socket://192.0.2.20:9100", "ipp:///nohost", "::"} {
		if _, err := newIPPClient(bad); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestIPPErrorMessage(t *testing.T) {
	e := &ippError{Status: 0x040A, Message: "Unsupported format 'application/pdf'."}
	want := "lprint said client-error-document-format-not-supported: Unsupported format 'application/pdf'."
	if e.Error() != want {
		t.Fatalf("got %q", e.Error())
	}
}
