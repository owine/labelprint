package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postMaintenance(t *testing.T, h http.Handler, body, site string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/printer/maintenance", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", site)
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestMaintenanceSendsZPLThroughLPrint(t *testing.T) {
	cases := []struct {
		action, doc, jobName string
	}{
		{"feed", "^XA^FO0,0^GB1,1,1^FS^XZ\n", "labelprint: feed one label"},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			fake, h := newTestServer(t)
			rec, out := postMaintenance(t, h, `{"action":"`+c.action+`"}`, "same-origin")
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			jobs := fake.printJobs()
			if len(jobs) != 1 {
				t.Fatalf("%d Print-Jobs, want 1", len(jobs))
			}
			m := jobs[0]
			if got := string(m.Data); got != c.doc {
				t.Errorf("document = %q, want %q", got, c.doc)
			}
			if got := opAttr(m, "document-format").str(); got != "application/vnd.zebra-zpl" {
				t.Errorf("document-format = %q", got)
			}
			if got := opAttr(m, "job-name").str(); got != c.jobName {
				t.Errorf("job-name = %q, want %q", got, c.jobName)
			}
			if got := opAttr(m, "requesting-user-name").str(); got != "alice" {
				t.Errorf("user = %q", got)
			}
			// As with uploaded ZPL: LPrint ignores both for raw ZPL.
			if g := m.group(tagJobGroup); g != nil && (g.get("copies") != nil || g.get("media") != nil) {
				t.Errorf("unexpected job attributes: %+v", g.Attrs)
			}
			got, _ := out["jobs"].([]any)
			if len(got) != 1 || got[0].(map[string]any)["name"] != c.jobName {
				t.Errorf("response jobs = %v", out["jobs"])
			}
		})
	}
}

func TestMaintenanceRejectsAnythingElse(t *testing.T) {
	for _, body := range []string{
		`{"action":"selftest"}`,
		`{"action":"calibrate"}`, // left out on purpose: see maintenanceActions
		`{"action":"FEED"}`,
		`{"action":""}`,
		`{}`,
		`{"action":"feed","zpl":"^XA^FDsneaky^FS^XZ"}`, // no client-supplied zpl, not even alongside a valid action
		`not json`,
		`{"action":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		t.Run(body[:min(len(body), 24)], func(t *testing.T) {
			fake, h := newTestServer(t)
			rec, out := postMaintenance(t, h, body, "same-origin")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
			if s, _ := out["error"].(string); s == "" {
				t.Error("no error message")
			}
			if len(fake.printJobs()) != 0 {
				t.Error("a rejected request reached lprint")
			}
		})
	}
}

func TestMaintenanceCrossSiteRejected(t *testing.T) {
	fake, h := newTestServer(t)
	rec, _ := postMaintenance(t, h, `{"action":"feed"}`, "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("cross-site request reached lprint")
	}
}

func TestMaintenanceGetNotAllowed(t *testing.T) {
	fake, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/printer/maintenance?action=feed", nil))
	if rec.Code < 400 { // falls through to the static files: 404
		t.Fatalf("status %d", rec.Code)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("a GET reached lprint")
	}
}

func TestMaintenanceShowsLPrintError(t *testing.T) {
	fake, h := newTestServer(t)
	fake.printStatus, fake.printMessage = 0x0507, "Printer busy."
	rec, out := postMaintenance(t, h, `{"action":"feed"}`, "same-origin")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "Printer busy") {
		t.Fatalf("error = %q", msg)
	}
}

func TestMaintenanceWhenLPrintDown(t *testing.T) {
	_, h := newTestServer(t, func(c *config) { c.LPrintURI = "ipp://127.0.0.1:1/ipp/print/x" })
	rec, out := postMaintenance(t, h, `{"action":"feed"}`, "same-origin")
	if rec.Code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "can't reach lprint") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}
