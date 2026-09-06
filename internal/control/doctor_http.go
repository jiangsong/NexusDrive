package control

import (
	"net/http"
)

// DoctorResponse is what the diagnostics page renders: the checks as the CLI
// prints them, with the same names, so a person can quote one to the terminal.
type DoctorResponse struct {
	Checks []Check `json:"checks"`
	OK     int     `json:"ok"`
	Warn   int     `json:"warn"`
	Fail   int     `json:"fail"`
}

// DoctorFixRequest is the body of /doctor/fix.
type DoctorFixRequest struct {
	Confirm bool `json:"confirm"`
}

// DoctorFixResponse lists what Fix did, in the words Fix uses.
type DoctorFixResponse struct {
	Done []string `json:"done"`
}

// POST /doctor/run. It is a POST although it only diagnoses, because Run is
// not a pure read: it writes a probe file to the cache directory and
// checkpoints the metadata database. Nothing it does needs confirming.
func (s *Server) doctorRun(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.collector.Doctor == nil {
		http.Error(w, "diagnostics are not wired on this daemon", http.StatusNotImplemented)
		return
	}
	checks := s.collector.Doctor.Run(r.Context())
	ok, warn, fail := Summary(checks)
	if checks == nil {
		checks = []Check{}
	}
	writeJSON(w, DoctorResponse{Checks: checks, OK: ok, Warn: warn, Fail: fail})
}

// POST /doctor/fix {"confirm": true}. Fix runs journal recovery, purges old
// completed uploads and evicts from the cache — each reversible only in the
// sense that the data was already redundant, which is a judgement the person
// makes, not the page.
func (s *Server) doctorFix(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.collector.Doctor == nil {
		http.Error(w, "diagnostics are not wired on this daemon", http.StatusNotImplemented)
		return
	}
	var q DoctorFixRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	if err := requireConfirm(q.Confirm, "runs journal recovery, purges completed uploads and evicts from the cache"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, DoctorFixResponse{Done: s.collector.Doctor.Fix(r.Context())})
}
