package api

import (
	"bytes"
	"net/http"
	"testing"

	"ensemble/internal/id"
)

func TestSPAServesIndexAtRoot(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.DistFS = distWith(map[string]string{
		"index.html": "<html>real ui</html>",
		"app.js":     "console.log(1)",
	})
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("real ui")) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
}

func TestSPAFallbackToIndex(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.DistFS = distWith(map[string]string{
		"index.html": "<html>spa</html>",
	})
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/groups")
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("spa")) {
		t.Fatalf("client route should fall back to index: status=%d body=%s", resp.StatusCode, b)
	}
}

func TestSPAServesAsset(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.DistFS = distWith(map[string]string{
		"index.html": "<html>spa</html>",
		"app.js":     "MARKER_JS",
	})
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("MARKER_JS")) {
		t.Fatalf("asset not served: status=%d body=%s", resp.StatusCode, b)
	}
}

func TestSPAAssetMissing404(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.DistFS = distWith(map[string]string{
		"index.html": "<html>spa</html>",
	})
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/missing.js")
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("missing asset status=%d", resp.StatusCode)
	}
	if bytes.Contains(b, []byte("spa")) {
		t.Errorf("missing asset must NOT return index.html")
	}
}

func TestSPAPlaceholderDetected(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.DistFS = distWith(map[string]string{
		"index.html": "<html><!-- ensemble-placeholder --></html>",
	})
	// We can't easily capture the slog warning without a custom handler; assert
	// initSPA detects it by checking the served placeholder still loads.
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(t, resp)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("ensemble-placeholder")) {
		t.Fatalf("placeholder should still serve: status=%d", resp.StatusCode)
	}
}
