package main

import "testing"

// Made-up tracking values in the carriers' formats. None is a real shipment.
const (
	fakeUPS    = "1ZX9Y8W7V6U5T4S3R2"
	fakeIMpb22 = "9400123456789012345678"
	fakeIMpb26 = "92001234567890123456789012"
	fakeZIP5   = "12345"
	fakeZIP9   = "123451234"
	fakeFedEx  = "9612345678901234567890"             // Ground, 22 digits
	fakeGSN    = "9612345678901234567890123456789012" // Ground, 34 digits
	fakeExp34  = "7000000000000000000000123456789012" // Express, 34 digits
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name, text       string
		rank             int
		carrier, tracked string
	}{
		{"ups", fakeUPS, 1, "UPS", fakeUPS},
		{"ups with ]C0", "]C0" + fakeUPS, 1, "UPS", fakeUPS},
		{"ups lowercase", "1zx9y8w7v6u5t4s3r2", 0, "", ""},
		{"ups too short", fakeUPS[:17], 0, "", ""},

		{"impb gs1 zip5", "]C1420" + fakeZIP5 + gs + fakeIMpb22, 2, "USPS", fakeIMpb22},
		{"impb gs1 zip9", "]C1420" + fakeZIP9 + gs + fakeIMpb22, 2, "USPS", fakeIMpb22},
		{"impb gs1 26", "]C1420" + fakeZIP5 + gs + fakeIMpb26, 2, "USPS", fakeIMpb26},
		{"impb gs1 30", "]C1420" + fakeZIP5 + gs + "95" + "0000000000000000000000001234", 2, "USPS", "950000000000000000000000001234"},
		{"impb gs1 34", "]C1420" + fakeZIP5 + gs + "93" + "00000000000000000000000000001234", 2, "USPS", "9300000000000000000000000000001234"},
		{"impb gs1 bad length", "]C1420" + fakeZIP5 + gs + "94001234", 0, "", ""},
		{"impb gs1 not 92-95", "]C1420" + fakeZIP5 + gs + "9100123456789012345678", 0, "", ""},
		{"impb gs1 bad zip", "]C1420" + "1234" + gs + fakeIMpb22, 0, "", ""},
		{"impb gs1 later elements ignored", "]C1420" + fakeZIP5 + gs + fakeIMpb22 + gs + "X", 2, "USPS", fakeIMpb22},

		// no GS: by length, the 92–95 where the ZIP ends
		{"impb no gs zip5+22", "420" + fakeZIP5 + fakeIMpb22, 2, "USPS", fakeIMpb22},
		{"impb no gs zip9+26", "420" + fakeZIP9 + fakeIMpb26, 2, "USPS", fakeIMpb26},
		{"impb no gs zip5+26", "420" + fakeZIP5 + fakeIMpb26, 2, "USPS", fakeIMpb26},
		{"impb no gs zip9+22", "420" + fakeZIP9 + fakeIMpb22, 2, "USPS", fakeIMpb22},
		// 34 digits that read both ways (the +4 starts 94): ZIP9 + 22 wins
		{"impb no gs 34 ambiguous", "420" + "123459400" + fakeIMpb22, 2, "USPS", fakeIMpb22},
		// 420 first, so never FedEx Express 34
		{"420 + 31 digits", "420" + "0000000000000000000000000000000", 0, "", ""},

		{"routing zip5", "420" + fakeZIP5, 0, "", ""},
		{"routing zip9", "]C1420" + fakeZIP9, 0, "", ""},

		{"fedex ground 22", fakeFedEx, 3, "FedEx", "678901234567890"},
		{"fedex ground 34", fakeGSN, 3, "FedEx", "123456789012"},
		{"fedex ground 34 gs1", "]C1" + fakeGSN, 3, "FedEx", "123456789012"},
		{"fedex express 34", fakeExp34, 4, "FedEx", "123456789012"},
		{"34 digits starting 9", "97" + "00000000000000000000000000000000", 0, "", ""},

		{"usps plain 22", fakeIMpb22, 5, "USPS", fakeIMpb22},
		{"usps plain 26", fakeIMpb26, 5, "USPS", fakeIMpb26},
		{"usps legacy 91", "9100123456789012345678", 0, "", ""},
		{"usps legacy 20", "12345678901234567890", 0, "", ""},

		// reference and order numbers: never tracking
		{"plain 12", "123456789012", 0, "", ""},
		{"plain 15", "123456789012345", 0, "", ""},
		{"plain 22", "1234567890123456789012", 0, "", ""},
		{"text", "ORDER 12345", 0, "", ""},
		{"empty", "", 0, "", ""},
		{"gs only", gs, 0, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := classify(c.text)
			if m.rank != c.rank || m.carrier != c.carrier || m.value != c.tracked {
				t.Errorf("classify(%q) = %+v, want rank %d %s %q", c.text, m, c.rank, c.carrier, c.tracked)
			}
		})
	}
}

func TestBestTracking(t *testing.T) {
	for _, c := range []struct {
		name    string
		texts   []string
		carrier string
		value   string
	}{
		{"routing then ups", []string{"420" + fakeZIP5, fakeUPS}, "UPS", fakeUPS},
		{"plain usps then fedex ground", []string{fakeIMpb22, fakeFedEx}, "FedEx", "678901234567890"},
		{"two of a rank: the first", []string{fakeIMpb26, fakeIMpb22}, "USPS", fakeIMpb26},
		{"impb beats plain usps", []string{fakeIMpb26, "]C1420" + fakeZIP5 + gs + fakeIMpb22}, "USPS", fakeIMpb22},
		{"nothing", []string{"123456789012"}, "", ""},
		{"none decoded", nil, "", ""},
	} {
		m := bestTracking(c.texts)
		if m.carrier != c.carrier || m.value != c.value {
			t.Errorf("%s: %+v, want %s %q", c.name, m, c.carrier, c.value)
		}
	}
}
