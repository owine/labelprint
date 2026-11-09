package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeLabelPDF builds a small 4×6 in PDF with n pages. Each page has a border,
// barcode-ish bars and a line of Helvetica that is deliberately NOT embedded,
// as in many carrier label PDFs, so rendering it exercises font substitution.
func makeLabelPDF(n int) []byte {
	var objs []string
	// 1 catalog, 2 pages, 3 font, then (page, content) pairs.
	kids := ""
	for i := range n {
		kids += fmt.Sprintf("%d 0 R ", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
	for i := range n {
		var c bytes.Buffer
		c.WriteString("4 w 12 12 264 408 re S\n") // border
		for b := range 24 {                       // bars of varying width
			fmt.Fprintf(&c, "%d 60 %d 90 re f\n", 36+b*9, 1+b%4)
		}
		fmt.Fprintf(&c, "BT /F1 28 Tf 36 360 Td (labelprint page %d) Tj ET\n", i+1)
		stream := c.String()
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 288 432] "+
				"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		)
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// The CI image smoke test posts these files; keep them in sync with their
// generators. Regenerate with UPDATE_FIXTURES=1 go test -run Fixture.
func TestFixtureUpToDate(t *testing.T) {
	for path, gen := range map[string]func() []byte{
		"testdata/label-2page.pdf":  func() []byte { return makeLabelPDF(2) },
		"testdata/ups-2up.pdf":      func() []byte { return fixUPS2up(true, 200) },
		"testdata/label-200ppi.png": func() []byte { return label200ppi(t) },
	} {
		want := gen()
		if os.Getenv("UPDATE_FIXTURES") == "1" {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run UPDATE_FIXTURES=1 go test -run Fixture", path)
		}
	}
}

// FIXTURE_DIR=/some/dir go test -run WriteFixtures writes every fixture there,
// the synthetic images included, for trying the page by hand. They're
// synthetic: no real labels.
func TestWriteFixtures(t *testing.T) {
	dir := os.Getenv("FIXTURE_DIR")
	if dir == "" {
		t.Skip("set FIXTURE_DIR")
	}
	for name, pdf := range map[string][]byte{
		"01-label-beside-text.pdf": fixLabelBesideText(), "02-ups-2up.pdf": fixUPS2up(true, 200),
		"03-hairline.pdf": fixHairline(), "04-2up.pdf": fix2up(), "05-wide.pdf": fixWide(),
		"06-text-only.pdf": fixTextOnly(), "07-stamped.pdf": fixStamped(),
		"08-return-blank-tab.pdf": fixReturn(0, 206, 196), "09-clipped-tab.pdf": fixClippedTab(),
		"10-ups-300ppi.pdf": fixUPS2up(false, 300), "11-return-tab.pdf": fixReturn(1340, 206, 196),
		"12-return-tab-193.pdf": fixReturn(1395, 193, 193), "13-band.pdf": fixBand(),
		"14-label-then-text.pdf": fixLabelThenText(), "15-small-label.pdf": fixSmallLabel(),
		"16-framed-tab-first.pdf": fixFramedTabFirst(), "17-label-then-one-line.pdf": fixLabelThenOneLine(),
		"18-label-200ppi.png": label200ppi(t), "19-ups-doc-tab.png": upsTabPNG(t),
		"20-screenshot.jpg": screenshotJPEG(t), "21-label.gif": labelGIF(t),
		"22-guessed-size.png": encodePNG(t, labelBitmap(300, 300).gray()),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pdf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------- response builder for fakes

type ippResponseBuilder struct{ ippEncoder }

func newIPPResponse(status uint16, requestID uint32) *ippResponseBuilder {
	b := &ippResponseBuilder{}
	b.buf.Write([]byte{2, 0})
	_ = binary.Write(&b.buf, binary.BigEndian, status)
	_ = binary.Write(&b.buf, binary.BigEndian, requestID)
	b.group(tagOperationGroup)
	b.str(tagCharset, "attributes-charset", "utf-8")
	b.str(tagLanguage, "attributes-natural-language", "en")
	return b
}

func (b *ippResponseBuilder) mustBytes(t testing.TB) []byte {
	t.Helper()
	out, err := b.bytes()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var discardLog = slog.New(slog.DiscardHandler)

// shimTool puts a shell script named tool first on PATH for the rest of the
// test. The script can run the real tool as "$REAL" "$@".
func shimTool(t *testing.T, tool, script string) {
	t.Helper()
	real, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("%s not installed", tool)
	}
	dir := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\nREAL='%s'\n%s\n", real, script)
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
