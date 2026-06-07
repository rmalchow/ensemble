package api

import (
	"net/http"
	"testing"

	"ensemble/internal/id"
)

func TestObserveOnProxiedRequest(t *testing.T) {
	self := id.New()
	from := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	_, ts := testServer(t, cfg)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.Header.Set(proxiedHeader, "1")
	req.Header.Set(fromHeader, from.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	calls := fc.observeCalls()
	if len(calls) != 1 {
		t.Fatalf("observe calls = %d, want 1", len(calls))
	}
	if calls[0].peer != from {
		t.Errorf("observed peer = %v, want %v", calls[0].peer, from)
	}
	if !calls[0].ip.IsValid() || !calls[0].ip.IsLoopback() {
		t.Errorf("observed ip = %v, want loopback", calls[0].ip)
	}
}

func TestObserveIgnoresXForwardedFor(t *testing.T) {
	self := id.New()
	from := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	_, ts := testServer(t, cfg)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.Header.Set(proxiedHeader, "1")
	req.Header.Set(fromHeader, from.String())
	req.Header.Set("X-Forwarded-For", "10.9.9.9")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	calls := fc.observeCalls()
	if len(calls) != 1 {
		t.Fatalf("observe calls = %d", len(calls))
	}
	if calls[0].ip.String() == "10.9.9.9" {
		t.Errorf("X-Forwarded-For must not influence observed IP")
	}
}

func TestObserveSkipsLocalNonProxied(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/status", nil)
	resp.Body.Close()

	if len(fc.observeCalls()) != 0 {
		t.Errorf("plain local request must not observe")
	}
}
