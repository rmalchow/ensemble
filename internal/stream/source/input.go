package source

// Input opener: resolve a path arg to a byte stream. An http/https URL is fetched
// via net/http yielding a non-seekable io.ReadCloser (looping re-issues the GET);
// anything else is treated as a filename under dataDir, validated against
// directory traversal, and opened as a seekable *os.File. The opener returns both
// the stream and a reopen() closure used by loopReader to restart a non-seekable
// HTTP body at the loop boundary.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// httpClient is shared so HTTP/keep-alive connections are reused across loop
// re-requests. The timeout bounds a stalled finite download (live/infinite
// streams are out of MVP scope — see §9 open question).
var httpClient = &http.Client{Timeout: 60 * time.Second}

// isHTTPURL reports whether path is an http(s):// URL (vs. a data/ filename).
func isHTTPURL(path string) bool {
	u, err := url.Parse(path)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// openInput resolves path to a byte stream and a reopen closure. An http(s) URL
// yields a non-seekable body (reopen re-issues the GET); any other path is opened
// as a seekable local file (no reopen — seekStart repositions in place).
func openInput(path string) (rc io.ReadCloser, reopen func() (io.ReadCloser, error), err error) {
	if isHTTPURL(path) {
		open := func() (io.ReadCloser, error) { return httpGet(path) }
		body, herr := open()
		if herr != nil {
			return nil, nil, herr
		}
		return body, open, nil
	}
	if path == "" {
		return nil, nil, errors.New("source: empty path")
	}
	f, oerr := os.Open(path)
	if oerr != nil {
		return nil, nil, oerr
	}
	return f, nil, nil
}

// ResolveDataPath validates a data/-relative media filename and returns its
// absolute, cleaned path under dataDir (config Paths.Data). It rejects absolute
// paths and any path that escapes dataDir, so callers (cmd/web) can safely turn a
// ConfigDoc.Groups[].Media.File value into the path argument for Open without a
// traversal risk. HTTP(S) URLs are returned unchanged (Open handles them).
func ResolveDataPath(name, dataDir string) (string, error) {
	if isHTTPURL(name) {
		return name, nil
	}
	if dataDir == "" {
		return "", errors.New("source: empty data dir")
	}
	if name == "" {
		return "", errors.New("source: empty path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("source: absolute path not allowed: %q", name)
	}
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(base, filepath.Clean("/"+name))
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source: path escapes data dir: %q", name)
	}
	return full, nil
}

// httpGet fetches url and returns its body as a ReadCloser. Non-2xx responses are
// turned into errors (and the body is drained/closed).
func httpGet(rawURL string) (io.ReadCloser, error) {
	resp, err := httpClient.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("source: http get %s: %w", rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("source: http get %s: status %d", rawURL, resp.StatusCode)
	}
	return resp.Body, nil
}
