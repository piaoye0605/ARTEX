package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// This file pins the task-asset association routes in task_assets.go:
//   - every handler answers 404 before touching the store when the task id
//     does not resolve (including a store-less Manager)
//   - POST /api/tasks/{id}/assets accepts either asset_ids or scope, never both
//   - the attach mutation reports requested/attached/existing and is idempotent
//   - intent-assets reads anchors written through AddIntent(asset ids)
//   - DELETE detaches only the association; a second call is a 404

func taskAssetCall(t *testing.T, s *Server, method, target, id, assetID, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, target, reader)
	req.SetPathValue("id", id)
	if assetID != "" {
		req.SetPathValue("assetID", assetID)
	}
	rec := httptest.NewRecorder()
	switch method {
	case http.MethodPost:
		s.attachTaskAssets(rec, req)
	case http.MethodDelete:
		s.detachTaskAsset(rec, req)
	default:
		s.taskIntentAssets(rec, req)
	}
	return rec
}

func TestTaskAssetRoutesUnknownTask(t *testing.T) {
	// A store-less Manager resolves no task, so all three handlers must answer
	// 404 without panicking on the nil stores.
	s := &Server{m: &Manager{}}
	checks := []struct {
		name   string
		method string
		call   func(http.ResponseWriter, *http.Request)
	}{
		{"attach", http.MethodPost, s.attachTaskAssets},
		{"detach", http.MethodDelete, s.detachTaskAsset},
		{"intents", http.MethodGet, s.taskIntentAssets},
	}
	for _, c := range checks {
		req := httptest.NewRequest(c.method, "/api/tasks/1/assets", bytes.NewReader([]byte(`{}`)))
		req.SetPathValue("id", "1")
		if c.name == "detach" {
			req.SetPathValue("assetID", "1")
		}
		rec := httptest.NewRecorder()
		c.call(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without a store status=%d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}

	// With a live store the same guard fires for a task id that never existed.
	_, s = newAssetTestServer(t)
	if rec := taskAssetCall(t, s, http.MethodPost, "/api/tasks/999999999/assets", "999999999", "", `{"asset_ids":[1]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task attach status=%d", rec.Code)
	}
	if rec := taskAssetCall(t, s, http.MethodGet, "/api/tasks/999999999/intent-assets", "999999999", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task intents status=%d", rec.Code)
	}
}

func TestAttachTaskAssetsLifecycle(t *testing.T) {
	m, s := newAssetTestServer(t)
	stamp := time.Now().UnixNano()
	as := m.pg.Assets()

	task, err := m.CreateTask(fmt.Sprintf("task assets %d", stamp), "task assets goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	pathID := i64s(taskID)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	domains := []string{
		fmt.Sprintf("ta-a-%d.example.com", stamp),
		fmt.Sprintf("ta-b-%d.example.com", stamp),
	}
	ids := make([]int64, 0, len(domains))
	for _, d := range domains {
		id, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: d})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() { _, _ = as.DeleteByIDs(ids) })

	target := "/api/tasks/" + pathID + "/assets"
	body := fmt.Sprintf(`{"asset_ids":[%d,%d],"source_summary":"  manual probe  "}`, ids[0], ids[1])
	rec := taskAssetCall(t, s, http.MethodPost, target, pathID, "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", rec.Code, rec.Body.String())
	}
	var mutation db.TaskAssetMutation
	assetsDecode(t, rec, &mutation)
	if mutation.Requested != 2 || mutation.Attached != 2 || mutation.Existing != 0 {
		t.Fatalf("first attach=%+v", mutation)
	}

	// Attaching the same set again counts everything as existing.
	assetsDecode(t, taskAssetCall(t, s, http.MethodPost, target, pathID, "", body), &mutation)
	if mutation.Attached != 0 || mutation.Existing != 2 {
		t.Fatalf("repeat attach=%+v", mutation)
	}

	// Duplicate ids in one request are de-duplicated before counting. The
	// upsert also overwrites the provenance: the last attach wins.
	dupBody := fmt.Sprintf(`{"asset_ids":[%d,%d],"source_summary":"dup"}`, ids[0], ids[0])
	assetsDecode(t, taskAssetCall(t, s, http.MethodPost, target, pathID, "", dupBody), &mutation)
	if mutation.Requested != 1 || mutation.Existing != 1 {
		t.Fatalf("dedup attach=%+v", mutation)
	}

	for name, badBody := range map[string]string{
		"empty ids":       `{"asset_ids":[],"source_summary":"x"}`,
		"zero id":         `{"asset_ids":[0],"source_summary":"x"}`,
		"negative id":     `{"asset_ids":[-3],"source_summary":"x"}`,
		"missing summary": fmt.Sprintf(`{"asset_ids":[%d]}`, ids[0]),
		"oversize summary": fmt.Sprintf(`{"asset_ids":[%d],"source_summary":"%s"}`,
			ids[0], strings.Repeat("长", db.MaxTaskAssetSummaryRunes+1)),
		"scope and ids": fmt.Sprintf(`{"asset_ids":[%d],"scope":["x-%d.example.com"]}`, ids[0], stamp),
	} {
		if rec := taskAssetCall(t, s, http.MethodPost, target, pathID, "", badBody); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}

	// A request over the 512 KiB body cap is a 413, not a 400.
	huge := `{"source_summary":"` + strings.Repeat("a", 600<<10) + `"}`
	if rec := taskAssetCall(t, s, http.MethodPost, target, pathID, "", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body status=%d", rec.Code)
	}

	// One unknown asset fails the whole request with a 404.
	missing := fmt.Sprintf(`{"asset_ids":[%d,999999999],"source_summary":"x"}`, ids[0])
	if rec := taskAssetCall(t, s, http.MethodPost, target, pathID, "", missing); rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status=%d body=%s", rec.Code, rec.Body.String())
	}

	// intent-assets starts empty, then surfaces the anchored intent with the
	// manual provenance recorded by the attach call above.
	rec = taskAssetCall(t, s, http.MethodGet, "/api/tasks/"+pathID+"/intent-assets", pathID, "", "")
	var empty struct {
		Assets []db.IntentAsset `json:"assets"`
	}
	assetsDecode(t, rec, &empty)
	if len(empty.Assets) != 0 {
		t.Fatalf("unexpected intent assets=%+v", empty.Assets)
	}
	if _, err := task.Store.AddIntent(map[string]any{"summary": "anchor probe"}, 0, []int64{ids[0]}, "planner"); err != nil {
		t.Fatal(err)
	}
	rec = taskAssetCall(t, s, http.MethodGet, "/api/tasks/"+pathID+"/intent-assets", pathID, "", "")
	assetsDecode(t, rec, &empty)
	if len(empty.Assets) != 1 {
		t.Fatalf("intent assets=%+v", empty.Assets)
	}
	got := empty.Assets[0]
	if got.AssetID != ids[0] || got.Type != "root_domain" || got.Label != domains[0] ||
		got.Source != "manual" || got.SourceSummary != "dup" || got.Inherited {
		t.Fatalf("intent asset=%+v", got)
	}

	// Detach: success, then a second attempt is a 404, and bad ids are 400s.
	del := "/api/tasks/" + pathID + "/assets"
	rec = taskAssetCall(t, s, http.MethodDelete, del+"/"+i64s(ids[0]), pathID, i64s(ids[0]), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detach status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detached struct {
		Detached int64 `json:"detached"`
	}
	assetsDecode(t, rec, &detached)
	if detached.Detached != ids[0] {
		t.Fatalf("detach response=%+v", detached)
	}
	if rec := taskAssetCall(t, s, http.MethodDelete, del+"/"+i64s(ids[0]), pathID, i64s(ids[0]), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("double detach status=%d", rec.Code)
	}
	for _, bad := range []string{"abc", "0", "-1"} {
		if rec := taskAssetCall(t, s, http.MethodDelete, del+"/"+bad, pathID, bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad asset id %q status=%d", bad, rec.Code)
		}
	}
}

func TestAttachTaskAssetsScopes(t *testing.T) {
	m, s := newAssetTestServer(t)
	stamp := time.Now().UnixNano()

	task, err := m.CreateTask(fmt.Sprintf("task scope %d", stamp), "task scope goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	pathID := i64s(taskID)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	target := "/api/tasks/" + pathID + "/assets"
	domain := fmt.Sprintf("task-scope-%d.example.com", stamp)
	ip := fmt.Sprintf("203.0.113.%d", stamp%200+1)
	created := make([]int64, 0, 2)
	t.Cleanup(func() {
		if len(created) > 0 {
			_, _ = m.pg.Assets().DeleteByIDs(created)
		}
	})

	// A domain scope creates (or reuses) a global root-domain asset and links it.
	rec := taskAssetCall(t, s, http.MethodPost, target, pathID, "", fmt.Sprintf(`{"scope":["%s"]}`, domain))
	if rec.Code != http.StatusOK {
		t.Fatalf("scope attach status=%d body=%s", rec.Code, rec.Body.String())
	}
	var mutation db.TaskAssetScopeMutation
	assetsDecode(t, rec, &mutation)
	if mutation.Requested != 1 || mutation.AssetsLinked != 1 || mutation.ScopesAdded != 1 {
		t.Fatalf("first scope=%+v", mutation)
	}
	var assetID int64
	if err := m.pg.QueryRow(`SELECT id FROM assets WHERE type='root_domain' AND domain=$1`, domain).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	created = append(created, assetID)

	// Re-submitting the same scope is fully idempotent.
	assetsDecode(t, taskAssetCall(t, s, http.MethodPost, target, pathID, "", fmt.Sprintf(`{"scope":["%s"]}`, domain)), &mutation)
	if mutation.AssetsLinked != 0 || mutation.AssetsExisting != 1 || mutation.ScopesAdded != 0 || mutation.ScopesExisting != 1 {
		t.Fatalf("repeat scope=%+v", mutation)
	}

	// An IP scope goes through the same path and normalises the address.
	assetsDecode(t, taskAssetCall(t, s, http.MethodPost, target, pathID, "", fmt.Sprintf(`{"scope":["%s"]}`, ip)), &mutation)
	if mutation.AssetsLinked != 1 {
		t.Fatalf("ip scope=%+v", mutation)
	}
	var ipAssetID int64
	if err := m.pg.QueryRow(`SELECT id FROM assets WHERE type='ip' AND ip=$1`, ip).Scan(&ipAssetID); err != nil {
		t.Fatal(err)
	}
	created = append(created, ipAssetID)

	for name, badBody := range map[string]string{
		"empty scope": `{"scope":[]}`,
		"bad rule":    `{"scope":[{"kind":"telepathy","value":"x"}]}`,
		"bad json":    `{"scope":`,
	} {
		if rec := taskAssetCall(t, s, http.MethodPost, target, pathID, "", badBody); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
}
