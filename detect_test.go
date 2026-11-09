package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A short UPS-style EPL2 label with made-up content.
const epl2Sample = `
N
q812
Q1218,24
S4
D8
ZT
A50,50,0,3,1,1,N,"SAMPLE SENDER"
A50,100,0,3,1,1,N,"1 EXAMPLE ST"
B50,400,0,1,2,6,100,B,"1Z000000000000000"
P1
`

func TestDetectKind(t *testing.T) {
	cases := []struct {
		name, data string
		want       docKind
		status     int // expected userError status when want == ""
	}{
		{"label.zpl", "^XA^FO50,50^FDhi^FS^XZ", kindZPL, 0},
		{"label.zpl", "~JC", kindZPL, 0}, // command-only .zpl is still ZPL
		{"label.txt", "^xa^fdlower^fs^xz", kindZPL, 0},
		{"anything.dat", "junk\n^XA^XZ\n", kindZPL, 0},
		{"notes.txt", "just some text", "", 415},
		{"notes.txt", "^XA but never closed", "", 415},
		{"x.pdf", "%PDF-1.7\n...", kindPDF, 0},
		{"no-extension", "\r\n%PDF-1.4 junk before header", kindPDF, 0},
		{"x.png", "\x89PNG\r\n\x1a\nrest", kindPNG, 0},
		{"x.jpg", "\xff\xd8\xff\xe0rest", kindJPEG, 0},
		{"x.gif", "GIF89a....", "", 415},
		{"empty.zpl", "", "", 400},
		// EPL2 is refused, whatever it is called; ZPL and plain text are not caught by it.
		{"label.epl", epl2Sample, "", 415},
		{"label.txt", epl2Sample, "", 415},
		{"label.zpl", epl2Sample, "", 415},
		{"label.txt", "\r\n\r\n" + strings.ReplaceAll(epl2Sample, "\n", "\r\n"), "", 415},
		{"label.txt", "N\nq812\nP1\n", "", 415},
		{"label.txt", "N  \nZT\n", "", 415},
		{"label.zpl", "N\n^XA^FDhi^FS^XZ", kindZPL, 0}, // ZPL label wins
		{"label.zpl", "N\n~JC", kindZPL, 0},
		{"notes.txt", "N", "", 415},
		{"notes.txt", "N\nPlease call me\nAbout the parcel", "", 415},
		{"data.csv", "N,name,city\nA1,bob,x\n", "", 415},
		{"notes.txt", "Name\nq812\n", "", 415},
		{"label.zpl", "Name\nq812\n", kindZPL, 0},
		// setup lines before N
		{"a.zpl", "I8,A,001\nOD\nJF\nZB\nq812\nQ1218,24\nN\nA50,50,0,3,1,1,N,\"x\"\nP1\n", "", 415},
		{"a.zpl", "\r\nOD\r\nN\r\nP1\r\n", "", 415},
		{"a.zpl", "OD\nN\nP1", "", 415},
		{"a.zpl", "line one\nline two\nN\nA1,2\n", kindZPL, 0},
		{"a.zpl", "O\nN\nrow,1\n", kindZPL, 0},
		{"a.zpl", "O\nN\n", kindZPL, 0},
		{"a.zpl", "OD\nN\n", kindZPL, 0},
		{"a.zpl", "Hello\nOD\nN\nP1\n", kindZPL, 0},
		{"x.pdf", "N\nq812\n%PDF-1.4", kindPDF, 0},
	}
	for _, c := range cases {
		got, err := detectKind(c.name, []byte(c.data))
		if c.want != "" {
			if err != nil || got != c.want {
				t.Errorf("%s %q: got %q, %v; want %q", c.name, c.data, got, err, c.want)
			}
			continue
		}
		var ue *userError
		if !errors.As(err, &ue) || ue.Status != c.status {
			t.Errorf("%s %q: got %q, %v; want status %d", c.name, c.data, got, err, c.status)
		}
	}
}

func TestCountZPLLabels(t *testing.T) {
	if n := countZPLLabels([]byte("^XA^XZ\n^xa^xz^XA^FD^XZ")); n != 3 {
		t.Fatalf("got %d", n)
	}
}

func TestRepeatZPL(t *testing.T) {
	if got := string(repeatZPL([]byte("^XA^XZ"), 3)); got != "^XA^XZ\n^XA^XZ\n^XA^XZ\n" {
		t.Fatalf("got %q", got)
	}
	in := []byte("^XA^XZ\n")
	if got := string(repeatZPL(in, 2)); got != "^XA^XZ\n^XA^XZ\n" {
		t.Fatalf("got %q", got)
	}
	if got := repeatZPL(in, 1); &got[0] != &in[0] {
		t.Fatal("one copy should be sent unchanged")
	}
}

func TestParsePageRange(t *testing.T) {
	ok := map[string][]int{
		"":          {1, 2, 3, 4, 5},
		"all":       {1, 2, 3, 4, 5},
		"2":         {2},
		"1-3":       {1, 2, 3},
		"4-":        {4, 5},
		"-2":        {1, 2},
		"5, 1-2, 2": {1, 2, 5},
		" 3 ,, 1 ":  {1, 3},
		"1-1":       {1},
		"-":         {1, 2, 3, 4, 5},
		"2-3,3-4,1": {1, 2, 3, 4},
	}
	for spec, want := range ok {
		got, err := parsePageRange(spec, 5)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: got %v, %v; want %v", spec, got, err, want)
		}
	}
	for _, spec := range []string{"0", "6", "3-2", "a", "1-b", "1-9", ",", "1.5"} {
		var ue *userError
		if _, err := parsePageRange(spec, 5); !errors.As(err, &ue) || ue.Status != 400 {
			t.Errorf("%q: want a 400 userError, got %v", spec, err)
		}
	}
}

func TestDetectEPL2Message(t *testing.T) {
	for _, name := range []string{"a.epl", "a.txt", "a.zpl", "a"} {
		_, err := detectKind(name, []byte(epl2Sample))
		if !errors.Is(err, errEPL2) || !strings.Contains(err.Error(), "choose ZPL or PDF") {
			t.Errorf("%s: got %v, want the EPL2 error", name, err)
		}
	}
	if _, err := detectKind("a.txt", []byte("N\nhello")); errors.Is(err, errEPL2) {
		t.Error("plain text starting with N is not EPL2")
	}
}

func TestDetectEPL2SetupFirst(t *testing.T) {
	for _, d := range []string{
		"I8,A,001\nOD\nJF\nZB\nq812\nQ1218,24\nN\nA50,50,0,3,1,1,N,\"x\"\nP1\n",
		"\r\nOD\r\nN\r\nP1\r\n",
		"OD\nN\nP1",
	} {
		if _, err := detectKind("a.zpl", []byte(d)); !errors.Is(err, errEPL2) {
			t.Errorf("%q: got %v, want the EPL2 error", d, err)
		}
	}
}

// What an upload's kind is served or sent as, and which kinds go through the
// label pipeline as images.
func TestImageKinds(t *testing.T) {
	for _, c := range []struct {
		kind  docKind
		mime  string
		image bool
	}{
		{kindZPL, "application/vnd.zebra-zpl", false},
		{kindPDF, "", false},
		{kindPNG, "image/png", true},
		{kindJPEG, "image/jpeg", true},
		{kindGIF, "image/gif", true},
	} {
		if got := c.kind.mimeType(); got != c.mime {
			t.Errorf("%s: mime type %q, want %q", c.kind, got, c.mime)
		}
		if got := c.kind.isImage(); got != c.image {
			t.Errorf("%s: isImage %v", c.kind, got)
		}
	}
}
