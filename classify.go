package main

import (
	"regexp"
	"strings"
)

// Tracking numbers by format alone: no check digits, no carrier lookup. The
// rules run in rank order and the first a value matches wins; across a
// label's barcodes the best rank wins, and among equals the first in reading
// order. Plain 12- or 15-digit numbers are never tracking: reference and
// order-number barcodes look just like them, and would show a false "FedEx ✓".

// trackingMatch is the rule a decoded value matched: rank 1 (best) to 5, or 0
// for none. value is what the page shows.
type trackingMatch struct {
	rank    int
	carrier string
	value   string
}

// gs is the GS1 element separator (ASCII 29): what gozxing returns for every
// FNC1 after the first with ASSUME_GS1, and what a later ZPL >8 becomes.
const gs = "\x1d"

var (
	reUPS       = regexp.MustCompile(`^1Z[0-9A-Z]{16}$`)
	reRouting   = regexp.MustCompile(`^420(\d{5}|\d{9})$`)
	reIMpb      = regexp.MustCompile(`^9[2-5]\d+$`)
	reDigits    = regexp.MustCompile(`^\d+$`)
	reGround22  = regexp.MustCompile(`^96\d{20}$`)
	reGround34  = regexp.MustCompile(`^96\d{32}$`)
	reExpress34 = regexp.MustCompile(`^[0-8]\d{33}$`)
	reUSPSPlain = regexp.MustCompile(`^9[2-5](\d{20}|\d{24})$`)
)

// classify matches one decoded value, after dropping a leading ]C1/]C0 (the
// GS1 and plain Code 128 symbology identifiers). GS separators are kept: they
// split a GS1 value into its elements.
func classify(text string) trackingMatch {
	if strings.HasPrefix(text, "]C1") || strings.HasPrefix(text, "]C0") {
		text = text[3:]
	}
	switch {
	case reUPS.MatchString(text):
		return trackingMatch{1, "UPS", text}
	case strings.HasPrefix(text, "420"):
		// USPS IMpb before any FedEx rule: 420 + ZIP9 + 22 is 34 digits too.
		// 420 + ZIP and nothing usable after it is postal routing (rule 6),
		// never tracking.
		if v, ok := impbTracking(text); ok {
			return trackingMatch{2, "USPS", v}
		}
		return trackingMatch{}
	case reGround22.MatchString(text):
		return trackingMatch{3, "FedEx", text[len(text)-15:]}
	case reGround34.MatchString(text):
		return trackingMatch{3, "FedEx", text[len(text)-12:]}
	case reExpress34.MatchString(text):
		return trackingMatch{4, "FedEx", text[len(text)-12:]}
	case reUSPSPlain.MatchString(text):
		// FedEx SmartPost / Ground Economy numbers are USPS-format IMpb
		// numbers, so there's no FedEx plain 20/22 rule
		return trackingMatch{5, "USPS", text}
	}
	return trackingMatch{}
}

// impbTracking is the tracking number in a USPS IMpb value that starts with
// 420: the element after "420 + ZIP5/ZIP9" when GS1 split it, else found by
// length.
func impbTracking(text string) (string, bool) {
	if els := strings.Split(text, gs); len(els) > 1 {
		next := els[1]
		switch len(next) {
		case 22, 26, 30, 34: // real IMpb lengths
		default:
			return "", false
		}
		if reRouting.MatchString(els[0]) && reIMpb.MatchString(next) {
			return next, true
		}
		return "", false
	}
	// No GS: the reader didn't see GS1. Split by length, with the 92–95
	// right where the ZIP ends. ZIP9 is tried first: 34 digits also fits
	// 420 + ZIP5 + 26 when the +4 starts 92–95, and ZIP9 + 22 is the common
	// one. 30 (ZIP5 + 22) and 38 (ZIP9 + 26) can only be read one way.
	if !reDigits.MatchString(text) {
		return "", false
	}
	for _, zip := range []int{9, 5} {
		if len(text) < 3+zip+2 {
			continue
		}
		rest := text[3+zip:]
		if (len(rest) == 22 || len(rest) == 26) && rest[0] == '9' && rest[1] >= '2' && rest[1] <= '5' {
			return rest, true
		}
	}
	return "", false
}

// bestTracking is the winning match among values in reading order.
func bestTracking(texts []string) trackingMatch {
	var best trackingMatch
	for _, t := range texts {
		if m := classify(t); m.rank > 0 && (best.rank == 0 || m.rank < best.rank) {
			best = m
		}
	}
	return best
}
