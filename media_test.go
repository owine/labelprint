package main

import "testing"

func TestMediaOptions(t *testing.T) {
	// A slice of what this LPrint really reports for the GX420t.
	supported := []string{
		"oe_4x6.5-label_4x6.5in", "na_index-4x6_4x6in", "oe_2x1-label_2x1in",
		"oe_30859-paint-can-label_2.25x3.125in", "iso_a6_105x148mm",
		"roll_max_4x39.6in", "roll_min_0.75x0.25in",
	}
	got := mediaOptions(supported, []string{"na_index-4x6_4x6in"})
	want := []struct {
		name, label string
		ready       bool
	}{
		{"oe_2x1-label_2x1in", "2 × 1 in", false},
		{"oe_30859-paint-can-label_2.25x3.125in", "2.25 × 3.125 in · 30859-paint-can", false},
		{"na_index-4x6_4x6in", "4 × 6 in", true},
		{"oe_4x6.5-label_4x6.5in", "4 × 6.5 in", false},
		{"iso_a6_105x148mm", "105 × 148 mm · a6", false}, // 4.13 in wide
	}
	if len(got) != len(want) {
		t.Fatalf("got %d options: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Label != w.label || got[i].Ready != w.ready {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestMediaKeywordGuard(t *testing.T) {
	for _, ok := range []string{"na_index-4x6_4x6in", "oe_2.25x1.25-label_2.25x1.25in"} {
		if !validMediaKeyword.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "Na_Index", "x y", "a\x00b", "-x"} {
		if validMediaKeyword.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
