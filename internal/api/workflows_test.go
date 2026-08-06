package api

import (
	"fmt"
	"testing"

	"github.com/namesarnav/synapse/internal/workflow/wfstore"
)

func TestWorkflowCRUDAndOptimisticConcurrency(t *testing.T) {
	h := newHarness(t)
	a := h.register("w@example.com")

	r := h.do("POST", a.wf(""), a.Token, map[string]any{"name": "My flow", "graph": simpleGraph})
	if r.Status != 201 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	var wf wfstore.Workflow
	r.JSON(t, &wf)
	if wf.Revision != 1 || wf.Status != "draft" || len(wf.Graph.Nodes) != 2 {
		t.Fatalf("unexpected %+v", wf)
	}

	r = h.do("PUT", a.wf("/"+wf.ID), a.Token, map[string]any{"name": "Renamed", "revision": 1})
	if r.Status != 200 {
		t.Fatalf("update: %d %s", r.Status, r.Body)
	}
	r.JSON(t, &wf)
	if wf.Name != "Renamed" || wf.Revision != 2 {
		t.Fatalf("after update %+v", wf)
	}
	if r := h.do("PUT", a.wf("/"+wf.ID), a.Token, map[string]any{"name": "Stale", "revision": 1}); r.Status != 409 {
		t.Fatalf("stale revision got %d", r.Status)
	}
	if r := h.do("PUT", a.wf("/"+wf.ID), a.Token, map[string]any{"name": "  "}); r.Status != 422 {
		t.Fatalf("blank name got %d", r.Status)
	}
	if r := h.do("DELETE", a.wf("/"+wf.ID), a.Token, nil); r.Status != 204 {
		t.Fatalf("delete got %d", r.Status)
	}
	if r := h.do("GET", a.wf("/"+wf.ID), a.Token, nil); r.Status != 404 {
		t.Fatalf("deleted workflow got %d", r.Status)
	}
}

func TestWorkflowListPagination(t *testing.T) {
	h := newHarness(t)
	a := h.register("l@example.com")
	for i := 0; i < 5; i++ {
		if r := h.do("POST", a.wf(""), a.Token, map[string]any{"name": fmt.Sprintf("flow-%d", i)}); r.Status != 201 {
			t.Fatal(r.Status, string(r.Body))
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		path := a.wf("?limit=2")
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := h.do("GET", path, a.Token, nil)
		var body struct {
			Items      []wfstore.Summary `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		r.JSON(t, &body)
		for _, it := range body.Items {
			if seen[it.ID] {
				t.Fatalf("duplicate item across pages: %s", it.ID)
			}
			seen[it.ID] = true
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paginated %d of 5", len(seen))
	}
	if r := h.do("GET", a.wf("?cursor=!!!"), a.Token, nil); r.Status != 400 {
		t.Fatalf("bad cursor got %d", r.Status)
	}
	var body struct{ Items []wfstore.Summary }
	h.do("GET", a.wf("?q=flow-3"), a.Token, nil).JSON(t, &body)
	if len(body.Items) != 1 {
		t.Fatalf("search returned %d", len(body.Items))
	}
}

func TestPublishVersioning(t *testing.T) {
	h := newHarness(t)
	a := h.register("p@example.com")
	var wf wfstore.Workflow
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "f", "graph": simpleGraph}).JSON(t, &wf)

	r := h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, map[string]any{"notes": "first"})
	if r.Status != 201 {
		t.Fatalf("publish: %d %s", r.Status, r.Body)
	}
	var v1 wfstore.Version
	r.JSON(t, &v1)
	if v1.Version != 1 {
		t.Fatalf("v1 = %d", v1.Version)
	}
	// unchanged draft: idempotent, same version
	r = h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, nil)
	var again wfstore.Version
	r.JSON(t, &again)
	if r.Status != 200 || again.ID != v1.ID {
		t.Fatalf("republish unchanged: %d %+v", r.Status, again)
	}
	// edit draft then publish -> v2, v1 graph untouched
	g2 := map[string]any{
		"nodes": append(simpleGraph["nodes"].([]map[string]any), map[string]any{"id": "log2", "type": "log", "config": map[string]any{"message": "two"}}),
		"edges": append(simpleGraph["edges"].([]map[string]any), map[string]any{"source": "log", "target": "log2"}),
	}
	if r := h.do("PUT", a.wf("/"+wf.ID), a.Token, map[string]any{"graph": g2}); r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	r = h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, nil)
	var v2 wfstore.Version
	r.JSON(t, &v2)
	if r.Status != 201 || v2.Version != 2 {
		t.Fatalf("publish v2: %d %+v", r.Status, v2)
	}
	var got wfstore.Version
	h.do("GET", a.wf("/"+wf.ID+"/versions/1"), a.Token, nil).JSON(t, &got)
	if len(got.Graph.Nodes) != 2 {
		t.Fatalf("v1 graph mutated: %d nodes", len(got.Graph.Nodes))
	}
	var list struct{ Items []wfstore.Version }
	h.do("GET", a.wf("/"+wf.ID+"/versions"), a.Token, nil).JSON(t, &list)
	if len(list.Items) != 2 || list.Items[0].Version != 2 {
		t.Fatalf("versions list %+v", list.Items)
	}
	if r := h.do("GET", a.wf("/"+wf.ID+"/versions/99"), a.Token, nil); r.Status != 404 {
		t.Fatalf("missing version got %d", r.Status)
	}
	var cur wfstore.Workflow
	h.do("GET", a.wf("/"+wf.ID), a.Token, nil).JSON(t, &cur)
	if cur.Status != "active" || cur.PublishedVersion == nil || *cur.PublishedVersion != 2 {
		t.Fatalf("current %+v", cur)
	}
	if r := h.do("POST", a.wf("/"+wf.ID+"/unpublish"), a.Token, nil); r.Status != 204 {
		t.Fatalf("unpublish got %d", r.Status)
	}
}

func TestPublishRejectsInvalidGraph(t *testing.T) {
	h := newHarness(t)
	a := h.register("i@example.com")
	bad := map[string]any{
		"nodes": []map[string]any{{"id": "a", "type": "log", "config": map[string]any{"message": "x"}}},
		"edges": []map[string]any{},
	}
	var wf wfstore.Workflow
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "bad", "graph": bad}).JSON(t, &wf)
	r := h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, nil)
	if r.Status != 422 {
		t.Fatalf("publish invalid: %d %s", r.Status, r.Body)
	}
	var vr struct {
		Valid  bool
		Issues []struct{ Code string }
	}
	h.do("POST", a.wf("/"+wf.ID+"/validate"), a.Token, nil).JSON(t, &vr)
	if vr.Valid || len(vr.Issues) == 0 {
		t.Fatalf("validate should flag issues: %+v", vr)
	}
	// validating a supplied graph
	h.do("POST", a.wf("/"+wf.ID+"/validate"), a.Token, map[string]any{"graph": simpleGraph}).JSON(t, &vr)
	if !vr.Valid {
		t.Fatalf("supplied valid graph rejected: %+v", vr)
	}
}

func TestVersionRowsAreImmutable(t *testing.T) {
	h := newHarness(t)
	a := h.register("m@example.com")
	var wf wfstore.Workflow
	h.do("POST", a.wf(""), a.Token, map[string]any{"name": "f", "graph": simpleGraph}).JSON(t, &wf)
	h.do("POST", a.wf("/"+wf.ID+"/publish"), a.Token, nil)
	_, err := h.DB.Pool.Exec(t.Context(), `UPDATE workflow_versions SET graph='{"nodes":[],"edges":[]}' WHERE workflow_id=$1`, wf.ID)
	if err == nil {
		t.Fatal("update of a published version must be rejected by the database")
	}
}

func TestAuthorizationMatrixAndIsolation(t *testing.T) {
	h := newHarness(t)
	owner := h.register("owner@example.com")
	other := h.register("other@example.com")
	viewer := h.register("viewer@example.com")
	member := h.register("member@example.com")

	if r := h.do("POST", fmt.Sprintf("/api/v1/workspaces/%s/members", owner.WorkspaceID), owner.Token, map[string]any{"email": viewer.Email, "role": "viewer"}); r.Status != 204 {
		t.Fatalf("add viewer: %d %s", r.Status, r.Body)
	}
	if r := h.do("POST", fmt.Sprintf("/api/v1/workspaces/%s/members", owner.WorkspaceID), owner.Token, map[string]any{"email": member.Email, "role": "member"}); r.Status != 204 {
		t.Fatalf("add member: %d", r.Status)
	}
	if r := h.do("POST", fmt.Sprintf("/api/v1/workspaces/%s/members", owner.WorkspaceID), owner.Token, map[string]any{"email": member.Email, "role": "owner"}); r.Status != 422 {
		t.Fatalf("granting owner got %d", r.Status)
	}
	var wf wfstore.Workflow
	h.do("POST", owner.wf(""), owner.Token, map[string]any{"name": "secret", "graph": simpleGraph}).JSON(t, &wf)
	path := owner.wf("/" + wf.ID)

	cases := []struct {
		name   string
		method string
		path   string
		token  string
		body   any
		want   int
	}{
		{"anon read", "GET", path, "", nil, 401},
		{"outsider read is 404", "GET", path, other.Token, nil, 404},
		{"outsider list is 404", "GET", owner.wf(""), other.Token, nil, 404},
		{"outsider write is 404", "PUT", path, other.Token, map[string]any{"name": "pwn"}, 404},
		{"outsider delete is 404", "DELETE", path, other.Token, nil, 404},
		{"viewer read", "GET", path, viewer.Token, nil, 200},
		{"viewer write forbidden", "PUT", path, viewer.Token, map[string]any{"name": "x"}, 403},
		{"viewer create forbidden", "POST", owner.wf(""), viewer.Token, map[string]any{"name": "x"}, 403},
		{"viewer publish forbidden", "POST", path + "/publish", viewer.Token, nil, 403},
		{"viewer delete forbidden", "DELETE", path, viewer.Token, nil, 403},
		{"member write", "PUT", path, member.Token, map[string]any{"name": "by member"}, 200},
		{"member cannot add members", "POST", fmt.Sprintf("/api/v1/workspaces/%s/members", owner.WorkspaceID), member.Token, map[string]any{"email": other.Email, "role": "viewer"}, 403},
		{"bad workspace id", "GET", "/api/v1/workspaces/not-a-uuid/workflows", owner.Token, nil, 404},
		{"bad workflow id", "GET", owner.wf("/nope"), owner.Token, nil, 404},
	}
	for _, c := range cases {
		if r := h.do(c.method, c.path, c.token, c.body); r.Status != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, r.Status, c.want, r.Body)
		}
	}
	// workflow of workspace A is invisible through workspace B's own path (IDOR)
	if r := h.do("GET", other.wf("/"+wf.ID), other.Token, nil); r.Status != 404 {
		t.Fatalf("cross-workspace workflow read got %d", r.Status)
	}
	// the outsider's attempted rename must not have applied
	var cur wfstore.Workflow
	h.do("GET", path, owner.Token, nil).JSON(t, &cur)
	if cur.Name != "by member" {
		t.Fatalf("name = %q", cur.Name)
	}
}

func TestNodeTypesEndpoint(t *testing.T) {
	h := newHarness(t)
	a := h.register("n@example.com")
	var body struct {
		NodeTypes []struct{ Type string } `json:"node_types"`
	}
	h.do("GET", "/api/v1/node-types", a.Token, nil).JSON(t, &body)
	if len(body.NodeTypes) != 13 {
		t.Fatalf("got %d node types", len(body.NodeTypes))
	}
	if r := h.do("GET", "/api/v1/node-types", "", nil); r.Status != 401 {
		t.Fatalf("anon got %d", r.Status)
	}
}
