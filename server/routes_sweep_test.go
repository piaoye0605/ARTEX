package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"golang.org/x/crypto/bcrypt"
)

// This file pins the fourteen routes that had never been referenced by a test:
// health / gc / audit / report, task-category CRUD (single + batch), login and
// password change, the LLM connection test, agent visibility toggling, and the
// findings asset tree / active-retest list.
//
// Everything runs the real router against a real Manager, so the assertions
// describe persistence and wire format, not handler plumbing.

// noTaskServer builds a Server whose Manager deliberately holds no tasks.
//
// newTestStack() goes through New(), which reloads every task the shared test
// database still carries (see Manager.LoadExisting). A task left behind by an
// earlier run would silently flip the "no active task" branches, so the empty
// state has to be constructed explicitly to stay deterministic.
func noTaskServer(t *testing.T) *Server {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("test postgres not configured: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		pg.Close()
	})
	return &Server{
		ctx:    ctx,
		m:      &Manager{pg: pg, dir: t.TempDir(), tasks: map[string]*Task{}},
		jwtKey: []byte("sweep-test-signing-key-32-byte"),
	}
}

func TestHealthGCAndNoTaskRoutes(t *testing.T) {
	s := noTaskServer(t)

	rec := crudDo(t, s, http.MethodGet, "/api/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", rec.Code, rec.Body.String())
	}
	var health struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
		Version string `json:"version"`
	}
	crudJSON(t, rec, &health)
	if !health.OK || health.Service != "artex" || health.Version == "" {
		t.Fatalf("health payload incomplete: %+v", health)
	}

	// GC is a documented no-op stub: the contract is 200 + removed=0, not 404.
	rec = crudDo(t, s, http.MethodPost, "/api/gc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("gc status=%d body=%s", rec.Code, rec.Body.String())
	}
	var gc struct {
		Removed int `json:"removed"`
	}
	crudJSON(t, rec, &gc)
	if gc.Removed != 0 {
		t.Fatalf("gc removed=%d want 0", gc.Removed)
	}

	// No task selected: audit answers with an empty array (never null — the
	// frontend maps over it directly), report answers 404.
	rec = crudDo(t, s, http.MethodGet, "/api/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var empty []any
	crudJSON(t, rec, &empty)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("audit without task = %#v, want empty non-nil array", empty)
	}

	rec = crudDo(t, s, http.MethodGet, "/api/report", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("report without task status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReportAndAuditWithTask(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("sweep-report", "把报告与审计读面跑通", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	rec := crudDo(t, s, http.MethodGet, "/api/report?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("report status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The report is served as raw markdown so the browser can render it inline.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("report content-type=%q want text/markdown", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sweep-report") || !strings.Contains(body, "把报告与审计读面跑通") {
		t.Fatalf("report body missing task identity:\n%s", body)
	}

	rec = crudDo(t, s, http.MethodGet, "/api/audit?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var audit struct {
		Entries []any `json:"entries"`
		// Attributions is a per-host tally (host → count), not a list.
		Attributions map[string]any `json:"attributions"`
	}
	crudJSON(t, rec, &audit)
	// Fresh task: nothing guarded yet, but both keys must exist so the UI can
	// distinguish "empty" from "field missing from this build".
	if audit.Entries == nil || audit.Attributions == nil {
		t.Fatalf("audit payload missing keys: %+v", audit)
	}
}

func TestTaskCategoryCRUDRoutes(t *testing.T) {
	m, s := newTestStack(t)
	name := "sweep-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	rec := crudDo(t, s, http.MethodGet, "/api/task-categories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list categories status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Categories []db.TaskCategory `json:"categories"`
	}
	crudJSON(t, rec, &list)
	if list.Categories == nil {
		t.Fatal("categories must be an empty array, not null")
	}

	rec = crudDo(t, s, http.MethodPost, "/api/task-categories", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created db.TaskCategory
	crudJSON(t, rec, &created)
	if created.ID == 0 || created.Name != name {
		t.Fatalf("created category = %+v", created)
	}
	t.Cleanup(func() { _, _ = m.DeleteTaskCategory(created.ID) })

	// Duplicate name is a 409, not a 500: the unique key is a user-facing rule.
	rec = crudDo(t, s, http.MethodPost, "/api/task-categories", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"blank-name", `{"name":"   "}`, http.StatusBadRequest},
		{"missing-name", `{}`, http.StatusBadRequest},
		{"too-long", `{"name":"` + strings.Repeat("x", db.MaxTaskCategoryNameRunes+1) + `"}`, http.StatusBadRequest},
		{"bad-json", `{`, http.StatusBadRequest},
		{"oversized", `{"name":"` + strings.Repeat("y", maxTaskCategoryRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := crudDo(t, s, http.MethodPost, "/api/task-categories", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	renamed := name + "-r"
	rec = crudDo(t, s, http.MethodPatch, "/api/task-categories/"+strconv.FormatInt(created.ID, 10), `{"name":"`+renamed+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", rec.Code, rec.Body.String())
	}
	var afterRename db.TaskCategory
	crudJSON(t, rec, &afterRename)
	if afterRename.Name != renamed {
		t.Fatalf("rename result=%+v want name %q", afterRename, renamed)
	}

	rec = crudDo(t, s, http.MethodPatch, "/api/task-categories/999999999", `{"name":"sweep-ghost"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rename unknown status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodPatch, "/api/task-categories/abc", `{"name":"sweep-ghost"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename bad id status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = crudDo(t, s, http.MethodDelete, "/api/task-categories/"+strconv.FormatInt(created.ID, 10), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodDelete, "/api/task-categories/"+strconv.FormatInt(created.ID, 10), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodDelete, "/api/task-categories/abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete bad id status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskCategoryAssignmentRoutes(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("sweep-cat-task", "任务分类绑定", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })

	other, err := m.CreateTask("sweep-cat-task-2", "批量分类", nil, 0, 0)
	if err != nil {
		t.Fatalf("create second task: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(other.ID, DeleteTaskOptions{}) })

	cat, err := m.pg.CreateTaskCategory("sweep-bind-" + strconv.FormatInt(time.Now().UnixNano(), 36))
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTaskCategory(cat.ID) })

	path := "/api/tasks/" + task.ID + "/category"
	rec := crudDo(t, s, http.MethodPatch, path, `{"category_id":`+strconv.FormatInt(cat.ID, 10)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status=%d body=%s", rec.Code, rec.Body.String())
	}
	var dto TaskDTO
	crudJSON(t, rec, &dto)
	if dto.CategoryID == nil || *dto.CategoryID != cat.ID || dto.CategoryName != cat.Name {
		t.Fatalf("assign result category=%v/%q want %d/%q", dto.CategoryID, dto.CategoryName, cat.ID, cat.Name)
	}

	// Explicit null clears the category. This is why the field is RawMessage:
	// *int64 cannot tell `"category_id":null` from an omitted key.
	rec = crudDo(t, s, http.MethodPatch, path, `{"category_id":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	dto = TaskDTO{}
	crudJSON(t, rec, &dto)
	if dto.CategoryID != nil {
		t.Fatalf("category should be cleared, got %d", *dto.CategoryID)
	}

	for _, tc := range []struct {
		name string
		path string
		body string
		want int
	}{
		{"missing-field", path, `{}`, http.StatusBadRequest},
		{"zero-id", path, `{"category_id":0}`, http.StatusBadRequest},
		{"string-id", path, `{"category_id":"x"}`, http.StatusBadRequest},
		{"bad-json", path, `{`, http.StatusBadRequest},
		{"unknown-task", "/api/tasks/999999999/category", `{"category_id":null}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := crudDo(t, s, http.MethodPatch, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// Batch move: one live task, one malformed id, one unknown id. The database
	// write is atomic, so per-item errors only distinguish ids that never
	// resolved to a live task.
	body := `{"task_ids":["` + task.ID + `","abc","999999999"],"category_id":` + strconv.FormatInt(cat.ID, 10) + `}`
	rec = crudDo(t, s, http.MethodPost, "/api/tasks/category/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch struct {
		Items []struct {
			ID    string `json:"id"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"items"`
		Category *db.TaskCategory `json:"category"`
	}
	crudJSON(t, rec, &batch)
	if len(batch.Items) != 3 {
		t.Fatalf("batch items=%+v", batch.Items)
	}
	for _, item := range batch.Items {
		switch item.ID {
		case task.ID:
			if !item.OK || item.Error != "" {
				t.Fatalf("live task should move, got %+v", item)
			}
		default:
			if item.OK || item.Error == "" {
				t.Fatalf("invalid/unknown id should report an error, got %+v", item)
			}
		}
	}
	if batch.Category == nil || batch.Category.ID != cat.ID {
		t.Fatalf("batch category=%+v want %d", batch.Category, cat.ID)
	}

	// Empty and oversized selections are rejected before touching the database.
	tooMany := make([]string, 0, db.MaxTaskCategoryBatchSize+1)
	for i := 0; i <= db.MaxTaskCategoryBatchSize; i++ {
		tooMany = append(tooMany, `"`+strconv.Itoa(i+1)+`"`)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", `{"task_ids":[],"category_id":null}`},
		{"too-many", `{"task_ids":[` + strings.Join(tooMany, ",") + `],"category_id":null}`},
		{"missing-category", `{"task_ids":["1"]}`},
	} {
		t.Run("batch-"+tc.name, func(t *testing.T) {
			rec := crudDo(t, s, http.MethodPost, "/api/tasks/category/batch", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// Clearing through the batch endpoint removes the assignment again.
	rec = crudDo(t, s, http.MethodPost, "/api/tasks/category/batch", `{"task_ids":["`+task.ID+`"],"category_id":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	reloaded, ok := m.Task(task.ID)
	if !ok {
		t.Fatal("task vanished")
	}
	if id := reloaded.lifecycleSnapshot().CategoryID; id != nil {
		t.Fatalf("batch clear left category %d", *id)
	}
}

func TestAuthLoginAndChangePasswordRoutes(t *testing.T) {
	_, s := newTestStack(t)
	pg := s.m.pg

	// The shared database may already carry a password from another test;
	// restore whatever was there so this case leaves no trace.
	prev, hadPrev, err := pg.GetSetting(authPassKey)
	if err != nil {
		t.Fatalf("read auth setting: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = pg.SetSetting(authPassKey, prev)
			return
		}
		_, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, authPassKey)
	})
	if _, err := pg.Exec(`DELETE FROM settings WHERE key=$1`, authPassKey); err != nil {
		t.Fatalf("reset auth setting: %v", err)
	}

	raw := func(method, path, body string, withToken bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withToken {
			tok, err := signJWT(s.jwtKey)
			if err != nil {
				t.Fatalf("sign jwt: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Password not initialised yet: both entry points refuse instead of
	// treating "unreadable" as "no password required".
	rec := raw(http.MethodPost, "/api/auth/login", `{"username":"ARTEX","password":"whatever1"}`, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("login before init status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = raw(http.MethodPost, "/api/auth/change-password", `{"old_password":"x","new_password":"newpassword1"}`, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("change before init status=%d body=%s", rec.Code, rec.Body.String())
	}

	const oldPass = "oldpassword1"
	hash, err := bcrypt.GenerateFromPassword([]byte(oldPass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := pg.SetSetting(authPassKey, string(hash)); err != nil {
		t.Fatalf("store password: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"wrong-username", `{"username":"admin","password":"` + oldPass + `"}`, http.StatusUnauthorized},
		{"wrong-password", `{"username":"ARTEX","password":"nope-nope"}`, http.StatusUnauthorized},
		{"bad-json", `{`, http.StatusBadRequest},
	} {
		t.Run("login-"+tc.name, func(t *testing.T) {
			rec := raw(http.MethodPost, "/api/auth/login", tc.body, false)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	rec = raw(http.MethodPost, "/api/auth/login", `{"username":"ARTEX","password":"`+oldPass+`"}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	crudJSON(t, rec, &login)
	if login.Token == "" || !verifyJWT(login.Token, s.jwtKey) {
		t.Fatalf("login token invalid: %q", login.Token)
	}

	// Change-password lives under /api/auth/*, which requireAuth exempts, so the
	// handler validates the token itself.
	rec = raw(http.MethodPost, "/api/auth/change-password", `{"old_password":"`+oldPass+`","new_password":"newpassword1"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("change without token status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty-new", `{"old_password":"` + oldPass + `"}`, http.StatusBadRequest},
		{"short-new", `{"old_password":"` + oldPass + `","new_password":"short"}`, http.StatusBadRequest},
		{"bad-json", `{`, http.StatusBadRequest},
		{"wrong-old", `{"old_password":"wrong-one","new_password":"newpassword1"}`, http.StatusUnauthorized},
	} {
		t.Run("change-"+tc.name, func(t *testing.T) {
			rec := raw(http.MethodPost, "/api/auth/change-password", tc.body, true)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	const newPass = "newpassword1"
	rec = raw(http.MethodPost, "/api/auth/change-password", `{"old_password":"`+oldPass+`","new_password":"`+newPass+`"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("change status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The change must be persisted: the old password stops working and the new
	// one authenticates.
	rec = raw(http.MethodPost, "/api/auth/login", `{"username":"ARTEX","password":"`+oldPass+`"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: status=%d", rec.Code)
	}
	rec = raw(http.MethodPost, "/api/auth/login", `{"username":"ARTEX","password":"`+newPass+`"}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("new password rejected: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// llmTestSSE is a minimal well-formed Anthropic stream that yields a single
// "OK" text delta — the wire shape TestConnection expects on the happy path.
const llmTestSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestLLMTestRoute(t *testing.T) {
	_, s := newTestStack(t)
	// Pin the global fallback key: an earlier test may have configured a real
	// provider, and this case asserts the "no key anywhere" branch.
	s.cfgMu.Lock()
	s.llmCfg.APIKey = ""
	s.cfgMu.Unlock()

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return crudDo(t, s, http.MethodPost, "/api/llm/test", body)
	}
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out map[string]any
		crudJSON(t, rec, &out)
		return out
	}

	// No key anywhere → 200 with ok=false (the UI shows the reason inline).
	out := decode(post(`{"provider":"anthropic","model":"claude-test"}`))
	if out["ok"] != false || !strings.Contains(fmt.Sprint(out["error"]), "API Key") {
		t.Fatalf("missing key payload = %+v", out)
	}

	// Bad JSON is a request error, not a test result.
	if rec := post(`{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d body=%s", rec.Code, rec.Body.String())
	}

	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, llmTestSSE)
	}))
	defer sse.Close()

	out = decode(post(`{"provider":"anthropic","model":"claude-test","base_url":"` + sse.URL + `","api_key":"k","streaming":true}`))
	if out["ok"] != true || out["reply"] != "OK" {
		t.Fatalf("happy path payload = %+v", out)
	}

	// A stored profile supplies the key when the form leaves it blank: keys are
	// never echoed back to the browser, so this is the only way to retest one.
	pid, err := s.m.pg.SaveProfile(&db.LLMProfile{
		Name: "sweep-llm-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		// format anthropic + explicit base URL keeps the request on the fake server
		Format: "anthropic", Model: "claude-test", BaseURL: sse.URL, APIKey: "k",
	})
	if err != nil {
		t.Fatalf("save profile: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteProfile(pid) })

	out = decode(post(`{"provider":"anthropic","model":"claude-test","base_url":"` + sse.URL + `","profile_id":` + strconv.FormatInt(pid, 10) + `,"streaming":true}`))
	if out["ok"] != true {
		t.Fatalf("profile key fallback failed: %+v", out)
	}

	// Dead endpoint: a transport error is reported as ok=false, never a 5xx —
	// the route answers "the config does not work" rather than "the server broke".
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	out = decode(post(`{"provider":"anthropic","model":"claude-test","base_url":"` + deadURL + `","api_key":"k"}`))
	if out["ok"] != false || out["error"] == "" {
		t.Fatalf("dead endpoint payload = %+v", out)
	}

	// Global config key is the last fallback.
	s.cfgMu.Lock()
	s.llmCfg.APIKey = "global-key"
	s.cfgMu.Unlock()
	out = decode(post(`{"provider":"anthropic","model":"claude-test","base_url":"` + sse.URL + `","streaming":true}`))
	if out["ok"] != true {
		t.Fatalf("global key fallback failed: %+v", out)
	}
}

func TestVisibilityToggleRoute(t *testing.T) {
	m, s := newTestStack(t)
	key := "sweep_vis_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	var agentID int64
	if err := m.pg.QueryRow(`INSERT INTO agents(key,name,role,builtin,enabled) VALUES ($1,'可见性测试','assistant',false,true) RETURNING id`, key).Scan(&agentID); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM agents WHERE id=$1`, agentID) })

	toggle := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return crudDo(t, s, http.MethodPost, "/api/visibility/toggle", body)
	}

	// agent_id is declared `json:"agent_id,string"`: ids travel as strings so
	// int64 survives JSON in the browser.
	rec := toggle(`{"agent_id":"` + strconv.FormatInt(agentID, 10) + `","kind":"mcp","resource_id":4242,"visible":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	var n int
	if err := m.pg.QueryRow(`SELECT count(*) FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=4242`, agentID).Scan(&n); err != nil {
		t.Fatalf("count visibility: %v", err)
	}
	if n != 1 {
		t.Fatalf("visibility rows=%d want 1", n)
	}

	// visible=false deletes the row instead of storing enabled=false.
	rec = toggle(`{"agent_id":"` + strconv.FormatInt(agentID, 10) + `","kind":"mcp","resource_id":4242,"visible":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := m.pg.QueryRow(`SELECT count(*) FROM agent_visibility WHERE agent_id=$1 AND resource_kind='mcp' AND resource_id=4242`, agentID).Scan(&n); err != nil {
		t.Fatalf("count visibility: %v", err)
	}
	if n != 0 {
		t.Fatalf("visibility rows=%d want 0", n)
	}

	if rec := toggle(`{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status=%d body=%s", rec.Code, rec.Body.String())
	}
	// resource_kind is constrained to 'mcp' in the schema; the failure surfaces
	// as a 500 from the write rather than being silently ignored.
	rec = toggle(`{"agent_id":"` + strconv.FormatInt(agentID, 10) + `","kind":"skill","resource_id":1,"visible":true}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad kind status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFindingTreeAndActiveRetestRoutes(t *testing.T) {
	m, s := newTestStack(t)
	if _, err := m.pg.AddFinding(0, 0, "sweep-tree", "资产树测试漏洞", "high", "summary", "evidence", "worker", nil); err != nil {
		t.Fatalf("add finding: %v", err)
	}

	rec := crudDo(t, s, http.MethodGet, "/api/exploration/findings/asset-tree", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset tree status=%d body=%s", rec.Code, rec.Body.String())
	}
	var tree json.RawMessage
	crudJSON(t, rec, &tree)
	if len(tree) == 0 {
		t.Fatal("asset tree body is empty")
	}

	rec = crudDo(t, s, http.MethodGet, "/api/exploration/findings/retests/active", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("active retests status=%d body=%s", rec.Code, rec.Body.String())
	}
	var active struct {
		Retests []any `json:"retests"`
	}
	crudJSON(t, rec, &active)
	if active.Retests == nil {
		t.Fatal("retests must be an array, not null")
	}

	// Filter plumbing is shared with the flat list, so an unknown filter value
	// is normalised away rather than turning into a 400.
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/findings/asset-tree?severity=bogus&asset_scope=root_domain", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered asset tree status=%d body=%s", rec.Code, rec.Body.String())
	}
}
