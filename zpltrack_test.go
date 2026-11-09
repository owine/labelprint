package main

import (
	"slices"
	"testing"
)

// fakeUPSDigits is a made-up 1Z number ending in an even run of digits, so
// its ZPL can switch to subset C with >5 as carrier ZPL does.
const fakeUPSDigits = "1ZA1B2C34567890123"

func TestZPLCode128Data(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{">:1ZA1B2C3>54567890123", fakeUPSDigits},
		{">;>8420" + fakeZIP5 + ">8" + fakeIMpb22, "420" + fakeZIP5 + gs + fakeIMpb22},
		{">;>8>8420", gs + "420"}, // only the first FNC1 is the GS1 marker
		{"AB>>CD", "AB>CD"},
		{">9AB>6cd>7EF", "ABcdEF"},
		{">0>1>2>3>4>=><x", "x"},
		{"A>", "A"},
		{"", ""},
	} {
		if got := zplCode128Data(c.in); got != c.want {
			t.Errorf("zplCode128Data(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestZPLTracking(t *testing.T) {
	ups := "^XA^FO50,50^BY3^BCN,200,Y,N,N^FD>:1ZA1B2C3>54567890123^FS^XZ"
	usps := "^XA^FO50,50^BCN,100,N,N,N^FD>;>8420" + fakeZIP5 + "^FS" + // routing alone: not tracking
		"^FO50,300^BCN,200,Y,N,N^FD>;>8420" + fakeZIP5 + ">8" + fakeIMpb22 + "^FS" +
		"^FO50,600^BCN,80,Y,N,N^FD>;123456789012^FS^XZ" // a reference number
	for _, c := range []struct {
		name string
		zpl  string
		want []zplTrackingEntry
	}{
		{"ups, >: and >5", ups, []zplTrackingEntry{{"UPS", fakeUPSDigits}}},
		// the leading >8 is dropped, the later one splits ZIP from tracking
		{"usps impb among several fields", usps, []zplTrackingEntry{{"USPS", fakeIMpb22}}},
		{"code 39", "^XA^FO50,50^B3N,N,100,Y,N^FD" + fakeUPS + "^FS^XZ", []zplTrackingEntry{{"UPS", fakeUPS}}},
		{"lowercase commands", "^xa^fo50,50^bcn,200^fd>:" + fakeUPS + "^fs^xz", []zplTrackingEntry{{"UPS", fakeUPS}}},
		{"two labels", ups + usps, []zplTrackingEntry{{"UPS", fakeUPSDigits}, {"USPS", fakeIMpb22}}},
		{"copies listed once", ups + ups, []zplTrackingEntry{{"UPS", fakeUPSDigits}}},
		{"^FH field skipped", "^XA^FO50,50^BCN,200^FH^FD>:" + fakeUPS + "^FS^XZ", nil},
		{"^FN field skipped", "^XA^FO50,50^BCN,200^FN1^FD>:" + fakeUPS + "^FS^XZ", nil},
		{"^DF template skipped", "^XA^DFR:SHIP.ZPL^FS^FO50,50^BCN,200^FD>:" + fakeUPS + "^FS^XZ", nil},
		{"text field", "^XA^FO50,50^A0N,30,30^FD" + fakeUPS + "^FS^XZ", nil},
		{"nothing classifies", "^XA^FO50,50^BCN,200^FD>;123456789012^FS^XZ", nil},
		{"no fields", "^XA^XZ", nil},
		{"unterminated", "^XA^FO50,50^BCN,200^FD>:" + fakeUPS, []zplTrackingEntry{{"UPS", fakeUPS}}},
		{"^FD without a barcode command", "^XA^FD" + fakeUPS + "^FS^XZ", nil},
		{"barcode command without ^FD", "^XA^BCN,200^FS^XZ", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := zplTracking([]byte(c.zpl)); !slices.Equal(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// zplBarcodesFixture is TestWriteFixtures' ZPL: a made-up UPS label and a
// made-up USPS one, for trying the tracking line by hand.
const zplBarcodesFixture = "^XA^FO50,50^BY3^BCN,200,Y,N,N^FD>:1ZA1B2C3>54567890123^FS^XZ\n" +
	"^XA^FO50,50^BY3^BCN,200,Y,N,N^FD>;>8420" + fakeZIP5 + ">8" + fakeIMpb22 + "^FS^XZ\n"
