package web

import (
	"net/http"
	"testing"
)

// api_calibrate_test.go drives F2.1 over a stub Deps (P4.9 §7.3): exactly-one
// selector validation + playedOn/warnings passthrough.

func newCalibrateServer(played, warnings []string, err error, capture *CalibrateSel) *Server {
	return New(Deps{
		NodeID:              "n-self",
		Initialized:         func() bool { return true },
		VerifyAdminPassword: func(pw string) bool { return pw == testPw },
		CalibratePlay: func(sel CalibrateSel, _ int) ([]string, []string, error) {
			if capture != nil {
				*capture = sel
			}
			return played, warnings, err
		},
	}, "")
}

func TestCalibrateSelectorValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     calibrateRequest
		wantCode int
	}{
		{"neither => 400", calibrateRequest{DurationSec: 3}, http.StatusBadRequest},
		{"both => 400", calibrateRequest{GroupID: "g1", NodeIDs: []string{"n1"}, DurationSec: 3}, http.StatusBadRequest},
		{"group only => 200", calibrateRequest{GroupID: "g1", DurationSec: 3}, http.StatusOK},
		{"nodes only => 200", calibrateRequest{NodeIDs: []string{"n1"}, DurationSec: 3}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newCalibrateServer([]string{"n1"}, nil, nil, nil)
			hdr := cookieHdr(loginAndGetCookie(t, s))
			rec := doJSON(t, s, http.MethodPost, "/api/v1/calibrate/play", tc.body, hdr)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

func TestCalibratePassthrough(t *testing.T) {
	s := newCalibrateServer([]string{"n1", "n2"}, []string{"n3: cannot render"}, nil, nil)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	rec := doJSON(t, s, http.MethodPost, "/api/v1/calibrate/play", calibrateRequest{GroupID: "g1", DurationSec: 5}, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var resp calibrateResponse
	decodeBody(t, rec.Body.Bytes(), &resp)
	if len(resp.PlayedOn) != 2 || len(resp.Warnings) != 1 {
		t.Fatalf("resp = %+v, want playedOn[2]/warnings[1]", resp)
	}
}

func TestCalibrateDefaultDuration(t *testing.T) {
	var captured CalibrateSel
	s := newCalibrateServer([]string{"n1"}, nil, nil, &captured)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	// Omit durationSec: the handler must still succeed (defaultCalibrateSec).
	rec := doJSON(t, s, http.MethodPost, "/api/v1/calibrate/play", calibrateRequest{NodeIDs: []string{"n1"}}, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(captured.NodeIDs) != 1 || captured.NodeIDs[0] != "n1" {
		t.Fatalf("selector = %+v, want NodeIDs[n1]", captured)
	}
}
