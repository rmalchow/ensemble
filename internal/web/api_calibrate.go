package web

import (
	"errors"
	"net/http"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// This file implements F2.1 POST /api/v1/calibrate/play (08 §F2.1, A.10b): play
// the in-process calibration signal synchronously on the selected group or nodes
// for durationSec. The body carries EXACTLY ONE of groupId / nodeIds (neither or
// both => 400). It is transient (no ConfigDoc write) so there is no If-Match. The
// engine is reached only through Deps.CalibratePlay, which fans the playback out
// over mTLS and reports playedOn / per-node warnings (a Render=false node cannot
// play and lands in warnings, not a fatal error).

// registerCalibrateRoutes mounts the §F2.1 endpoint behind RequireAdminSession
// (operator-initiated). Called from registerAPIRoutes.
func (s *Server) registerCalibrateRoutes(api *http.ServeMux) {
	api.Handle("POST /api/v1/calibrate/play", auth.RequireAdminSession(http.HandlerFunc(s.handleCalibratePlay)))
}

// calibrateRequest is the F2.1 body: exactly one of groupId / nodeIds, plus
// durationSec (default 5 if omitted/<=0).
type calibrateRequest struct {
	GroupID     string   `json:"groupId,omitempty"`
	NodeIDs     []string `json:"nodeIds,omitempty"`
	DurationSec int      `json:"durationSec"`
}

// calibrateResponse is the F2.1 success body: the nodes the signal played on and
// any per-node warnings (Render=false, unreachable peer, …).
type calibrateResponse struct {
	PlayedOn []string `json:"playedOn"`
	Warnings []string `json:"warnings,omitempty"`
}

// defaultCalibrateSec is the fallback duration when the body omits durationSec.
const defaultCalibrateSec = 5

// handleCalibratePlay serves POST /api/v1/calibrate/play (08 §F2.1). It validates
// the exactly-one selector rule, then drives Deps.CalibratePlay and returns
// playedOn/warnings. A nil error with warnings is a partial success (200).
func (s *Server) handleCalibratePlay(w http.ResponseWriter, r *http.Request) {
	if s.deps.CalibratePlay == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "calibration unavailable")
		return
	}
	var req calibrateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	haveGroup := req.GroupID != ""
	haveNodes := len(req.NodeIDs) > 0
	if haveGroup == haveNodes {
		// Neither or both: exactly-one rule (F2.1).
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "provide exactly one of groupId or nodeIds")
		return
	}
	dur := req.DurationSec
	if dur <= 0 {
		dur = defaultCalibrateSec
	}
	sel := CalibrateSel{GroupID: req.GroupID, NodeIDs: req.NodeIDs}

	played, warnings, err := s.deps.CalibratePlay(sel, dur)
	if err != nil {
		writeCalibrateErr(w, err)
		return
	}
	if played == nil {
		played = []string{}
	}
	writeJSON(w, calibrateResponse{PlayedOn: played, Warnings: warnings})
}

// writeCalibrateErr maps a Deps.CalibratePlay sentinel to its status. An unknown
// group is 404; an unreachable master/peer is 502; everything else is 500.
func writeCalibrateErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotMember):
		writeErr(w, http.StatusNotFound, codeNotFound, "group not found")
	case errors.Is(err, ErrUnreachable):
		writeErr(w, http.StatusBadGateway, codeProxyFailed, "target unreachable")
	default:
		writeErr(w, http.StatusInternalServerError, codeInternal, "calibration failed")
	}
}
