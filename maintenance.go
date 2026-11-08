package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// maintenanceAction is a printer chore sent as an ordinary ZPL job, so it goes
// through LPrint and shows up in its job list like any print.
type maintenanceAction struct {
	JobName string
	ZPL     string
}

// maintenanceActions is the whole allow-list: the client names an action and
// never sends ZPL of its own. LPrint's ZPL driver copies the document to the
// printer unchanged (lprint_zpl_printfile).
//
// feed prints a one-dot box in the corner, which forces exactly one label out.
// On the GX420t neither ~PH (slew to home) nor an empty ^XA^XZ format feeds
// anything once the label is already at its home position.
//
// There is deliberately no remote calibrate: ~JC through LPrint fed 6–7 labels
// and then left the GX420t jammed or reporting media-empty, with nobody at the
// printer to clear it. Calibrate at the printer with SmartCal instead.
var maintenanceActions = map[string]maintenanceAction{
	"feed": {JobName: "labelprint: feed one label", ZPL: "^XA^FO0,0^GB1,1,1^FS^XZ\n"},
}

func (s *server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	// Exactly one JSON value: anything after it (a second object, garbage)
	// makes the request malformed, and a malformed request mustn't feed.
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		s.fail(w, badRequest(`expected {"action": "feed"}`))
		return
	}
	act, ok := maintenanceActions[req.Action]
	if !ok {
		s.fail(w, badRequest("unknown printer action %q", truncateUTF8(req.Action, 32)))
		return
	}

	// Copies and media stay unset, as for uploaded ZPL (see buildJobs).
	pr := printRequest{
		JobName: act.JobName,
		User:    s.requestUser(r),
		Format:  kindZPL.mimeType(),
		Copies:  1,
		Data:    []byte(act.ZPL),
	}
	s.printMu.Lock()
	job, warning, err := s.ipp.printJob(r.Context(), pr)
	s.printMu.Unlock()

	s.log.Info("maintenance", "action", req.Action, "job", job.ID, "error", errString(err))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, printResponse{Jobs: []jobInfo{}, Error: err.Error()})
		return
	}
	resp := printResponse{Jobs: []jobInfo{job}}
	if warning != "" {
		resp.Warnings = []string{warning}
	}
	writeJSON(w, http.StatusOK, resp)
}
