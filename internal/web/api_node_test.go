package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// nodeTestDeps builds a Server whose State carries one node and whose
// NodeDetail/SetNodeConfig closures act on a fake doc, mirroring the daemon's
// contract (ErrNotFound / ErrVersionConflict).
func nodeTestDeps(version *uint64, name *string) Deps {
	var keys []state.APIKey
	d := authTestDeps(&keys, version)
	d.State = func() ConfigView {
		return ConfigView{Version: *version, Nodes: []NodeView{{ID: "n-1", Name: *name, Addrs: []string{}}}}
	}
	d.NodeDetail = func(id string) (NodeDetailView, bool) {
		if id != "n-1" {
			return NodeDetailView{}, false
		}
		return NodeDetailView{
			NodeView:    NodeView{ID: "n-1", Name: *name, Addrs: []string{}},
			Fingerprint: "sha256:ab",
			Online:      true,
			GroupID:     "default",
			IsMaster:    true,
		}, true
	}
	d.SetNodeConfig = func(id string, patch NodePatch, ifMatch uint64) error {
		if id != "n-1" {
			return ErrNotFound
		}
		if ifMatch != *version {
			return ErrVersionConflict
		}
		if patch.Name != nil {
			*name = *patch.Name
		}
		*version++
		return nil
	}
	return d
}

func TestNodeDetailAndPatch(t *testing.T) {
	version := uint64(42)
	name := "kitchen"
	s := New(nodeTestDeps(&version, &name), "")
	cookie := loginAndGetCookie(t, s)

	// GET unknown id -> 404.
	rec := doJSON(t, s, http.MethodGet, "/api/v1/nodes/nope", nil, cookieHdr(cookie))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown node: got %d want 404 (%s)", rec.Code, rec.Body.String())
	}

	// GET known id -> {version, node} with the joined detail fields.
	rec = doJSON(t, s, http.MethodGet, "/api/v1/nodes/n-1", nil, cookieHdr(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET node: got %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got nodeDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != 42 || got.Node.ID != "n-1" || !got.Node.Online || !got.Node.IsMaster ||
		got.Node.GroupID != "default" || got.Node.Fingerprint != "sha256:ab" {
		t.Fatalf("node detail = %+v", got)
	}

	// PATCH without If-Match -> 428 precondition_required.
	newName := "livingroom"
	rec = doJSON(t, s, http.MethodPatch, "/api/v1/nodes/n-1", NodePatch{Name: &newName}, cookieHdr(cookie))
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PATCH no If-Match: got %d want 428 (%s)", rec.Code, rec.Body.String())
	}

	// PATCH with a stale If-Match -> 409.
	hdr := cookieHdr(cookie)
	hdr["If-Match"] = "7"
	rec = doJSON(t, s, http.MethodPatch, "/api/v1/nodes/n-1", NodePatch{Name: &newName}, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH stale If-Match: got %d want 409 (%s)", rec.Code, rec.Body.String())
	}

	// PATCH with a bad channel -> 400 before the dep runs.
	bad := "surround"
	hdr["If-Match"] = "42"
	rec = doJSON(t, s, http.MethodPatch, "/api/v1/nodes/n-1", NodePatch{Channel: &bad}, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH bad channel: got %d want 400 (%s)", rec.Code, rec.Body.String())
	}

	// PATCH ok -> 200 with the post-write projection + bumped version/ETag.
	rec = doJSON(t, s, http.MethodPatch, "/api/v1/nodes/n-1", NodePatch{Name: &newName}, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: got %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != 43 || got.Node.Name != "livingroom" {
		t.Fatalf("post-patch detail = %+v", got)
	}
	if rec.Header().Get("ETag") != "43" {
		t.Fatalf("post-patch ETag = %q want 43", rec.Header().Get("ETag"))
	}
}
