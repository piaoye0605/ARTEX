package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Second sweep of never-covered handler branches: the guard ladders that decide
// whether a worker message may be delivered, the notification-channel PATCH
// validation, the tail of the settings PUT, and the runtime LLM switch.
//
// Every case is an HTTP round trip through the real router; the assertions are
// the status codes and persistence those guards promise.

func TestWorkerMessageValidationLadder(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("worker-msg", "消息接口的守卫阶梯", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	post := func(taskID, iid, body string) (code int, resp string) {
		t.Helper()
		rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+taskID+"/intents/"+iid+"/messages", body)
		return rec.Code, rec.Body.String()
	}

	// Unknown task / malformed intent id / malformed body.
	if code, _ := post("999999999", "1", `{"message":"hi","request_id":"r1"}`); code != http.StatusNotFound {
		t.Fatalf("unknown task status=%d", code)
	}
	if code, _ := post(task.ID, "abc", `{"message":"hi","request_id":"r1"}`); code != http.StatusBadRequest {
		t.Fatalf("bad intent id status=%d", code)
	}
	if code, _ := post(task.ID, "1", `{`); code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d", code)
	}
	oversized := `{"message":"` + strings.Repeat("x", maxWorkerMessageBytes+1024) + `","request_id":"r1"}`
	if code, _ := post(task.ID, "1", oversized); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d", code)
	}

	// Message content guards.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", `{"message":"   ","request_id":"r1"}`},
		{"too-long", `{"message":"` + strings.Repeat("字", 4001) + `","request_id":"r1"}`},
		{"bad-request-id", `{"message":"hi","request_id":"has space"}`},
		{"missing-request-id", `{"message":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := post(task.ID, "1", tc.body); code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", code, body)
			}
		})
	}

	// Task lifecycle guards come before the intent lookup: a paused, queued or
	// finished task cannot receive a message, and saying so beats a silent no-op.
	setLifecycle := func(fn func(*taskLifecycleState)) {
		task.updateLifecycle(fn)
	}
	valid := `{"message":"继续","request_id":"r1"}`

	m.SetTaskPaused(task.ID, true)
	if code, body := post(task.ID, "1", valid); code != http.StatusConflict {
		t.Fatalf("paused task status=%d body=%s", code, body)
	}
	m.SetTaskPaused(task.ID, false)

	setLifecycle(func(l *taskLifecycleState) { l.Queued = true })
	if code, body := post(task.ID, "1", valid); code != http.StatusConflict {
		t.Fatalf("queued task status=%d body=%s", code, body)
	}
	setLifecycle(func(l *taskLifecycleState) { l.Queued = false })

	setLifecycle(func(l *taskLifecycleState) { l.Status = "done" })
	if code, body := post(task.ID, "1", valid); code != http.StatusConflict {
		t.Fatalf("terminal task status=%d body=%s", code, body)
	}
	setLifecycle(func(l *taskLifecycleState) { l.Status = "" })

	// Intent-level guards.
	if code, body := post(task.ID, "999999", valid); code != http.StatusNotFound {
		t.Fatalf("missing intent status=%d body=%s", code, body)
	}

	goalID, err := task.Store.AddNode(db.KindGoal, map[string]any{"summary": "目标节点"}, 1, "open", "human", nil)
	if err != nil {
		t.Fatalf("add goal node: %v", err)
	}
	if code, body := post(task.ID, strconv.FormatInt(goalID, 10), valid); code != http.StatusConflict {
		t.Fatalf("non-intent node status=%d body=%s", code, body)
	}

	runningID, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "运行中意图"}, 1, "running", "planner", nil)
	if err != nil {
		t.Fatalf("add running intent: %v", err)
	}
	if code, body := post(task.ID, strconv.FormatInt(runningID, 10), valid); code != http.StatusConflict {
		t.Fatalf("running intent status=%d body=%s", code, body)
	}
}

func TestNotifyUpdateChannelBranches(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "更新分支",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})
	id := strconv.FormatInt(chID, 10)
	path := "/api/notify/channels/" + id

	patch := func(target, body string) (int, string) {
		t.Helper()
		rec := f.request("PATCH", target, body)
		return rec.Code, rec.Body.String()
	}
	patchJSON := func(payload map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		return patch(path, string(raw))
	}

	if code, _ := patch("/api/notify/channels/abc", `{}`); code != http.StatusBadRequest {
		t.Fatalf("bad id status=%d", code)
	}
	if code, _ := patch("/api/notify/channels/999999999", `{}`); code != http.StatusNotFound {
		t.Fatalf("unknown channel status=%d", code)
	}
	if code, body := patch(path, `{`); code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d body=%s", code, body)
	}

	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"invalid-kind", map[string]any{"kind": "sms"}, http.StatusBadRequest},
		{"invalid-mode", map[string]any{"mode": "sometimes"}, http.StatusBadRequest},
		{"negative-rate", map[string]any{"rate_per_min": -1}, http.StatusBadRequest},
		{"blank-name", map[string]any{"name": "   "}, http.StatusBadRequest},
		{"bad-filter", map[string]any{"filter": map[string]any{"min_severity": "nope"}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := patchJSON(tc.payload)
			if code != tc.want {
				t.Fatalf("status=%d want %d body=%s", code, tc.want, body)
			}
		})
	}

	// Valid partial update: name + digest mode + unlimited rate + filter.
	code, body := patchJSON(map[string]any{
		"name":         "改名后",
		"mode":         "digest",
		"rate_per_min": 0,
		"filter":       map[string]any{"min_severity": "high"},
	})
	if code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", code, body)
	}
	ch := f.channel(t, chID)
	if ch.Name != "改名后" || ch.Mode != "digest" || ch.RatePerMin != 0 {
		t.Fatalf("update not persisted: name=%q mode=%q rate=%d", ch.Name, ch.Mode, ch.RatePerMin)
	}
	if !strings.Contains(string(ch.Filter), "high") {
		t.Fatalf("filter not persisted: %s", ch.Filter)
	}

	// Flipping enabled takes the "save config, then toggle" path so pending
	// deliveries get marked skipped instead of arriving stale on re-enable.
	code, body = patchJSON(map[string]any{"enabled": false})
	if code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", code, body)
	}
	if ch := f.channel(t, chID); ch.Enabled != nil && *ch.Enabled {
		t.Fatalf("channel still enabled: %+v", ch)
	}
	code, body = patchJSON(map[string]any{"enabled": true})
	if code != http.StatusOK {
		t.Fatalf("re-enable status=%d body=%s", code, body)
	}

	// Changing the kind replaces the credential set wholesale: the old credentials
	// belong to the old address, so changing the target without re-stating them is
	// refused rather than silently forwarding secrets to a new host.
	code, body = patchJSON(map[string]any{
		"kind":   notify.KindWebhook,
		"config": map[string]any{"url": "https://example.com/hook"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("kind change without credentials status=%d body=%s", code, body)
	}
	code, body = patchJSON(map[string]any{
		"kind":   notify.KindWebhook,
		"config": map[string]any{"url": "https://example.com/hook", "headers": map[string]any{}},
	})
	if code != http.StatusOK {
		t.Fatalf("kind change status=%d body=%s", code, body)
	}
	if ch := f.channel(t, chID); ch.Kind != notify.KindWebhook {
		t.Fatalf("kind not changed: %q", ch.Kind)
	}
}

func TestSettingsPutTailFields(t *testing.T) {
	m, s := newTestStack(t)

	for _, key := range []string{
		settingNotifyPublicBaseURL, settingNotifyDigestMinutes, settingPythonInterp,
		settingAgentTrafficBinding, settingLLMPoolOn, settingLLMPoolBindFallback,
		settingTrafficCapture, settingLLMRecord,
		settingConcurrencyOn, settingConcurrencyLimit,
	} {
		snapshotSetting(t, s, key)
	}
	before := snapshotSettings(t, m)
	origOn, origLimit := m.ConcurrencyLimit()
	t.Cleanup(func() {
		for k, v := range before {
			_ = m.pg.SetSetting(k, v)
		}
		// Concurrency is the one setting the whole package observes: leaving it
		// enabled with a small limit would queue every task later tests create.
		_ = m.SetConcurrency(origOn, origLimit)
	})
	origWorkers := int(m.Workers())

	put := func(body string) (int, string) {
		t.Helper()
		rec := crudDo(t, s, http.MethodPut, "/api/settings", body)
		return rec.Code, rec.Body.String()
	}
	payload := func() map[string]any {
		t.Helper()
		rec := crudDo(t, s, http.MethodGet, "/api/settings", "")
		var out map[string]any
		crudJSON(t, rec, &out)
		return out
	}

	// One compound PUT covers every remaining field handler in the chain.
	code, body := put(`{
		"notify_public_base_url":"https://artex.example.com/",
		"notify_digest_interval_min":5,
		"python_interpreter":"/usr/bin/python3",
		"llm_record":true,
		"llm_pool_enabled":true,
		"llm_pool_bind_fallback":true,
		"agent_traffic_binding":false,
		"traffic_capture":false,
		"task_concurrency_enabled":true,
		"task_concurrency_limit":3,
		"web_search_enabled":false,
		"global_proxy":""
	}`)
	if code != http.StatusOK {
		t.Fatalf("compound put status=%d body=%s", code, body)
	}
	after := payload()
	// Trailing slashes are trimmed: back-links are built with "%s/function/...".
	if after["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("base url=%v want trimmed value", after["notify_public_base_url"])
	}
	if after["notify_digest_interval_min"] != float64(5) {
		t.Fatalf("digest interval=%v want 5", after["notify_digest_interval_min"])
	}
	if after["python_interpreter"] != "/usr/bin/python3" {
		t.Fatalf("python interpreter=%v", after["python_interpreter"])
	}
	if after["llm_record"] != true || after["llm_pool_enabled"] != true || after["llm_pool_bind_fallback"] != true {
		t.Fatalf("LLM switches not echoed: %v/%v/%v", after["llm_record"], after["llm_pool_enabled"], after["llm_pool_bind_fallback"])
	}
	if after["agent_traffic_binding"] != false || after["traffic_capture"] != false {
		t.Fatalf("traffic switches not echoed: %v/%v", after["agent_traffic_binding"], after["traffic_capture"])
	}
	if after["task_concurrency_enabled"] != true || after["task_concurrency_limit"] != float64(3) {
		t.Fatalf("concurrency=%v/%v", after["task_concurrency_enabled"], after["task_concurrency_limit"])
	}

	// Validation failures are client errors, never 500s.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"base-url-scheme", `{"notify_public_base_url":"artex.example.com"}`},
		{"digest-too-small", `{"notify_digest_interval_min":0}`},
		{"digest-too-large", `{"notify_digest_interval_min":1441}`},
		{"proxy-scheme", `{"global_proxy":"ftp://127.0.0.1:1"}`},
		{"workers-zero", `{"workers":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := put(tc.body); code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 body=%s", code, body)
			}
		})
	}

	// Clearing the back-link base URL is allowed (empty means "not configured").
	if code, body := put(`{"notify_public_base_url":""}`); code != http.StatusOK {
		t.Fatalf("clear base url status=%d body=%s", code, body)
	}
	if got := payload()["notify_public_base_url"]; got != "" {
		t.Fatalf("base url not cleared: %v", got)
	}
	// Put the worker count back so later tasks in this package start with the
	// same concurrency the package found.
	if code, body := put(fmt.Sprintf(`{"workers":%d}`, origWorkers)); code != http.StatusOK {
		t.Fatalf("restore workers status=%d body=%s", code, body)
	}
}

func TestSetLLMRouteBranches(t *testing.T) {
	_, s := newTestStack(t)
	pg := s.m.pg

	// The runtime switch persists the "default" profile, which is global state
	// for the shared database: snapshot it and put it back afterwards.
	var prevID int64
	if err := pg.QueryRow(`SELECT COALESCE(min(id),0) FROM llm_profiles WHERE name='default'`).Scan(&prevID); err != nil {
		t.Fatalf("find default profile: %v", err)
	}
	var prev *db.LLMProfile
	if prevID > 0 {
		var err error
		if prev, err = pg.ProfileByID(prevID); err != nil {
			t.Fatalf("load default profile: %v", err)
		}
	}
	t.Cleanup(func() {
		if prev != nil {
			if _, err := pg.SaveProfile(prev); err != nil {
				t.Logf("restore default profile: %v", err)
			}
			return
		}
		_, _ = pg.Exec(`DELETE FROM llm_profiles WHERE name='default'`)
	})

	post := func(body string) (int, string) {
		t.Helper()
		rec := crudDo(t, s, http.MethodPost, "/api/llm", body)
		return rec.Code, rec.Body.String()
	}

	if code, body := post(`{`); code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d body=%s", code, body)
	}

	// No key in the request and none stored → refuse instead of saving a profile
	// that can never run.
	s.cfgMu.Lock()
	s.llmCfg.APIKey = ""
	s.cfgMu.Unlock()
	if code, body := post(`{"provider":"anthropic","model":"claude-test","base_url":"http://127.0.0.1:1"}`); code != http.StatusBadRequest {
		t.Fatalf("missing key status=%d body=%s", code, body)
	}

	// A provider that cannot even be constructed is rejected before persisting.
	if code, body := post(`{"provider":"anthropic","model":"claude-test","base_url":"http://127.0.0.1:1","api_key":"k","proxy":"ftp://127.0.0.1:1"}`); code != http.StatusBadRequest {
		t.Fatalf("bad proxy status=%d body=%s", code, body)
	}

	// Happy path: a well-formed config is persisted and echoed back.
	code, body := post(`{"provider":"anthropic","model":"claude-test","base_url":"http://127.0.0.1:1","api_key":"k","rate_per_second":2,"rate_per_minute":30,"context_window_k":0}`)
	if code != http.StatusOK {
		t.Fatalf("set llm status=%d body=%s", code, body)
	}
	var applied struct {
		Configured bool   `json:"configured"`
		Model      string `json:"model"`
		KeySet     bool   `json:"key_set"`
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/llm", ""), &applied)
	if !applied.Configured || applied.Model != "claude-test" || !applied.KeySet {
		t.Fatalf("applied config = %+v", applied)
	}

	// Leaving api_key blank keeps the stored key: the UI never re-renders it.
	code, body = post(`{"provider":"anthropic","model":"claude-test-2","base_url":"http://127.0.0.1:1"}`)
	if code != http.StatusOK {
		t.Fatalf("blank key status=%d body=%s", code, body)
	}
	applied = struct {
		Configured bool   `json:"configured"`
		Model      string `json:"model"`
		KeySet     bool   `json:"key_set"`
	}{}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/llm", ""), &applied)
	if applied.Model != "claude-test-2" || !applied.KeySet {
		t.Fatalf("blank key dropped the stored key: %+v", applied)
	}
}

func TestGlobalFindingsPagedRoute(t *testing.T) {
	m, s := newTestStack(t)
	if _, err := m.pg.AddFinding(0, 0, "sweep-paged", "全局分页漏洞", "high", "summary", "evidence", "worker", nil); err != nil {
		t.Fatalf("add finding: %v", err)
	}

	// Bare list (dashboard summary) answers with a naked array.
	rec := crudDo(t, s, http.MethodGet, "/api/exploration/findings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bare list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var bare []any
	crudJSON(t, rec, &bare)
	if bare == nil {
		t.Fatal("bare list must be an array, not null")
	}

	// Paged list carries the cursor envelope and honours filters.
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/findings?page=1&limit=5&severity=high&q=sweep-paged", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("paged list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var paged struct {
		Items    []map[string]any `json:"items"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
	}
	crudJSON(t, rec, &paged)
	if paged.Page != 1 || paged.PageSize != 5 {
		t.Fatalf("paging envelope = %+v", paged)
	}
	if paged.Total < 1 || len(paged.Items) < 1 {
		t.Fatalf("filtered page lost the seeded finding: %+v", paged)
	}

	// Unknown task → empty array (task-scoped view of a deleted task).
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/findings?task=999999999", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown task status=%d body=%s", rec.Code, rec.Body.String())
	}
	var empty []any
	crudJSON(t, rec, &empty)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("unknown task = %#v", empty)
	}
}

func TestDeleteTaskRouteRejectsBadInput(t *testing.T) {
	_, s := newTestStack(t)

	if rec := crudDo(t, s, http.MethodDelete, "/api/tasks/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/tasks/1", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d body=%s", rec.Code, rec.Body.String())
	}
}
