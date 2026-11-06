package main

import (
	"errors"
	"slices"
	"testing"
)

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
