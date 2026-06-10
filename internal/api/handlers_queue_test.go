package api

import (
	"net/http"
	"reflect"
	"testing"

	"ensemble/internal/id"
)

func TestEnqueueFoldsAndDelegates(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/queue", map[string]any{
		"uris": []string{"a.mp3", "file:b.mp3"},
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	want := []string{"file:a.mp3", "file:b.mp3"}
	if !reflect.DeepEqual(fg.enqueueURIs, want) {
		t.Fatalf("enqueued = %v, want %v", fg.enqueueURIs, want)
	}
}

func TestEnqueueRejectsTraversal(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/queue", map[string]any{
		"uris": []string{"../escape.mp3"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestQueueRemoveDelegates(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/queue/remove", map[string]any{
		"index": 2, "uri": "file:c.mp3",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if fg.removeIndex != 2 || fg.removeURI != "file:c.mp3" {
		t.Fatalf("remove = (%d, %q), want (2, file:c.mp3)", fg.removeIndex, fg.removeURI)
	}
}

func TestQueueRemoveRejectsNegativeIndex(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/queue/remove", map[string]any{"index": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestNextDelegates(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/next", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if fg.nextN != 1 {
		t.Fatalf("next called %d times, want 1", fg.nextN)
	}
}
