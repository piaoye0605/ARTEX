package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// This file pins three control-plane surfaces:
//
//   - /api/settings (server_settings.go): the payload echo, every putSettings
//     branch including notify validation ranges and partial concurrency PUT,
//     plus the web-search test probe (fast-fail on a keyless backend).
//   - /api/tools + /api/mcp (server_tools_mcp.go): tool catalog read/update/
//     reset (builtin + orchestration) and the MCP CRUD lifecycle including the
//     404/502 refresh paths.
//   - main sessions + rule-mode chat (server_chat.go): session listing/creation,
//     the chat busy/deleting guards, chat status/stop, and the fallback replies
//     ("意图 …"/"提示 …"/态势摘要).
//
// Shared-DB hygiene: every settings key touched is snapshotted and restored;
// tool rows are put back with the reset endpoint itself; MCP rows are deleted.

// --- generic direct-handler callers ---

func invokeHandler(t *testing.T, s *Server, h func(http.ResponseWriter, *http.Request), method, target, body string, pathVals map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func chatDecode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

// --- /api/settings ---

func TestSettingsGetAndPutLifecycle(t *testing.T) {
	m, s := newTestStack(t)

	// Shared-DB hygiene: restore every settings key this test touches.
	snapshotSetting(t, s, settingNotifyEnabled)
	snapshotSetting(t, s, settingNotifyPublicBaseURL)
	snapshotSetting(t, s, settingNotifyDigestMinutes)
	snapshotSetting(t, s, settingConstraintsInjectPlanner)
	snapshotSetting(t, s, settingConstraintsInjectWorker)
	snapshotSetting(t, s, settingPythonInterp)
	before := snapshotSettings(t, m)
	t.Cleanup(func() {
		for k, v := range before {
			_ = m.pg.SetSetting(k, v)
		}
	})
	// Establish a clean web-search baseline: saved keys from earlier runs would
	// flip brave_key_set and make the probe below hit the real network.
	empty := ""
	if err := m.SetWebSearch(false, "", &empty, &empty, nil); err != nil {
		t.Fatalf("clear web search baseline: %v", err)
	}

	// Baseline GET: payload carries the documented keys with sane types.
	rec := invokeHandler(t, s, s.getSettings, http.MethodGet, "/api/settings", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	var base map[string]any
	chatDecode(t, rec, &base)
	for _, key := range []string{
		"traffic_capture", "agent_traffic_binding", "llm_record", "web_search_enabled",
		"web_search_backend", "brave_key_set", "tavily_key_set", "web_search_proxy",
		"global_proxy", "python_interpreter", "workers", "task_concurrency_enabled",
		"task_concurrency_limit", "llm_pool_enabled", "llm_pool_bind_fallback",
		"constraints_inject_planner", "constraints_inject_worker", "noa_compaction",
		"notify_enabled", "notify_public_base_url", "notify_digest_interval_min",
	} {
		if _, ok := base[key]; !ok {
			t.Fatalf("settings payload missing key %q", key)
		}
	}
	if base["brave_key_set"] != false {
		t.Fatalf("brave_key_set should be a presence bool, got %v", base["brave_key_set"])
	}
	// Concurrency off → the payload still echoes a usable default limit.
	if n, ok := base["task_concurrency_limit"].(float64); !ok || n != float64(defaultConcurrencyLimit) {
		t.Fatalf("task_concurrency_limit=%v want default %d", base["task_concurrency_limit"], defaultConcurrencyLimit)
	}
	if n, ok := base["notify_digest_interval_min"].(float64); !ok || n != float64(notifyDefaultDigestMinutes) {
		t.Fatalf("notify_digest_interval_min=%v want default %d", base["notify_digest_interval_min"], notifyDefaultDigestMinutes)
	}

	// PUT with no fields at all is a no-op that still echoes the payload.
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings", `{}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty put status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Bad JSON is a 400.
	if rec := invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings", `{"workers":`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d", rec.Code)
	}

	// Constraint injection switches persist immediately.
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"constraints_inject_planner":false,"constraints_inject_worker":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("constraints put status=%d body=%s", rec.Code, rec.Body.String())
	}
	var after map[string]any
	chatDecode(t, rec, &after)
	if after["constraints_inject_planner"] != false || after["constraints_inject_worker"] != false {
		t.Fatalf("constraints switches not echoed: %v/%v", after["constraints_inject_planner"], after["constraints_inject_worker"])
	}

	// Workers: invalid 0 → 400; valid value echoed back.
	if rec := invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings", `{"workers":0}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("workers=0 status=%d body=%s", rec.Code, rec.Body.String())
	}
	origWorkers := int(base["workers"].(float64))
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings", fmt.Sprintf(`{"workers":%d}`, origWorkers+1), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("workers put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if int(after["workers"].(float64)) != origWorkers+1 {
		t.Fatalf("workers echo=%v want %d", after["workers"], origWorkers+1)
	}

	// Task concurrency: a full PUT echoes both fields. Note the payload contract:
	// while the switch is OFF the limit always echoes the default (the stored
	// value only takes effect once enabled), so partial-PUT preservation is
	// asserted against the settings table instead of the echo.
	curOn, _ := m.ConcurrencyLimit()
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"task_concurrency_enabled":true,"task_concurrency_limit":7}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("concurrency put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["task_concurrency_enabled"] != true || after["task_concurrency_limit"] != float64(7) {
		t.Fatalf("concurrency echo: %v/%v want true/7", after["task_concurrency_enabled"], after["task_concurrency_limit"])
	}
	// Partial PUT (only the switch): the stored limit must survive untouched.
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"task_concurrency_enabled":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial concurrency put status=%d body=%s", rec.Code, rec.Body.String())
	}
	if v, ok, _ := m.pg.GetSetting(settingConcurrencyLimit); !ok || v != "7" {
		t.Fatalf("stored concurrency limit=%q ok=%v want 7 preserved", v, ok)
	}
	t.Cleanup(func() { _ = m.SetConcurrency(curOn, defaultConcurrencyLimit) })

	// Notify digest range guard: 0 and 1441 are rejected, 30 sticks.
	for _, bad := range []int{0, -5, 1441} {
		rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
			fmt.Sprintf(`{"notify_digest_interval_min":%d}`, bad), nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("digest=%d status=%d body=%s", bad, rec.Code, rec.Body.String())
		}
	}
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings", `{"notify_digest_interval_min":30}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("digest put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["notify_digest_interval_min"] != float64(30) {
		t.Fatalf("digest echo=%v want 30", after["notify_digest_interval_min"])
	}

	// Notify base URL: scheme required, trailing slash trimmed, "" clears.
	if rec := invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"notify_public_base_url":"ftp://example.com"}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scheme status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"notify_public_base_url":"https://artex.example.com/"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("base url put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("base url echo=%v (trailing slash must be trimmed)", after["notify_public_base_url"])
	}

	// Notify master switch.
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings", `{"notify_enabled":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("notify put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["notify_enabled"] != false {
		t.Fatalf("notify_enabled echo=%v", after["notify_enabled"])
	}

	// Python interpreter: value is trimmed before storage and echoed back.
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"python_interpreter":"  /usr/bin/python3  "}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("python put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["python_interpreter"] != "/usr/bin/python3" {
		t.Fatalf("python echo=%v", after["python_interpreter"])
	}

	// Web search: partial PUT persists the key; brave_key_set flips on.
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"web_search_backend":"brave-free","brave_search_api_key":"sk-brave-test"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("web search put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["web_search_backend"] != "brave-free" || after["brave_key_set"] != true {
		t.Fatalf("web search echo: backend=%v brave_key_set=%v", after["web_search_backend"], after["brave_key_set"])
	}

	// LLM pool toggles persist (llmOn is false here, so no agent rebuild runs).
	rec = invokeHandler(t, s, s.putSettings, http.MethodPut, "/api/settings",
		`{"llm_pool_enabled":true,"llm_pool_bind_fallback":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("llm pool put status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &after)
	if after["llm_pool_enabled"] != true || after["llm_pool_bind_fallback"] != true {
		t.Fatalf("llm pool echo: %v/%v", after["llm_pool_enabled"], after["llm_pool_bind_fallback"])
	}
}

func TestSettingsWebSearchProbe(t *testing.T) {
	m, s := newTestStack(t)
	before := snapshotSettings(t, m)
	t.Cleanup(func() {
		for k, v := range before {
			_ = m.pg.SetSetting(k, v)
		}
	})
	// Clear saved keys: a leftover brave/tavily key from another test would make
	// the probe below construct a real provider and hit the network.
	empty := ""
	if err := m.SetWebSearch(false, "brave-free", &empty, &empty, nil); err != nil {
		t.Fatalf("clear keys: %v", err)
	}

	// A keyless brave-free backend fails fast inside newSearchProvider — no
	// network involved — so the probe answers 200 {ok:false} with the reason.
	rec := invokeHandler(t, s, s.testWebSearch, http.MethodPost, "/api/settings/web-search/test",
		`{"web_search_backend":"brave-free"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Backend string `json:"backend"`
	}
	chatDecode(t, rec, &out)
	if out.OK || out.Backend != "brave-free" || !strings.Contains(out.Error, "Brave Search API key") {
		t.Fatalf("probe out=%+v", out)
	}

	// Malformed JSON is a 400; an empty body is allowed and falls back to the
	// saved config.
	if rec := invokeHandler(t, s, s.testWebSearch, http.MethodPost, "/api/settings/web-search/test", `{bad`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d", rec.Code)
	}
	// Save a keyless brave-free config so the fallback path also fails fast.
	if err := m.SetWebSearch(false, "brave-free", nil, nil, nil); err != nil {
		t.Fatalf("seed web search: %v", err)
	}
	rec = invokeHandler(t, s, s.testWebSearch, http.MethodPost, "/api/settings/web-search/test", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-body probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	chatDecode(t, rec, &out)
	if out.OK || out.Backend != "brave-free" {
		t.Fatalf("fallback probe out=%+v", out)
	}
}

func TestSettingsPythonDetect(t *testing.T) {
	_, s := newTestStack(t)
	snapshotSetting(t, s, settingPythonInterp)

	rec := invokeHandler(t, s, s.pgDetectPython, http.MethodPost, "/api/settings/python/detect", "", nil)
	// Environment-dependent: 200 when python(3) is on PATH, 404 otherwise.
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("detect status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusOK {
		var out struct {
			PythonInterpreter string `json:"python_interpreter"`
		}
		chatDecode(t, rec, &out)
		if strings.TrimSpace(out.PythonInterpreter) == "" {
			t.Fatalf("200 without interpreter value: %s", rec.Body.String())
		}
	}
}

// --- /api/tools ---

func TestToolsCatalogRoutes(t *testing.T) {
	_, s := newTestStack(t)

	// List: the seeded builtin catalog is non-empty and usage counts decorate rows.
	rec := invokeHandler(t, s, s.pgListTools, http.MethodGet, "/api/tools", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tools status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Tools []*dbToolDTO `json:"tools"`
	}
	chatDecode(t, rec, &listed)
	if len(listed.Tools) == 0 {
		t.Fatal("tool catalog is empty — builtin seeds should populate it")
	}
	var seed *dbToolDTO
	for _, tool := range listed.Tools {
		if tool.Key == "report_finding" {
			seed = tool
			break
		}
	}
	if seed == nil {
		t.Fatal("builtin seed report_finding missing from catalog")
	}
	if seed.Description == "" || len(seed.Schema) == 0 {
		t.Fatalf("seed row incomplete: %+v", seed)
	}
	defaultDesc := seed.Description

	// Update the editable fields.
	rec = invokeHandler(t, s, s.pgUpdateTool, http.MethodPut, "/api/tools/report_finding",
		`{"description":"改过的描述","agents":["worker"],"enabled":false}`, map[string]string{"key": "report_finding"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update tool status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = invokeHandler(t, s, s.pgListTools, http.MethodGet, "/api/tools", "", nil)
	chatDecode(t, rec, &listed)
	for _, tool := range listed.Tools {
		if tool.Key == "report_finding" {
			if tool.Description != "改过的描述" || tool.Enabled {
				t.Fatalf("updated row: %+v", tool)
			}
		}
	}

	// Bad JSON → 400; unknown key → 404.
	if rec := invokeHandler(t, s, s.pgUpdateTool, http.MethodPut, "/api/tools/report_finding",
		`{"description":`, map[string]string{"key": "report_finding"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d", rec.Code)
	}
	if rec := invokeHandler(t, s, s.pgUpdateTool, http.MethodPut, "/api/tools/nope",
		`{"description":"x"}`, map[string]string{"key": "nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Reset restores the code-defined description and re-enables the tool.
	rec = invokeHandler(t, s, s.pgResetTool, http.MethodPost, "/api/tools/report_finding/reset",
		"", map[string]string{"key": "report_finding"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = invokeHandler(t, s, s.pgListTools, http.MethodGet, "/api/tools", "", nil)
	chatDecode(t, rec, &listed)
	for _, tool := range listed.Tools {
		if tool.Key == "report_finding" {
			if tool.Description != defaultDesc || !tool.Enabled {
				t.Fatalf("reset row: %+v (want desc=%q enabled)", tool, defaultDesc)
			}
		}
	}

	// Orchestration tools (auto agent) reset through the second branch.
	rec = invokeHandler(t, s, s.pgResetTool, http.MethodPost, "/api/tools/list_tasks/reset",
		"", map[string]string{"key": "list_tasks"})
	if rec.Code != http.StatusOK {
		t.Fatalf("orchestration reset status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Unknown key on reset → 404.
	if rec := invokeHandler(t, s, s.pgResetTool, http.MethodPost, "/api/tools/nope/reset",
		"", map[string]string{"key": "nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("reset unknown status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// dbToolDTO mirrors the JSON of db.Tool for decoding without importing db here.
type dbToolDTO struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Enabled     bool            `json:"enabled"`
	Calls       int             `json:"calls"`
}

// --- /api/mcp ---

func TestMCPRoutesLifecycle(t *testing.T) {
	_, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	name := fmt.Sprintf("mcp-contract-%d", stamp)

	// Bad JSON → 400.
	if rec := invokeHandler(t, s, s.pgSaveMCP, http.MethodPost, "/api/mcp", `{"name":`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d", rec.Code)
	}

	// Create (disabled → skips auto tool discovery).
	body := fmt.Sprintf(`{"name":%q,"transport":"stdio","command":"__artex_missing_cmd__","args":[],"env":{},"enabled":false}`, name)
	rec := invokeHandler(t, s, s.pgSaveMCP, http.MethodPost, "/api/mcp", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	chatDecode(t, rec, &saved)
	if saved.ID == 0 {
		t.Fatal("save returned id=0")
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteMCP(saved.ID) })

	// List contains the new row.
	rec = invokeHandler(t, s, s.pgListMCP, http.MethodGet, "/api/mcp", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listOut struct {
		Servers []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Transport string `json:"transport"`
			Enabled   bool   `json:"enabled"`
		} `json:"servers"`
	}
	chatDecode(t, rec, &listOut)
	var found bool
	for _, srv := range listOut.Servers {
		if srv.ID == saved.ID {
			found = true
			if srv.Name != name || srv.Transport != "stdio" || srv.Enabled {
				t.Fatalf("listed row: %+v", srv)
			}
		}
	}
	if !found {
		t.Fatalf("saved id=%d missing from list", saved.ID)
	}

	// Cached tools of a fresh server: empty array (not null).
	rec = invokeHandler(t, s, s.pgMCPTools, http.MethodGet, "/api/mcp/"+i64s(saved.ID)+"/tools", "", map[string]string{"id": i64s(saved.ID)})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools status=%d body=%s", rec.Code, rec.Body.String())
	}
	var toolsOut struct {
		Tools []map[string]any `json:"tools"`
	}
	chatDecode(t, rec, &toolsOut)
	if toolsOut.Tools == nil {
		t.Fatal("tools must be [] not null")
	}

	// Refresh unknown id → 404; existing id with a broken stdio command → 502.
	if rec := invokeHandler(t, s, s.pgRefreshMCP, http.MethodPost, "/api/mcp/999999/refresh", "", map[string]string{"id": "999999"}); rec.Code != http.StatusNotFound {
		t.Fatalf("refresh unknown status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = invokeHandler(t, s, s.pgRefreshMCP, http.MethodPost, "/api/mcp/"+i64s(saved.ID)+"/refresh", "", map[string]string{"id": i64s(saved.ID)})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh broken stdio status=%d body=%s (want 502)", rec.Code, rec.Body.String())
	}

	// Delete, then the list no longer contains it.
	rec = invokeHandler(t, s, s.pgDeleteMCP, http.MethodDelete, "/api/mcp/"+i64s(saved.ID), "", map[string]string{"id": i64s(saved.ID)})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var delOut struct {
		Deleted int64 `json:"deleted"`
	}
	chatDecode(t, rec, &delOut)
	if delOut.Deleted != saved.ID {
		t.Fatalf("deleted=%d want %d", delOut.Deleted, saved.ID)
	}
	rec = invokeHandler(t, s, s.pgListMCP, http.MethodGet, "/api/mcp", "", nil)
	chatDecode(t, rec, &listOut)
	for _, srv := range listOut.Servers {
		if srv.ID == saved.ID {
			t.Fatal("row survived delete")
		}
	}
}

// TestMCPSaveEnabledDiscoversTools pins the auto-discovery contract: adding an
// ENABLED MCP triggers tool discovery right away (cached for the UI), and the
// refresh endpoint re-runs it on demand. Uses the JSON-RPC MCP mock.
func TestMCPSaveEnabledDiscoversTools(t *testing.T) {
	_, s := newTestStack(t)
	mock := newMCPMock(t, func(tool string, args map[string]any) (string, bool) {
		return "ok", false
	})
	stamp := time.Now().UnixNano()

	body := fmt.Sprintf(`{"name":"mcp-discover-%d","transport":"http","url":%q,"args":[],"env":{},"enabled":true}`, stamp, mock.URL)
	rec := invokeHandler(t, s, s.pgSaveMCP, http.MethodPost, "/api/mcp", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save enabled status=%d body=%s", rec.Code, rec.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	chatDecode(t, rec, &saved)
	if saved.ID == 0 {
		t.Fatal("save returned id=0")
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteMCP(saved.ID) })

	// Discovery on add populated the cache with the mock's tool.
	rec = invokeHandler(t, s, s.pgMCPTools, http.MethodGet, "/api/mcp/"+i64s(saved.ID)+"/tools", "", map[string]string{"id": i64s(saved.ID)})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools status=%d body=%s", rec.Code, rec.Body.String())
	}
	var toolsOut struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	// Discovery on add populated the cache with the mock's tool. Cached names
	// carry the mcp__<server>__ prefix.
	chatDecode(t, rec, &toolsOut)
	if len(toolsOut.Tools) != 1 || !strings.HasSuffix(toolsOut.Tools[0].Name, "__list_assets") {
		t.Fatalf("cached tools=%+v want one *__list_assets", toolsOut.Tools)
	}

	// Refresh re-discovers and returns the detailed cache.
	rec = invokeHandler(t, s, s.pgRefreshMCP, http.MethodPost, "/api/mcp/"+i64s(saved.ID)+"/refresh", "", map[string]string{"id": i64s(saved.ID)})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", rec.Code, rec.Body.String())
	}
	var refreshOut struct {
		Tools []map[string]any `json:"tools"`
	}
	chatDecode(t, rec, &refreshOut)
	if len(refreshOut.Tools) != 1 {
		t.Fatalf("refresh tools=%+v", refreshOut.Tools)
	}

	// Note: an unknown transport can't reach connectMCP through the API — the
	// mcp_servers.transport CHECK constraint rejects it with a 500 on save.
}

// --- main sessions + rule-mode chat ---

func TestMainSessionsLifecycle(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	task, err := m.CreateTask(fmt.Sprintf("chat sessions %d", stamp), "sessions goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	pathID := i64s(taskID)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	q := "?task=" + pathID

	// Fresh task: segment 0 is always listed, newest first.
	rec := invokeHandler(t, s, s.mainSessions, http.MethodGet, "/api/exploration/main-sessions"+q, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Sessions []struct {
			Seq       int    `json:"seq"`
			CreatedAt string `json:"created_at"`
		} `json:"sessions"`
		Current int `json:"current"`
	}
	chatDecode(t, rec, &out)
	if len(out.Sessions) == 0 || out.Sessions[0].Seq != out.Current {
		t.Fatalf("sessions=%+v current=%d", out.Sessions, out.Current)
	}
	prevCurrent := out.Current

	// Unknown task → 404.
	if rec := invokeHandler(t, s, s.mainSessions, http.MethodGet, "/api/exploration/main-sessions?task=999999", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task status=%d", rec.Code)
	}

	// New session advances the counter by exactly one and becomes current.
	rec = invokeHandler(t, s, s.newMainSession, http.MethodPost, "/api/exploration/main-session/new"+q, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("new session status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Seq       int    `json:"seq"`
		CreatedAt string `json:"created_at"`
		Current   int    `json:"current"`
	}
	chatDecode(t, rec, &created)
	if created.Seq != prevCurrent+1 || created.Current != created.Seq || created.CreatedAt == "" {
		t.Fatalf("created=%+v prevCurrent=%d", created, prevCurrent)
	}

	// Creating while the task is deleting → 409.
	s.engine.deleting.Store(pathID, true)
	defer s.engine.deleting.Delete(pathID)
	if rec := invokeHandler(t, s, s.newMainSession, http.MethodPost, "/api/exploration/main-session/new"+q, "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("deleting status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChatRuleModeAndGuards(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	task, err := m.CreateTask(fmt.Sprintf("chat rule %d", stamp), "rule goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	pathID := i64s(taskID)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	// Force the no-LLM rule path: no chat agent and no global provider, so the
	// handler answers synchronously (batch-9 lesson: shared DB may hold an
	// active profile that would otherwise build a chat agent).
	s.cfgMu.Lock()
	s.llmOn = false
	s.cfgMu.Unlock()
	s.chatAgent = nil

	// Unknown task → 404.
	if rec := invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task=999999", `{"message":"hi"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task status=%d", rec.Code)
	}
	// Bad JSON → 400.
	if rec := invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{message`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d", rec.Code)
	}
	// Deleting task → 409 (first barrier, before the busy map).
	s.engine.deleting.Store(pathID, true)
	rec := invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{"message":"hi"}`, nil)
	s.engine.deleting.Delete(pathID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Busy guard → 409.
	s.chatMu.Lock()
	s.chatBusy[pathID] = true
	s.chatMu.Unlock()
	rec = invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{"message":"hi"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("busy status=%d body=%s", rec.Code, rec.Body.String())
	}
	s.chatMu.Lock()
	delete(s.chatBusy, pathID)
	s.chatMu.Unlock()

	// Plain message → rule-mode situational summary, synchronous 200.
	rec = invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{"message":"hello"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rule chat status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Reply string `json:"reply"`
		Mode  string `json:"mode"`
	}
	chatDecode(t, rec, &reply)
	if reply.Mode != "rule" || !strings.Contains(reply.Reply, "规则模式") || !strings.Contains(reply.Reply, "意图") {
		t.Fatalf("rule reply=%+v", reply)
	}

	// "意图 …" injects a high-priority intent node.
	rec = invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{"message":"意图优先重建视图"}`, nil)
	chatDecode(t, rec, &reply)
	if reply.Mode != "rule" || !strings.HasPrefix(reply.Reply, "已注入一条高优先级意图：优先重建视图") {
		t.Fatalf("intent reply=%+v", reply)
	}
	// English alias works too.
	rec = invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{"message":"intent second one"}`, nil)
	chatDecode(t, rec, &reply)
	if !strings.Contains(reply.Reply, "second one") {
		t.Fatalf("english intent reply=%+v", reply)
	}

	// "提示 …" records a hint node for the planner.
	rec = invokeHandler(t, s, s.chat, http.MethodPost, "/api/chat?task="+pathID, `{"message":"提示留意内网段"}`, nil)
	chatDecode(t, rec, &reply)
	if !strings.HasPrefix(reply.Reply, "已记录提示") || !strings.Contains(reply.Reply, "留意内网段") {
		t.Fatalf("hint reply=%+v", reply)
	}

	// The human turns landed in the activity stream under worker=mainagent.
	rec = invokeHandler(t, s, s.activity, http.MethodGet, "/api/exploration/activity?task="+pathID+"&limit=50", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", rec.Code, rec.Body.String())
	}
	var act struct {
		Items []struct {
			Worker  string `json:"worker"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	chatDecode(t, rec, &act)
	var sawHuman bool
	for _, it := range act.Items {
		if it.Worker == "mainagent" && (strings.Contains(it.Summary, "hello") || strings.Contains(it.Summary, "优先重建视图")) {
			sawHuman = true
		}
	}
	if !sawHuman {
		t.Fatalf("human turn missing from activity: %+v", act.Items)
	}

	// Chat status reflects the busy flag.
	rec = invokeHandler(t, s, s.taskChatStatus, http.MethodGet, "/api/tasks/"+pathID+"/chat/status", "", map[string]string{"id": pathID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var statusOut struct {
		Running bool `json:"running"`
	}
	chatDecode(t, rec, &statusOut)
	if statusOut.Running {
		t.Fatal("status running without busy flag")
	}
	s.chatMu.Lock()
	s.chatBusy[pathID] = true
	s.chatMu.Unlock()
	rec = invokeHandler(t, s, s.taskChatStatus, http.MethodGet, "/api/tasks/"+pathID+"/chat/status", "", map[string]string{"id": pathID})
	chatDecode(t, rec, &statusOut)
	if !statusOut.Running {
		t.Fatal("status not running with busy flag set")
	}
	s.chatMu.Lock()
	delete(s.chatBusy, pathID)
	s.chatMu.Unlock()

	// Stop: without an in-flight turn → idle; with a cancel registered → stopping.
	rec = invokeHandler(t, s, s.stopChat, http.MethodPost, "/api/tasks/"+pathID+"/chat/stop", "", map[string]string{"id": pathID})
	var stopOut struct {
		Status string `json:"status"`
	}
	chatDecode(t, rec, &stopOut)
	if stopOut.Status != "idle" {
		t.Fatalf("stop idle=%+v", stopOut)
	}
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	s.chatMu.Lock()
	s.chatCancel[pathID] = cancel
	s.chatMu.Unlock()
	rec = invokeHandler(t, s, s.stopChat, http.MethodPost, "/api/tasks/"+pathID+"/chat/stop", "", map[string]string{"id": pathID})
	chatDecode(t, rec, &stopOut)
	if stopOut.Status != "stopping" {
		t.Fatalf("stop stopping=%+v", stopOut)
	}
	// The cancel entry stays registered until finishTaskChat (only the chat
	// turn itself removes it), so a second stop still reports "stopping".
	rec = invokeHandler(t, s, s.stopChat, http.MethodPost, "/api/tasks/"+pathID+"/chat/stop", "", map[string]string{"id": pathID})
	chatDecode(t, rec, &stopOut)
	if stopOut.Status != "stopping" {
		t.Fatalf("second stop=%+v", stopOut)
	}

	// Unknown task on status/stop → 404.
	if rec := invokeHandler(t, s, s.taskChatStatus, http.MethodGet, "/api/tasks/999999/chat/status", "", map[string]string{"id": "999999"}); rec.Code != http.StatusNotFound {
		t.Fatalf("status unknown=%d", rec.Code)
	}
	if rec := invokeHandler(t, s, s.stopChat, http.MethodPost, "/api/tasks/999999/chat/stop", "", map[string]string{"id": "999999"}); rec.Code != http.StatusNotFound {
		t.Fatalf("stop unknown=%d", rec.Code)
	}
}
