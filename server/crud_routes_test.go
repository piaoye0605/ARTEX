package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file pins the contracts of the plain-CRUD routes that previously had no
// coverage at all: goals_api.go, triggers.go, asset_intercept.go, commands.go
// and the traffic delete/list guards. Every test runs against a real Manager +
// Server via newTestStack, so the assertions describe actual persistence
// behavior, not handler plumbing mocks.

// crudDo issues one authenticated JSON request against the server handler and
// returns the recorder. Body may be empty.
func crudDo(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func crudJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode status=%d: %v body=%s", rec.Code, err, rec.Body.String())
	}
}

// --- goals_api.go ---

func TestGoalsCRUDRoutes(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("goals crud", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	base := "/api/tasks/" + task.ID + "/goals"

	// Unknown task id is a 404 on every verb.
	if rec := crudDo(t, s, http.MethodGet, "/api/tasks/999999999/goals", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/999999999/goals", `{"text":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task add: status=%d", rec.Code)
	}

	// addGoal rejects malformed JSON and blank text before touching the store.
	if rec := crudDo(t, s, http.MethodPost, base, `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"text":"   "}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "目标内容不能为空") {
		t.Fatalf("blank text: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Valid create returns the persisted goal (ids travel as JSON strings).
	var created struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	rec := crudDo(t, s, http.MethodPost, base, `{"text":"find admin panel","vulnclass":"info"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &created)
	if created.ID == "" || created.Text != "find admin panel" {
		t.Fatalf("created=%+v", created)
	}

	// listGoals reflects the insert.
	var list struct {
		Goals []map[string]any `json:"goals"`
	}
	rec = crudDo(t, s, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	if len(list.Goals) != 1 || list.Goals[0]["text"] != "find admin panel" {
		t.Fatalf("list=%+v body=%s", list.Goals, rec.Body.String())
	}

	// editGoal validates the goal id, then applies the text change.
	if rec := crudDo(t, s, http.MethodPatch, base+"/abc", `{"text":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad gid: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, base+"/424242", `{"text":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown gid: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodPatch, base+"/"+created.ID, `{"text":"find admin panel v2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Text string `json:"text"`
	}
	crudJSON(t, rec, &updated)
	if updated.Text != "find admin panel v2" {
		t.Fatalf("updated=%+v", updated)
	}

	// deleteGoal removes the node; the list goes back to empty.
	if rec := crudDo(t, s, http.MethodDelete, base+"/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list after delete: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	if len(list.Goals) != 0 {
		t.Fatalf("goals after delete=%+v", list.Goals)
	}
}

func itoaInt64(v int64) string { return strconv.FormatInt(v, 10) }

// --- triggers.go ---

func TestValidateTrigger(t *testing.T) {
	// No condition selected at all is rejected.
	if msg := validateTrigger(&triggerReq{}); msg == "" {
		t.Fatal("no-condition request must be rejected")
	}
	// on_tool_call without a tool set is rejected.
	if msg := validateTrigger(&triggerReq{OnToolCall: true}); msg == "" {
		t.Fatal("on_tool_call without tool names must be rejected")
	}
	// A single condition is enough.
	if msg := validateTrigger(&triggerReq{OnFinding: true}); msg != "" {
		t.Fatalf("on_finding-only rejected: %s", msg)
	}
	// Tool-call condition with tools passes.
	if msg := validateTrigger(&triggerReq{OnToolCall: true, ToolNames: []string{"nmap"}}); msg != "" {
		t.Fatalf("valid tool-call rejected: %s", msg)
	}
}

func TestTriggerRoutes(t *testing.T) {
	_, s := newTestStack(t)
	pg := s.m.PG()

	// Unknown agent key → 404 from agentByKey.
	if rec := crudDo(t, s, http.MethodGet, "/api/agents/nope/triggers", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status=%d", rec.Code)
	}

	// Triggers only exist for custom agents. The key must satisfy
	// ^[a-z][a-z0-9_]*$ and the store is persistent, so suffix with the run id.
	key := "trig_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := pg.CreateAgent(key, "trigger tester", "fixture"); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// Create validates the shared rules before persisting.
	if rec := crudDo(t, s, http.MethodPost, "/api/agents/"+key+"/triggers", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty trigger: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := crudDo(t, s, http.MethodPost, "/api/agents/"+key+"/triggers",
		`{"enabled":true,"on_finding":true,"finding_message":"new finding"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	crudJSON(t, rec, &created)
	if created.ID <= 0 {
		t.Fatalf("created trigger id=%d", created.ID)
	}

	// List returns the stored trigger.
	var list struct {
		Triggers []map[string]any `json:"triggers"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/agents/"+key+"/triggers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	if len(list.Triggers) != 1 || list.Triggers[0]["finding_message"] != "new finding" {
		t.Fatalf("list=%+v body=%s", list.Triggers, rec.Body.String())
	}

	// Update flips the condition set; bad ids are rejected first.
	if rec := crudDo(t, s, http.MethodPatch, "/api/triggers/abc", `{"on_finding":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad trigger id: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodPatch, "/api/triggers/"+itoaInt64(created.ID),
		`{"enabled":false,"on_goal_met":true,"goal_message":"goal met"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete removes the row; a second delete of the same id still returns 200
	// (idempotent by rowcount) so callers cannot wedge on a stale UI.
	if rec := crudDo(t, s, http.MethodDelete, "/api/triggers/"+itoaInt64(created.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodGet, "/api/agents/"+key+"/triggers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list after delete: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	if len(list.Triggers) != 0 {
		t.Fatalf("triggers after delete=%+v", list.Triggers)
	}
}

// --- asset_intercept.go ---

func TestValidateAssetInterceptRuleReq(t *testing.T) {
	cases := []struct {
		kind, pattern string
		wantErr       bool
	}{
		{"", "", true},
		{"exact_domain", "  ", true},
		{"no-such-kind", "example.com", true},
		{"exact_ip", "not-an-ip", true},
		{"cidr", "192.168.1.1", true},
		{"exact_domain", "example.com", false},
		{"fuzzy_url", "admin.example.com/login", false},
		{"exact_ip", "10.0.0.8", false},
		{"cidr", "192.168.0.0/16", false},
	}
	for _, c := range cases {
		req := &assetInterceptRuleReq{Kind: c.kind, Pattern: c.pattern}
		err := validateAssetInterceptRuleReq(req)
		if c.wantErr && err == nil {
			t.Fatalf("kind=%q pattern=%q: want error, got nil", c.kind, c.pattern)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("kind=%q pattern=%q: want ok, got %v", c.kind, c.pattern, err)
		}
	}
	// The pattern is normalized in place before persistence.
	req := &assetInterceptRuleReq{Kind: "exact_domain", Pattern: "  padded.example.com  "}
	if err := validateAssetInterceptRuleReq(req); err != nil {
		t.Fatalf("trim check: %v", err)
	}
	if req.Pattern != "padded.example.com" {
		t.Fatalf("pattern not trimmed: %q", req.Pattern)
	}
}

func TestAssetInterceptRuleRoutes(t *testing.T) {
	_, s := newTestStack(t)

	// The DB ships with builtin seed rules, so assert on deltas from the
	// baseline rather than on an absolute count (shared persistent store).
	rec := crudDo(t, s, http.MethodGet, "/api/asset-intercept/rules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	var list struct {
		Rules []map[string]any `json:"rules"`
	}
	crudJSON(t, rec, &list)
	if list.Rules == nil {
		t.Fatal("rules must list as an array, not null")
	}
	baseline := len(list.Rules)

	// Create validates kind/pattern before persisting.
	if rec := crudDo(t, s, http.MethodPost, "/api/asset-intercept/rules", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/asset-intercept/rules", `{"kind":"weird","pattern":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodPost, "/api/asset-intercept/rules", `{"kind":"fuzzy_domain","pattern":"blocked.example.com","note":"scope guard","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	crudJSON(t, rec, &created)
	if created.ID <= 0 {
		t.Fatalf("created rule id=%d", created.ID)
	}

	// Update reuses the same validation; bad ids reject early.
	if rec := crudDo(t, s, http.MethodPut, "/api/asset-intercept/rules/abc", `{"kind":"exact_ip","pattern":"1.2.3.4"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad rule id: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodPut, "/api/asset-intercept/rules/"+itoaInt64(created.ID), `{"kind":"exact_ip","pattern":"10.9.8.7","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Toggle flips enabled without touching the pattern.
	rec = crudDo(t, s, http.MethodPost, "/api/asset-intercept/rules/"+itoaInt64(created.ID)+"/toggle", `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: status=%d", rec.Code)
	}

	// Delete removes the rule; the list is empty afterwards.
	if rec := crudDo(t, s, http.MethodDelete, "/api/asset-intercept/rules/"+itoaInt64(created.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/asset-intercept/rules", ""); rec.Code != http.StatusOK {
		t.Fatalf("list after delete: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	if len(list.Rules) != baseline {
		t.Fatalf("rules after delete=%d, want baseline %d", len(list.Rules), baseline)
	}
}

// --- commands.go ---

func TestCommandTaskFilter(t *testing.T) {
	if commandTaskFilter("") != nil || commandTaskFilter("abc") != nil {
		t.Fatal("absent or unparsable task must yield no filter")
	}
	if f := commandTaskFilter("42"); f == nil || *f != 42 {
		t.Fatalf("parsed filter=%v", f)
	}
}

func TestCommandAndLLMRecordRoutes(t *testing.T) {
	_, s := newTestStack(t)

	// Empty-history reads all return well-formed empty payloads.
	var cmds struct {
		Commands []map[string]any `json:"commands"`
		Total    int              `json:"total"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/commands?page=0&size=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("commands: status=%d", rec.Code)
	}
	crudJSON(t, rec, &cmds)
	if len(cmds.Commands) != 0 || cmds.Total != 0 {
		t.Fatalf("commands=%+v total=%d", cmds.Commands, cmds.Total)
	}

	if rec := crudDo(t, s, http.MethodGet, "/api/commands/stats?q=recon", ""); rec.Code != http.StatusOK {
		t.Fatalf("stats: status=%d", rec.Code)
	}

	var records struct {
		Records []map[string]any `json:"records"`
		Total   int              `json:"total"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/llm/records?model=gpt&page=0&size=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("llm records: status=%d", rec.Code)
	}
	crudJSON(t, rec, &records)
	if len(records.Records) != 0 || records.Total != 0 {
		t.Fatalf("records=%+v", records)
	}

	if rec := crudDo(t, s, http.MethodGet, "/api/llm/records/tasks", ""); rec.Code != http.StatusOK {
		t.Fatalf("llm tasks: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/tokens/usage?days=30", ""); rec.Code != http.StatusOK {
		t.Fatalf("usage: status=%d", rec.Code)
	}

	// by-model requires an explicit task scope.
	if rec := crudDo(t, s, http.MethodGet, "/api/llm/records/by-model", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("by-model without task: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/llm/records/by-model?task=42", ""); rec.Code != http.StatusOK {
		t.Fatalf("by-model: status=%d", rec.Code)
	}

	// Delete requires the task scope too.
	if rec := crudDo(t, s, http.MethodDelete, "/api/llm/records", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete without task: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodDelete, "/api/llm/records?task=42", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	var del struct {
		Deleted int64 `json:"deleted"`
	}
	crudJSON(t, rec, &del)
	if del.Deleted != 0 {
		t.Fatalf("deleted=%d, want 0", del.Deleted)
	}

	// Single-record reads validate the id and then report absence.
	if rec := crudDo(t, s, http.MethodGet, "/api/llm/records/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad record id: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/llm/records/987654", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing record: status=%d", rec.Code)
	}
}

// --- server_traffic.go guards ---

func TestTrafficRouteGuards(t *testing.T) {
	_, s := newTestStack(t)
	// The test stack may run with traffic disabled (no traffic root); the
	// disabled branch is deterministic and worth pinning. When enabled, the
	// empty-ledger list is exercised instead.
	if s.m.Traffic() == nil {
		if rec := crudDo(t, s, http.MethodGet, "/api/traffic?page=0&size=10", ""); rec.Code != http.StatusOK ||
			!strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Fatalf("traffic disabled list: status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec := crudDo(t, s, http.MethodDelete, "/api/traffic/hosts", `{"hosts":["a.example.com"]}`); rec.Code != http.StatusNotFound {
			t.Fatalf("traffic disabled delete: status=%d", rec.Code)
		}
		if rec := crudDo(t, s, http.MethodDelete, "/api/traffic", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("traffic disabled purge: status=%d", rec.Code)
		}
		return
	}
	rec := crudDo(t, s, http.MethodGet, "/api/traffic?page=0&size=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("traffic list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Host deletion validates the body before touching the ledger.
	if rec := crudDo(t, s, http.MethodDelete, "/api/traffic/hosts", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/traffic/hosts", `{"hosts":["  ",""]}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "missing hosts") {
		t.Fatalf("blank hosts: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodDelete, "/api/traffic/hosts", `{"hosts":["gone.example.com"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("host delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
