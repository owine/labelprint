package main

import (
	"strings"
	"testing"
)

func TestConfigRequiresLPrintURI(t *testing.T) {
	t.Setenv("LPRINT_URI", "")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "LPRINT_URI is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Setenv("LPRINT_URI", "ipp://192.0.2.10:8000/ipp/print/q")
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.UserHeader != "" {
		t.Errorf("USER_HEADER should be opt-in, got %q", c.UserHeader)
	}
	if c.MaxUpload != 20<<20 || c.DPI != 203 || c.DefaultMedia != "na_index-4x6_4x6in" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestConfigRejectsBadNumbers(t *testing.T) {
	t.Setenv("LPRINT_URI", "ipp://192.0.2.10/ipp/print/q")
	t.Setenv("MAX_UPLOAD_MB", "0")
	t.Setenv("RENDER_DPI", "lots")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "MAX_UPLOAD_MB") || !strings.Contains(err.Error(), "RENDER_DPI") {
		t.Fatalf("err = %v", err)
	}
}
