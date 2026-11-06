package main

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// mediaOption is one entry in the size dropdown.
type mediaOption struct {
	Name  string `json:"name"`  // PWG keyword sent as `media`, e.g. na_index-4x6_4x6in
	Label string `json:"label"` // "4 × 6 in"
	Ready bool   `json:"ready"` // listed in media-ready (what LPrint thinks is loaded)
	w, h  float64
}

// PWG 5101.1 self-describing names: class_name_WxHunit, e.g.
// oe_2.25x1.25-label_2.25x1.25in or iso_a6_105x148mm.
var pwgMediaRe = regexp.MustCompile(`^([a-z0-9-]+)_(.+)_([0-9.]+)x([0-9.]+)(in|mm)$`)

// mediaOptions builds the dropdown from LPrint's media-supported. The roll_max
// / roll_min (and custom_min/max) entries describe the custom-size range, not
// sizes you can pick, so they're left out.
func mediaOptions(supported, ready []string) []mediaOption {
	var out []mediaOption
	for _, name := range supported {
		m := pwgMediaRe.FindStringSubmatch(name)
		if m == nil {
			out = append(out, mediaOption{Name: name, Label: name, Ready: slices.Contains(ready, name)})
			continue
		}
		class, nick, ws, hs, unit := m[1], m[2], m[3], m[4], m[5]
		if class == "roll" || class == "custom" || strings.HasPrefix(nick, "max") || strings.HasPrefix(nick, "min") {
			continue
		}
		w, _ := strconv.ParseFloat(ws, 64)
		h, _ := strconv.ParseFloat(hs, 64)
		label := ws + " × " + hs + " " + unit
		// Nicknames that only restate the size ("2x1-label", "index-4x6") add nothing.
		dims := ws + "x" + hs
		if nick != dims+"-label" && nick != dims && nick != "index-"+dims {
			label += " · " + strings.TrimSuffix(nick, "-label")
		}
		if unit == "mm" { // sort alongside inch sizes
			w, h = w/25.4, h/25.4
		}
		out = append(out, mediaOption{Name: name, Label: label, Ready: slices.Contains(ready, name), w: w, h: h})
	}
	slices.SortStableFunc(out, func(a, b mediaOption) int {
		return cmp.Or(cmp.Compare(a.w, b.w), cmp.Compare(a.h, b.h))
	})
	return out
}

// mediaSize reads the dimensions out of a PWG media keyword
// (na_index-4x6_4x6in → 101.6 × 152.4 mm).
func mediaSize(name string) (labelSize, bool) {
	m := pwgMediaRe.FindStringSubmatch(name)
	if m == nil {
		return labelSize{}, false
	}
	w, errW := strconv.ParseFloat(m[3], 64)
	h, errH := strconv.ParseFloat(m[4], 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return labelSize{}, false
	}
	if m[5] == "in" {
		w, h = w*25.4, h*25.4
	}
	return labelSize{WidthMm: w, HeightMm: h}, true
}

// validMediaKeyword guards what we forward as `media` when LPrint's list isn't
// available to check against.
var validMediaKeyword = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)
