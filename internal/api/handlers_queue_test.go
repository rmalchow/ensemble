package api

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"ensemble/internal/contracts"
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

func TestQueuePlayDelegates(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/queue/play", map[string]any{
		"index": 3, "uri": "file:d.mp3",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if fg.playQIndex != 3 || fg.playQURI != "file:d.mp3" {
		t.Fatalf("playQ = (%d, %q), want (3, file:d.mp3)", fg.playQIndex, fg.playQURI)
	}
}

func TestQueuePlayRejectsNegativeIndex(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/queue/play", map[string]any{"index": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestQueueListReturnsItems(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.queueList = []contracts.QueueItem{
		{URI: "file:a.mp3", Metadata: &contracts.TrackMetadata{Title: "A"}},
		{URI: "file:b.mp3"},
	}
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []contracts.QueueItem
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(got) != 2 || got[0].URI != "file:a.mp3" || got[1].URI != "file:b.mp3" {
		t.Fatalf("queue = %+v", got)
	}
}

func TestQueueListEmptyIsArray(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
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
