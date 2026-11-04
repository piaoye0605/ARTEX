package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Coverage for sync_scopesentry.go — the ScopeSentry (external ASM platform)
// asset-sync surface: datasource discovery/status, project & task listing, the
// bulk sync runner, the paging helper and the per-type ingest mapping. The
// remote side is a local JSON-RPC mock (Streamable HTTP), so the tests exercise
// the real mcphttp client handshake instead of stubbing it out.

// --- mock MCP server ---

// callHandler returns the text block and isError flag for one tools/call.
type callHandler func(tool string, args map[string]any) (string, bool)

// newMCPMock starts a JSON-RPC MCP endpoint that answers the initialize
// handshake, exposes one tool and delegates tools/call to handle.
func newMCPMock(t *testing.T, handle callHandler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete { // session teardown
			w.WriteHeader(http.StatusOK)
			return
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&frame)
		switch frame.Method {
		case "initialize":
			writeRPC(w, frame.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mock-scopesentry", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(w, frame.ID, map[string]any{"tools": []map[string]any{
				{"name": "list_assets", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(frame.Params, &p)
			text, isErr := handle(p.Name, p.Arguments)
			writeRPC(w, frame.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// configureScopeSentry installs the ScopeSentry MCP row pointing at url and
// restores whatever was there before on cleanup (the row is global/shared).
func configureScopeSentry(t *testing.T, m *Manager, url string, enabled bool) {
	t.Helper()
	var prev *db.MCPServer
	if all, err := m.pg.ListMCP(); err == nil {
		for _, row := range all {
			if row.Name == scopeSentryMCPName {
				copy := *row
				prev = &copy
			}
		}
	}
	if prev != nil {
		snapshot := prev
		t.Cleanup(func() { _, _ = m.pg.SaveMCP(snapshot) })
	}
	row := &db.MCPServer{
		Name: scopeSentryMCPName, Transport: "http", URL: url,
		Env: json.RawMessage(`{"X-API-Key":"test-key"}`), Enabled: enabled,
	}
	if prev != nil {
		row.ID = prev.ID
	}
	id, err := m.pg.SaveMCP(row)
	if err != nil {
		t.Fatalf("save scopesentry row: %v", err)
	}
	if prev == nil {
		t.Cleanup(func() { _ = m.pg.DeleteMCP(id) })
	}
}

func TestQueryIntAndEnvHasValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/sync/scopesentry/projects?page=3&size=abc&empty=", nil)
	if got := queryInt(r, "page", 1); got != 3 {
		t.Fatalf("page=%d want 3", got)
	}
	if got := queryInt(r, "size", 50); got != 50 {
		t.Fatalf("size fallback=%d want 50", got)
	}
	if got := queryInt(r, "missing", 7); got != 7 {
		t.Fatalf("missing=%d want 7", got)
	}
	neg := httptest.NewRequest(http.MethodGet, "/x?n=-2", nil)
	if got := queryInt(neg, "n", 9); got != 9 {
		t.Fatalf("negative=%d want 9", got)
	}

	for _, c := range []struct {
		raw  string
		want bool
	}{
		{`{"X-API-Key":"k"}`, true},
		{`{"X-API-Key":"  "}`, false},
		{`{}`, false},
		{`null`, false},
		{`not-json`, false},
	} {
		if got := envHasValue(json.RawMessage(c.raw)); got != c.want {
			t.Fatalf("envHasValue(%s)=%v want %v", c.raw, got, c.want)
		}
	}
}

func TestScopeSentryClientGuards(t *testing.T) {
	m, s := newTestStack(t)
	pg := m.PG()
	// Remove any leftover row so the "missing" branch is deterministic.
	if all, err := pg.ListMCP(); err == nil {
		for _, row := range all {
			if row.Name == scopeSentryMCPName {
				_ = pg.DeleteMCP(row.ID)
			}
		}
	}
	if _, err := s.scopeSentryClient(t.Context()); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing row error=%v", err)
	}

	// A row without a URL is reported as unconfigured.
	if _, err := pg.SaveMCP(&db.MCPServer{Name: scopeSentryMCPName, Transport: "http", Env: json.RawMessage(`{"X-API-Key":"k"}`)}); err != nil {
		t.Fatalf("save row: %v", err)
	}
	t.Cleanup(func() {
		if all, err := pg.ListMCP(); err == nil {
			for _, row := range all {
				if row.Name == scopeSentryMCPName {
					_ = pg.DeleteMCP(row.ID)
				}
			}
		}
	})
	if _, err := s.scopeSentryClient(t.Context()); err == nil || !strings.Contains(err.Error(), "未配置 URL") {
		t.Fatalf("no-url error=%v", err)
	}

	// A reachable endpoint yields a working client.
	srv := newMCPMock(t, func(string, map[string]any) (string, bool) { return `{"ok":true}`, false })
	configureScopeSentry(t, m, srv.URL, true)
	cl, err := s.scopeSentryClient(t.Context())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cl.Close()
	if text, err := cl.Call(t.Context(), "list_assets", map[string]any{}); err != nil || text != `{"ok":true}` {
		t.Fatalf("call=%q err=%v", text, err)
	}
}

func TestSyncSSStatusRoute(t *testing.T) {
	m, s := newTestStack(t)
	pg := m.PG()
	// Clean slate: no row → exists false, everything else false-y.
	if all, err := pg.ListMCP(); err == nil {
		for _, row := range all {
			if row.Name == scopeSentryMCPName {
				_ = pg.DeleteMCP(row.ID)
			}
		}
	}
	var status map[string]any
	rec := crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	crudJSON(t, rec, &status)
	if status["exists"] != false || status["configured"] != false || status["reachable"] != false {
		t.Fatalf("empty status=%+v", status)
	}

	// Configured + enabled + reachable: the probe handshakes with the endpoint.
	srv := newMCPMock(t, func(string, map[string]any) (string, bool) { return `{}`, false })
	configureScopeSentry(t, m, srv.URL, true)
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/status", ""), &status)
	if status["exists"] != true || status["configured"] != true || status["enabled"] != true || status["reachable"] != true {
		t.Fatalf("configured status=%+v", status)
	}
	if _, ok := status["tools"].([]any); !ok {
		t.Fatalf("tools must serialize as an array: %+v", status)
	}

	// Disabled rows are not probed.
	configureScopeSentry(t, m, srv.URL, false)
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/status", ""), &status)
	if status["reachable"] != false {
		t.Fatalf("disabled row probed: %+v", status)
	}
}

func TestSyncSSDatasourceRoute(t *testing.T) {
	m, s := newTestStack(t)
	pg := m.PG()
	if all, err := pg.ListMCP(); err == nil {
		for _, row := range all {
			if row.Name == scopeSentryMCPName {
				_ = pg.DeleteMCP(row.ID)
			}
		}
	}
	t.Cleanup(func() {
		if all, err := pg.ListMCP(); err == nil {
			for _, row := range all {
				if row.Name == scopeSentryMCPName {
					_ = pg.DeleteMCP(row.ID)
				}
			}
		}
	})

	// Empty body just creates the placeholder, disabled.
	var out struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	rec := crudDo(t, s, http.MethodPost, "/api/sync/scopesentry/datasource", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("placeholder: %d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.ID <= 0 || out.Enabled {
		t.Fatalf("placeholder=%+v", out)
	}

	// URL + key enables it; tool discovery runs against the endpoint.
	srv := newMCPMock(t, func(string, map[string]any) (string, bool) { return `{}`, false })
	rec = crudDo(t, s, http.MethodPost, "/api/sync/scopesentry/datasource",
		`{"url":"`+srv.URL+`","api_key":"k2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("configure: %d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.ID <= 0 || !out.Enabled {
		t.Fatalf("configured=%+v", out)
	}
	// The row is now reachable via the status route.
	var status map[string]any
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/status", ""), &status)
	if status["configured"] != true || status["enabled"] != true {
		t.Fatalf("status after configure=%+v", status)
	}
}

func TestSyncSSProjectsAndTasksRoutes(t *testing.T) {
	m, s := newTestStack(t)
	srv := newMCPMock(t, func(tool string, args map[string]any) (string, bool) {
		switch tool {
		case "list_projects_data":
			return `{"result":{"All":[{"id":"p1","name":"Acme"}],"tagged":[{"id":"p2"}]},"tag":{"All":2}}`, false
		case "list_tasks":
			return `{"list":[{"name":"task-a"},{"name":"task-b"}]}`, false
		}
		return `{}`, false
	})
	configureScopeSentry(t, m, srv.URL, true)

	var projects struct {
		Projects []map[string]any `json:"projects"`
		Tag      map[string]int   `json:"tag"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/projects?page=2&size=10&search=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("projects: %d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &projects)
	if len(projects.Projects) != 1 || projects.Projects[0]["name"] != "Acme" {
		t.Fatalf("projects=%+v", projects.Projects)
	}
	if projects.Tag["All"] != 2 {
		t.Fatalf("tag=%+v", projects.Tag)
	}

	var tasks struct {
		Tasks []map[string]any `json:"tasks"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks: %d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &tasks)
	if len(tasks.Tasks) != 2 || tasks.Tasks[0]["name"] != "task-a" {
		t.Fatalf("tasks=%+v", tasks.Tasks)
	}

	// Remote error and malformed envelopes are 502s with the tool name.
	failing := newMCPMock(t, func(string, map[string]any) (string, bool) { return "boom", true })
	configureScopeSentry(t, m, failing.URL, true)
	if rec := crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/projects", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("failing projects: %d", rec.Code)
	}
	bad := newMCPMock(t, func(string, map[string]any) (string, bool) { return "not-json", false })
	configureScopeSentry(t, m, bad.URL, true)
	if rec := crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/projects", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("malformed projects: %d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/sync/scopesentry/tasks", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("malformed tasks: %d", rec.Code)
	}
}

func TestSyncSSRunValidation(t *testing.T) {
	m, s := newTestStack(t)
	base := "/api/sync/scopesentry/sync"
	if rec := crudDo(t, s, http.MethodPost, base, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rec.Code)
	}
	for _, body := range []string{
		`{"dimension":"galaxy","targets":["x"]}`,
		`{"dimension":"project"}`,
	} {
		if rec := crudDo(t, s, http.MethodPost, base, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: %d body=%s", body, rec.Code, rec.Body.String())
		}
	}
	// No datasource configured → 502 (the client refuses to dial).
	if all, err := m.PG().ListMCP(); err == nil {
		for _, row := range all {
			if row.Name == scopeSentryMCPName {
				_ = m.PG().DeleteMCP(row.ID)
			}
		}
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"dimension":"project","targets":["p1"]}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("no datasource: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSyncSSRunSyncsAssets(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	company := fmt.Sprintf("SS Acme %d", stamp)
	subdomain := fmt.Sprintf("ss-sub-%d.example.com", stamp)
	appName := fmt.Sprintf("SS App %d", stamp)
	svcURL := fmt.Sprintf("https://ss-svc-%d.example.com", stamp)

	srv := newMCPMock(t, func(tool string, args map[string]any) (string, bool) {
		switch tool {
		case "get_project":
			return fmt.Sprintf(`{"name":%q,"target":"a-%d.example.com\nb-%d.example.com"}`, company, stamp, stamp), false
		case "list_assets":
			switch args["asset_type"] {
			case "subdomain":
				return fmt.Sprintf(`{"list":[{"host":%q,"type":"A","value":["1.2.3.4"],"ip":["10.61.61.1"]}]}`, subdomain), false
			case "asset":
				return fmt.Sprintf(`{"list":[{"url":%q,"ip":"10.62.62.2","service":"http","title":"svc","status":200,"products":["nginx"]}]}`, svcURL), false
			case "app":
				return fmt.Sprintf(`{"list":[{"name":%q,"category":"web","description":"d","icp":"x"}]}`, appName), false
			}
		}
		return `{"list":[]}`, false
	})
	configureScopeSentry(t, m, srv.URL, true)
	t.Cleanup(func() {
		if comp, err := m.PG().Companies().GetCompanyByName(company); err == nil && comp != nil {
			_ = m.PG().Companies().DeleteCompany(comp.ID)
		}
	})

	var out struct {
		Synced    map[string]int `json:"synced"`
		Companies []string       `json:"companies"`
		Warnings  []string       `json:"warnings"`
		Errors    []string       `json:"errors"`
	}
	// An unknown asset type is skipped with a warning; the three real types sync.
	rec := crudDo(t, s, http.MethodPost, "/api/sync/scopesentry/sync",
		fmt.Sprintf(`{"dimension":"project","targets":["p-%d"],"asset_types":["subdomain","service","app","mystery"],"create_company":true,"page_size":50}`, stamp))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.Synced["subdomain"] != 1 || out.Synced["ip"] != 1 || out.Synced["service"] != 1 || out.Synced["app"] != 1 {
		t.Fatalf("synced=%+v errors=%v warnings=%v", out.Synced, out.Errors, out.Warnings)
	}
	if len(out.Errors) != 0 {
		t.Fatalf("sync errors=%v", out.Errors)
	}
	if len(out.Companies) != 1 || out.Companies[0] != company {
		t.Fatalf("companies=%v want %q", out.Companies, company)
	}
	unknownWarned := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "未知资产类型") {
			unknownWarned = true
		}
	}
	if !unknownWarned {
		t.Fatalf("unknown asset type not warned: %v", out.Warnings)
	}

	// The company exists with the project's target roots as scope.
	comp, err := m.PG().Companies().GetCompanyByName(company)
	if err != nil || comp == nil {
		t.Fatalf("company not created: %+v err=%v", comp, err)
	}
	scope, err := m.PG().Companies().GetScope(comp.ID)
	if err != nil {
		t.Fatalf("company scope: %v", err)
	}
	roots := map[string]bool{}
	for _, row := range scope {
		roots[row.Domain] = true
	}
	if !roots[fmt.Sprintf("a-%d.example.com", stamp)] || !roots[fmt.Sprintf("b-%d.example.com", stamp)] {
		t.Fatalf("company scope=%+v", scope)
	}
}

func TestSSIngestMapping(t *testing.T) {
	m, s := newTestStack(t)
	as := m.Assets()
	if as == nil {
		t.Skip("asset store unavailable")
	}
	stamp := time.Now().UnixNano()
	synced := map[string]int{"subdomain": 0, "app": 0, "service": 0, "ip": 0}

	// subdomain with a bound IP increments both counters.
	host := fmt.Sprintf("ingest-%d.example.com", stamp)
	if err := s.ssIngest(as, "subdomain", json.RawMessage(fmt.Sprintf(
		`{"host":%q,"type":"A","value":["1.1.1.1"],"ip":["10.63.63.1"]}`, host)), synced); err != "" {
		t.Fatalf("subdomain: %s", err)
	}
	if synced["subdomain"] != 1 || synced["ip"] != 1 {
		t.Fatalf("subdomain synced=%+v", synced)
	}
	// Blank host is skipped silently.
	if err := s.ssIngest(as, "subdomain", json.RawMessage(`{"host":"","ip":["10.63.63.2"]}`), synced); err != "" {
		t.Fatalf("blank host: %s", err)
	}
	if synced["subdomain"] != 1 {
		t.Fatalf("blank host counted: %+v", synced)
	}
	// Malformed payloads report a parse error.
	if err := s.ssIngest(as, "subdomain", json.RawMessage(`not-json`), synced); !strings.Contains(err, "解析失败") {
		t.Fatalf("bad subdomain err=%q", err)
	}
	if err := s.ssIngest(as, "app", json.RawMessage(`{bad`), synced); !strings.Contains(err, "app 解析失败") {
		t.Fatalf("bad app err=%q", err)
	}
	if err := s.ssIngest(as, "service", json.RawMessage(`{bad`), synced); !strings.Contains(err, "service 解析失败") {
		t.Fatalf("bad service err=%q", err)
	}

	// app ingest.
	appName := fmt.Sprintf("Ingest App %d", stamp)
	if err := s.ssIngest(as, "app", json.RawMessage(fmt.Sprintf(
		`{"name":%q,"category":"web","description":"d","icp":"ICP"}`, appName)), synced); err != "" {
		t.Fatalf("app: %s", err)
	}
	if synced["app"] != 1 {
		t.Fatalf("app synced=%+v", synced)
	}
	// Blank app name is skipped.
	if err := s.ssIngest(as, "app", json.RawMessage(`{"name":""}`), synced); err != "" || synced["app"] != 1 {
		t.Fatalf("blank app err=%q synced=%+v", err, synced)
	}

	// service: http row goes to the HTTPService table.
	url := fmt.Sprintf("https://ingest-svc-%d.example.com", stamp)
	if err := s.ssIngest(as, "service", json.RawMessage(fmt.Sprintf(
		`{"url":%q,"ip":"10.64.64.1","service":"http","title":"t","status":204,"products":["nginx"]}`, url)), synced); err != "" {
		t.Fatalf("http service: %s", err)
	}
	if synced["service"] != 1 {
		t.Fatalf("service synced=%+v", synced)
	}
	// service: non-http without URL goes to OtherService.
	if err := s.ssIngest(as, "service", json.RawMessage(fmt.Sprintf(
		`{"domain":%q,"ip":"10.64.64.2","port":"6379","service":"redis"}`, host)), synced); err != "" {
		t.Fatalf("other service: %s", err)
	}
	if synced["service"] != 2 {
		t.Fatalf("other service not counted: %+v", synced)
	}
	// service: an http-labelled row without a URL is skipped.
	if err := s.ssIngest(as, "service", json.RawMessage(`{"service":"http","ip":"10.64.64.3"}`), synced); err != "" || synced["service"] != 2 {
		t.Fatalf("url-less http err=%q synced=%+v", err, synced)
	}
}

func TestSSPageAllPaging(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	page := 0
	srv := newMCPMock(t, func(tool string, args map[string]any) (string, bool) {
		if tool != "list_assets" {
			return `{}`, false
		}
		if args["asset_type"] != "subdomain" {
			t.Fatalf("unexpected asset_type=%v", args["asset_type"])
		}
		page++
		if f, ok := args["filter"].(map[string]any); !ok || f["project"] == nil {
			t.Fatalf("filter not forwarded: %v", args["filter"])
		}
		switch page {
		case 1:
			// A full page keeps the loop going.
			items := make([]string, 0, 2)
			for i := 0; i < 2; i++ {
				items = append(items, fmt.Sprintf(`{"host":"p%d-%d.example.com"}`, i, stamp))
			}
			return `{"list":[` + strings.Join(items, ",") + `]}`, false
		case 2:
			// A short page ends it.
			return fmt.Sprintf(`{"list":[{"host":"last-%d.example.com"}]}`, stamp), false
		}
		return `{"list":[]}`, false
	})
	configureScopeSentry(t, m, srv.URL, true)
	cl, err := s.scopeSentryClient(t.Context())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cl.Close()
	items, truncated, err := s.ssPageAll(t.Context(), cl, "subdomain", map[string]any{"project": []string{"p1"}}, 2)
	if err != nil {
		t.Fatalf("page all: %v", err)
	}
	if len(items) != 3 || truncated {
		t.Fatalf("items=%d truncated=%v pages=%d", len(items), truncated, page)
	}

	// A tool error stops paging and surfaces the failure.
	failing := newMCPMock(t, func(string, map[string]any) (string, bool) { return "nope", true })
	configureScopeSentry(t, m, failing.URL, true)
	cl2, err := s.scopeSentryClient(t.Context())
	if err != nil {
		t.Fatalf("client2: %v", err)
	}
	defer cl2.Close()
	if _, _, err := s.ssPageAll(t.Context(), cl2, "subdomain", nil, 100); err == nil {
		t.Fatal("tool error not surfaced")
	}
}
