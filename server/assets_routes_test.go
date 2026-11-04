package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// This file pins the company/asset control plane in assets.go. The scope rules
// themselves are covered in assets_scope_test.go (error classification, bounds,
// duplicate handling); here the subject is the route surface users actually
// call: which filter wins, what the page envelope looks like, and how each asset
// type is dispatched by POST /api/assets.
//
// Contracts pinned here:
//   - every handler answers 503 when the database store is absent
//   - POST /api/assets is per-item: one bad entry never fails the batch, it is
//     reported in `errors` with its index while the rest are inserted
//   - list filters are exclusive: dsl > company_id > task_id > type
//   - pagination clamps silently (limit 0 → 50, >200 → 200, offset < 0 → 0) and
//     an offset past the total returns an empty page without a query

func newAssetTestServer(t *testing.T) (*Manager, *Server) {
	t.Helper()
	m, s := newTestStack(t)
	return m, s
}

func assetsDecode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

func companyMutation(t *testing.T, s *Server, method, target, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, target, reader)
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodGet && id == "":
		s.listCompanies(rec, req)
	case method == http.MethodPost && id == "":
		s.createCompany(rec, req)
	case method == http.MethodGet:
		s.getCompany(rec, req)
	case method == http.MethodPost:
		s.addCompanyScope(rec, req)
	default:
		s.deleteCompany(rec, req)
	}
	return rec
}

func TestCompanyAndAssetRoutesRequireDatabase(t *testing.T) {
	// A Manager without a store is what the server sees before PG is reachable.
	s := &Server{m: &Manager{}}
	for _, target := range []string{"/api/companies", "/api/companies/1", "/api/companies/1/scope", "/api/assets", "/api/assets/counts"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.SetPathValue("id", "1")
		rec := httptest.NewRecorder()
		s.listCompanies(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("listCompanies %s status=%d", target, rec.Code)
		}
	}
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"createCompany":   s.createCompany,
		"getCompany":      s.getCompany,
		"addCompanyScope": s.addCompanyScope,
		"deleteCompany":   s.deleteCompany,
		"reattribute":     s.reattribute,
		"listAssets":      s.listAssets,
		"assetCounts":     s.assetCounts,
		"deleteAssets":    s.deleteAssets,
		"insertAssets":    s.insertAssets,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/x", bytes.NewReader([]byte(`{}`)))
		req.SetPathValue("id", "1")
		rec := httptest.NewRecorder()
		call(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without a store status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestCompanyRouteLifecycle(t *testing.T) {
	m, s := newAssetTestServer(t)
	stamp := time.Now().UnixNano()
	domain := fmt.Sprintf("company-%d.example.com", stamp)

	rec := companyMutation(t, s, http.MethodPost, "/api/companies", "", fmt.Sprintf(
		`{"name":"  Route Company %d  ","logo":"logo.png","scope":["%s","%s",{"kind":"ip","value":"203.0.113.%d"}]}`,
		stamp, domain, domain, stamp%200+1,
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID           int64 `json:"id"`
		Created      bool  `json:"created"`
		ScopeAdded   int   `json:"scope_added"`
		ScopeSkipped int   `json:"scope_skipped"`
	}
	assetsDecode(t, rec, &created)
	if created.ID == 0 || !created.Created {
		t.Fatalf("create response=%+v", created)
	}
	if created.ScopeSkipped != 1 {
		t.Fatalf("duplicate legacy scope rule was not skipped: %+v", created)
	}
	t.Cleanup(func() { _ = m.pg.Companies().DeleteCompany(created.ID) })

	// The list route returns the store rows as a bare JSON array (no envelope).
	rec = companyMutation(t, s, http.MethodGet, "/api/companies", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed []map[string]any
	assetsDecode(t, rec, &listed)
	if len(listed) == 0 {
		t.Fatal("list returned no companies")
	}

	id := i64s(created.ID)
	rec = companyMutation(t, s, http.MethodGet, "/api/companies/"+id, id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Company map[string]any   `json:"company"`
		Scope   []map[string]any `json:"scope"`
	}
	assetsDecode(t, rec, &detail)
	if detail.Company == nil || len(detail.Scope) != 2 {
		t.Fatalf("detail=%+v", detail)
	}

	for _, badID := range []string{"abc", "1.5"} {
		if rec := companyMutation(t, s, http.MethodGet, "/api/companies/"+badID, badID, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad id %q status=%d", badID, rec.Code)
		}
	}
	if rec := companyMutation(t, s, http.MethodGet, "/api/companies/999999999", "999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing company status=%d", rec.Code)
	}

	// Add a rule, then re-add it: the duplicate is counted, not an error.
	extra := fmt.Sprintf("extra-%d.example.com", stamp)
	rec = companyMutation(t, s, http.MethodPost, "/api/companies/"+id+"/scope", id, fmt.Sprintf(`{"scope":["%s"],"reason":"probe"}`, extra))
	if rec.Code != http.StatusOK {
		t.Fatalf("add scope status=%d body=%s", rec.Code, rec.Body.String())
	}
	var added struct {
		Added   int      `json:"added"`
		Skipped int      `json:"skipped"`
		Invalid int      `json:"invalid"`
		Errors  []string `json:"errors"`
	}
	assetsDecode(t, rec, &added)
	if added.Added != 1 {
		t.Fatalf("add scope response=%+v", added)
	}
	assetsDecode(t, companyMutation(t, s, http.MethodPost, "/api/companies/"+id+"/scope", id, fmt.Sprintf(`{"scope":["%s"]}`, extra)), &added)
	if added.Skipped != 1 || added.Added != 0 {
		t.Fatalf("duplicate add response=%+v", added)
	}

	// Unknown kind is reported per rule instead of failing the request.
	rec = companyMutation(t, s, http.MethodPost, "/api/companies/"+id+"/scope", id, `{"scope":[{"kind":"telepathy","value":"x"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown kind status=%d body=%s", rec.Code, rec.Body.String())
	}
	assetsDecode(t, rec, &added)
	if added.Invalid != 1 || len(added.Errors) == 0 {
		t.Fatalf("unknown kind response=%+v", added)
	}

	// reset replaces the whole scope instead of appending.
	replacement := fmt.Sprintf("replacement-%d.example.com", stamp)
	rec = companyMutation(t, s, http.MethodPost, "/api/companies/"+id+"/scope", id, fmt.Sprintf(`{"scope":["%s"],"reset":true}`, replacement))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset scope status=%d body=%s", rec.Code, rec.Body.String())
	}
	scope, err := m.pg.Companies().GetScope(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) != 1 || scope[0].Domain != replacement {
		t.Fatalf("reset scope=%+v", scope)
	}

	// Attribution rebuild is a global pass but must stay a plain 200.
	rec = httptest.NewRecorder()
	s.reattribute(rec, httptest.NewRequest(http.MethodPost, "/api/companies/reattribute", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reattribute status=%d body=%s", rec.Code, rec.Body.String())
	}

	// An asset owned by the company is deleted with it when asked.
	ownedDomain := fmt.Sprintf("owned-%d.example.com", stamp)
	var assetID int64
	if err := m.pg.QueryRow(`INSERT INTO assets(type,domain,company_id,company_source) VALUES ('root_domain',$1,$2,'explicit') RETURNING id`,
		ownedDomain, created.ID).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Assets().DeleteByIDs([]int64{assetID}) })
	rec = companyMutation(t, s, http.MethodDelete, "/api/companies/"+id, id, `{"delete_assets":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete company status=%d body=%s", rec.Code, rec.Body.String())
	}
	var deleted struct {
		Deleted       int `json:"deleted"`
		AssetsDeleted int `json:"assets_deleted"`
	}
	assetsDecode(t, rec, &deleted)
	if deleted.Deleted != 1 || deleted.AssetsDeleted == 0 {
		t.Fatalf("delete company response=%+v", deleted)
	}
	if rec := companyMutation(t, s, http.MethodDelete, "/api/companies/"+id, id, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("double delete status=%d", rec.Code)
	}
}

func TestCompanyRouteBodyGuards(t *testing.T) {
	_, s := newAssetTestServer(t)
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`, `{"scope":123}`, `{"name":`} {
		if rec := companyMutation(t, s, http.MethodPost, "/api/companies", "", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
	// Both a non-numeric and an empty path id are client errors, never a panic.
	for _, badID := range []string{"abc", "", "1.5"} {
		if rec := companyMutation(t, s, http.MethodPost, "/api/companies/x/scope", badID, `{"scope":[]}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("scope bad id %q status=%d", badID, rec.Code)
		}
		if rec := companyMutation(t, s, http.MethodDelete, "/api/companies/x", badID, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("delete bad id %q status=%d", badID, rec.Code)
		}
	}
}

// insertAssetFixture posts one batch and returns the decoded envelope.
type insertAssetEnvelope struct {
	Results []struct {
		Index int    `json:"index"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"results"`
	Errors []struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	} `json:"errors"`
}

func postAssets(t *testing.T, s *Server, body string) (int, insertAssetEnvelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/assets", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	s.insertAssets(rec, req)
	var envelope insertAssetEnvelope
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	}
	return rec.Code, envelope
}

func TestInsertAssetsDispatchesEveryType(t *testing.T) {
	m, s := newAssetTestServer(t)
	stamp := time.Now().UnixNano()
	task, err := m.CreateTask(fmt.Sprintf("asset api %d", stamp), "asset api goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	domain := fmt.Sprintf("batch-%d.example.com", stamp)
	sub := fmt.Sprintf("www.batch-%d.example.com", stamp)
	ip := fmt.Sprintf("203.0.113.%d", stamp%200+1)
	svcURL := fmt.Sprintf("https://batch-%d.example.com/", stamp)
	otherPort := 8000 + int(stamp%1000)
	endpointURL := fmt.Sprintf("%sapi/%d", svcURL, stamp)
	body := fmt.Sprintf(`{"task_id":%d,"assets":[
		{"type":"root_domain","domain":"%s","icp":"京ICP备%d号"},
		{"type":"subdomain","domain":"%s","record_type":"A","record_value":["%s"]},
		{"type":"ip","ip":"%s","bound_domains":["%s"],"open_ports":[{"port":443,"service":"https"}]},
		{"type":"app","app_name":"Batch App %d","bundle_id":"com.batch.%d","category":"tools"},
		{"type":"service","url":"%s","technologies":["nginx"],"status_code":200,"page_title":"batch"},
		{"type":"service","domain":"%s","port":%d,"service_name":"batch-ssh"},
		{"type":"endpoint","url":"%s","method":"GET","params":[{"name":"q"}]},
		{"type":"telepathy"}
	]}`, taskID, domain, stamp, sub, ip, ip, domain, stamp, stamp, svcURL, domain, otherPort, endpointURL)

	code, envelope := postAssets(t, s, body)
	if code != http.StatusOK {
		t.Fatalf("insert assets status=%d", code)
	}
	t.Cleanup(func() {
		ids := make([]int64, 0, len(envelope.Results))
		for _, r := range envelope.Results {
			ids = append(ids, r.ID)
		}
		_, _ = m.pg.Assets().DeleteByIDs(ids)
	})
	if len(envelope.Results) != 7 {
		t.Fatalf("inserted %d assets, want 7: %+v", len(envelope.Results), envelope)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Index != 7 {
		t.Fatalf("unknown type was not reported per item: %+v", envelope.Errors)
	}
	for _, result := range envelope.Results {
		if result.ID == 0 {
			t.Fatalf("asset %q has no id: %+v", result.Type, result)
		}
	}

	// Every inserted asset is reachable through the type filter of GET /api/assets.
	req := httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"type": {"root_domain"}, "limit": {"200"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	s.listAssets(rec, req)
	var page struct {
		Count  int              `json:"count"`
		Total  int              `json:"total"`
		Assets []map[string]any `json:"assets"`
	}
	assetsDecode(t, rec, &page)
	if page.Total < 1 || page.Count != len(page.Assets) {
		t.Fatalf("root domain page=%+v", page)
	}

	// A per-item failure is reported with its index, not escalated: an invalid IP
	// fails validation inside the store while the neighbouring entry still lands.
	code, envelope = postAssets(t, s, fmt.Sprintf(`{"assets":[{"type":"ip","ip":"not-an-ip"},{"type":"root_domain","domain":"%s"}]}`, fmt.Sprintf("mixed-%d.example.com", stamp)))
	if code != http.StatusOK {
		t.Fatalf("mixed batch status=%d", code)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Index != 0 || len(envelope.Results) != 1 || envelope.Results[0].Index != 1 {
		t.Fatalf("mixed batch envelope=%+v", envelope)
	}
	t.Cleanup(func() {
		for _, result := range envelope.Results {
			_, _ = m.pg.Assets().DeleteByIDs([]int64{result.ID})
		}
	})
}

func TestListAssetsFiltersAndPaging(t *testing.T) {
	m, s := newAssetTestServer(t)
	stamp := time.Now().UnixNano()
	as := m.pg.Assets()

	// One company-owned asset, one task-owned asset, both root domains so the
	// default type filter also sees them.
	companyID, _, err := m.pg.Companies().UpsertCompany(fmt.Sprintf("Filter Company %d", stamp), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.Companies().DeleteCompany(companyID) })
	companyDomain := fmt.Sprintf("filter-company-%d.example.com", stamp)
	var companyAssetID int64
	if err := m.pg.QueryRow(`INSERT INTO assets(type,domain,company_id,company_source) VALUES ('root_domain',$1,$2,'explicit') RETURNING id`,
		companyDomain, companyID).Scan(&companyAssetID); err != nil {
		t.Fatal(err)
	}

	task, err := m.CreateTask(fmt.Sprintf("filter task %d", stamp), "filter goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	taskDomain := fmt.Sprintf("filter-task-%d.example.com", stamp)
	taskAssetID, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: taskDomain, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = as.DeleteByIDs([]int64{companyAssetID, taskAssetID}) })

	// company_id filter wins over type and only counts that company's assets.
	req := httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"company_id": {i64s(companyID)}}.Encode(), nil)
	rec := httptest.NewRecorder()
	s.listAssets(rec, req)
	var page struct {
		Count  int              `json:"count"`
		Total  int              `json:"total"`
		Assets []map[string]any `json:"assets"`
	}
	assetsDecode(t, rec, &page)
	if page.Total < 1 || page.Count == 0 {
		t.Fatalf("company filter page=%+v", page)
	}
	found := false
	for _, asset := range page.Assets {
		if asset["domain"] == companyDomain {
			found = true
		}
	}
	if !found {
		t.Fatalf("company filter did not return %s: %+v", companyDomain, page.Assets)
	}

	// task_id filter scopes to the task's assets.
	req = httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"task_id": {i64s(taskID)}}.Encode(), nil)
	rec = httptest.NewRecorder()
	s.listAssets(rec, req)
	assetsDecode(t, rec, &page)
	if page.Total != 1 || len(page.Assets) != 1 || page.Assets[0]["domain"] != taskDomain {
		t.Fatalf("task filter page=%+v", page)
	}

	// dsl search with an explicit type.
	req = httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"dsl": {"domain=" + taskDomain}}.Encode(), nil)
	rec = httptest.NewRecorder()
	s.listAssets(rec, req)
	assetsDecode(t, rec, &page)
	if page.Total != 1 || page.Assets[0]["domain"] != taskDomain {
		t.Fatalf("dsl page=%+v", page)
	}

	// dsl + task_id narrows to the task's assets only.
	req = httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"dsl": {"domain=" + taskDomain}, "task_id": {i64s(taskID)}}.Encode(), nil)
	rec = httptest.NewRecorder()
	s.listAssets(rec, req)
	assetsDecode(t, rec, &page)
	if page.Total != 1 {
		t.Fatalf("dsl scoped page=%+v", page)
	}

	// offset past the total returns an empty page, still with the true total.
	req = httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"task_id": {i64s(taskID)}, "offset": {"50"}}.Encode(), nil)
	rec = httptest.NewRecorder()
	s.listAssets(rec, req)
	assetsDecode(t, rec, &page)
	if page.Total != 1 || page.Count != 0 || len(page.Assets) != 0 {
		t.Fatalf("offset past total page=%+v", page)
	}

	// limit is clamped silently: 5000 becomes 200 and still returns the row.
	req = httptest.NewRequest(http.MethodGet, "/api/assets?"+url.Values{"task_id": {i64s(taskID)}, "limit": {"5000"}}.Encode(), nil)
	rec = httptest.NewRecorder()
	s.listAssets(rec, req)
	assetsDecode(t, rec, &page)
	if page.Count != 1 {
		t.Fatalf("clamped limit page=%+v", page)
	}

	// counts endpoint: global and per task.
	rec = httptest.NewRecorder()
	s.assetCounts(rec, httptest.NewRequest(http.MethodGet, "/api/assets/counts", nil))
	var counts map[string]int
	assetsDecode(t, rec, &counts)
	if counts["root_domain"] < 2 {
		t.Fatalf("global counts=%+v", counts)
	}
	rec = httptest.NewRecorder()
	s.assetCounts(rec, httptest.NewRequest(http.MethodGet, "/api/assets/counts?task_id="+i64s(taskID), nil))
	counts = nil
	assetsDecode(t, rec, &counts)
	if counts["root_domain"] != 1 {
		t.Fatalf("task counts=%+v", counts)
	}
}

func TestDeleteAssetsRoutes(t *testing.T) {
	m, s := newAssetTestServer(t)
	stamp := time.Now().UnixNano()
	as := m.pg.Assets()
	ids := make([]int64, 0, 2)
	for i := 0; i < 2; i++ {
		id, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("delete-%d-%d.example.com", stamp, i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	body, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.deleteAssets(rec, httptest.NewRequest(http.MethodDelete, "/api/assets", bytes.NewReader(body)))
	var deleted struct {
		Deleted int64 `json:"deleted"`
	}
	assetsDecode(t, rec, &deleted)
	if rec.Code != http.StatusOK || deleted.Deleted != 2 {
		t.Fatalf("delete assets status=%d response=%+v", rec.Code, deleted)
	}

	for _, body := range []string{`{"ids":[]}`, `{}`, `{"ids":"x"}`} {
		rec := httptest.NewRecorder()
		s.deleteAssets(rec, httptest.NewRequest(http.MethodDelete, "/api/assets", bytes.NewReader([]byte(body))))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d", body, rec.Code)
		}
	}
}
