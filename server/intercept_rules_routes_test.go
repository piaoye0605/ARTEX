package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// This file pins the intercept control plane — the routes that configure how
// tool calls get judged. Before this file the entire rule CRUD surface, the
// pending-approval queue, the global tool scope and the LLM fallback-judge
// config/usage read model were untested at the HTTP layer (every one of those
// handlers sat at 0% coverage), so a wiring change could silently disable the
// approval system without failing any test.
//
// Contract notes learned while writing these tests:
//   - Rule patterns are global: a rule with action=ask and a broad pattern would
//     slow every later test that runs a tool call. Probe rules therefore use a
//     never-matching sentinel pattern and are deleted as soon as they are no
//     longer needed (cleanup is registered immediately, not at the end).
//   - Tool scope and judge config live in the settings KV table, i.e. they are
//     database-global. Every test snapshots the keys it touches and restores
//     them, otherwise a stray "enabled_tools: []" would leak into other tests.

// snapshotSetting restores one settings key to its pre-test state when the test
// ends, including the "key did not exist" case (which a plain SetSetting cannot
// express).
func snapshotSetting(t *testing.T, s *Server, key string) {
	t.Helper()
	pg := s.m.PG()
	val, ok, err := pg.GetSetting(key)
	if err != nil {
		t.Fatalf("read setting %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = pg.SetSetting(key, val)
			return
		}
		_, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
	})
}

// clearSetting removes a key so the code under test sees "never configured".
func clearSetting(t *testing.T, s *Server, key string) {
	t.Helper()
	if _, err := s.m.PG().Exec(`DELETE FROM settings WHERE key=$1`, key); err != nil {
		t.Fatalf("clear setting %s: %v", key, err)
	}
}

// --- /api/intercept/rules ---

func TestInterceptRuleCRUDRoutes(t *testing.T) {
	m, s := newTestStack(t)
	pg := m.PG()
	stamp := time.Now().UnixNano()

	// A pattern that cannot match any real tool name or input, so the rule can
	// exist in the shared DB without perturbing other tests that run tools.
	probePattern := fmt.Sprintf("__intercept_contract_probe_%d__", stamp)
	name := fmt.Sprintf("contract-rule-%d", stamp)

	var created db.InterceptRule
	body := `{"name":"` + name + `","enabled":true,"priority":7,` +
		`"match_target":"tool_name","match_type":"regex","pattern":"` + probePattern + `",` +
		`"action":"ask","message":"需要人工确认","timeout_enabled":true,` +
		`"timeout_seconds":30,"timeout_action":"deny"}`
	rec := crudDo(t, s, http.MethodPost, "/api/intercept/rules", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &created)
	if created.ID <= 0 {
		t.Fatalf("create rule returned id=%d body=%s", created.ID, rec.Body.String())
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM intercept_rules WHERE id=$1`, created.ID)
		s.m.interceptor.Invalidate()
	})
	// The handler echoes every persisted column, not just the id.
	if created.Name != name || created.Priority != 7 ||
		created.MatchTarget != "tool_name" || created.MatchType != "regex" ||
		created.Pattern != probePattern || created.Action != "ask" ||
		created.Message != "需要人工确认" || !created.Enabled ||
		!created.TimeoutEnabled || created.TimeoutSeconds != 30 || created.TimeoutAction != "deny" {
		t.Fatalf("create rule echo mismatch: %+v", created)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not stamped: %+v", created)
	}

	// List contains the row (other rules may exist — assert containment only).
	var listed struct {
		Rules []db.InterceptRule `json:"rules"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/intercept/rules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list rules: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &listed)
	found := false
	for _, row := range listed.Rules {
		if row.ID == created.ID {
			found = true
			if row.Pattern != probePattern {
				t.Fatalf("listed pattern=%q want %q", row.Pattern, probePattern)
			}
		}
	}
	if !found {
		t.Fatalf("created rule %d missing from list (%d rows)", created.ID, len(listed.Rules))
	}

	// Update replaces all editable fields; the regex is recompiled by callers,
	// so validation runs again on the new pattern.
	updatedName := name + "-v2"
	rec = crudDo(t, s, http.MethodPut, "/api/intercept/rules/"+strconv.FormatInt(created.ID, 10),
		`{"name":"`+updatedName+`","enabled":false,"priority":1,`+
			`"match_target":"tool_input","match_type":"string","pattern":"curl ",`+
			`"action":"deny","message":"改判拒绝","timeout_enabled":false,`+
			`"timeout_seconds":0,"timeout_action":"allow"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var updated db.InterceptRule
	crudJSON(t, rec, &updated)
	if updated.ID != created.ID || updated.Name != updatedName || updated.Enabled ||
		updated.Priority != 1 || updated.MatchTarget != "tool_input" ||
		updated.MatchType != "string" || updated.Pattern != "curl " ||
		updated.Action != "deny" || updated.Message != "改判拒绝" ||
		updated.TimeoutEnabled || updated.TimeoutSeconds != 0 || updated.TimeoutAction != "allow" {
		t.Fatalf("update rule echo mismatch: %+v", updated)
	}

	// Toggle flips the enabled flag only.
	rec = crudDo(t, s, http.MethodPost, "/api/intercept/rules/"+strconv.FormatInt(created.ID, 10)+"/toggle",
		`{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var toggled map[string]any
	crudJSON(t, rec, &toggled)
	if toggled["ok"] != true || toggled["enabled"] != true {
		t.Fatalf("toggle response=%+v", toggled)
	}
	var afterToggle db.InterceptRule
	for _, row := range mustListRules(t, s) {
		if row.ID == created.ID {
			afterToggle = row
		}
	}
	if !afterToggle.Enabled || afterToggle.Name != updatedName {
		t.Fatalf("toggle did not persist: %+v", afterToggle)
	}

	// Delete reports the id it removed, and the row disappears from the list.
	rec = crudDo(t, s, http.MethodDelete, "/api/intercept/rules/"+strconv.FormatInt(created.ID, 10), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var deleted map[string]any
	crudJSON(t, rec, &deleted)
	if int64(deleted["deleted"].(float64)) != created.ID {
		t.Fatalf("delete response=%+v", deleted)
	}
	for _, row := range mustListRules(t, s) {
		if row.ID == created.ID {
			t.Fatalf("rule %d still listed after delete", created.ID)
		}
	}

	// Idempotency of the "no such row" paths, pinned as-is: DELETE and toggle are
	// plain UPDATEs (0 rows is not an error), while PUT uses RETURNING and thus
	// surfaces sql.ErrNoRows as a 500.
	if rec := crudDo(t, s, http.MethodDelete, "/api/intercept/rules/999999999", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete unknown rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/intercept/rules/999999999/toggle", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("toggle unknown rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/rules/999999999",
		`{"name":"ghost","match_target":"tool_name","match_type":"string","pattern":"x","action":"allow"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("update unknown rule: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Non-numeric ids never reach the store.
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/rules/abc",
		`{"name":"ghost","match_target":"tool_name","match_type":"string","pattern":"x","action":"allow"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("update bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/intercept/rules/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func mustListRules(t *testing.T, s *Server) []db.InterceptRule {
	t.Helper()
	rec := crudDo(t, s, http.MethodGet, "/api/intercept/rules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list rules: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Rules []db.InterceptRule `json:"rules"`
	}
	crudJSON(t, rec, &listed)
	return listed.Rules
}

// TestInterceptRuleValidation pins every rejection branch of
// validateInterceptRuleReq: each one is a 400 before the store is touched.
func TestInterceptRuleValidation(t *testing.T) {
	_, s := newTestStack(t)

	valid := map[string]any{
		"name":         "validator",
		"match_target": "tool_name",
		"match_type":   "string",
		"pattern":      "Bash",
		"action":       "allow",
	}
	mutate := func(key string, value any) string {
		clone := map[string]any{}
		for k, v := range valid {
			clone[k] = v
		}
		if value == nil {
			delete(clone, key)
		} else {
			clone[key] = value
		}
		b, err := json.Marshal(clone)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed json", `{not-json`, "invalid character"},
		{"missing name", mutate("name", nil), "name 不能为空"},
		{"blank name", mutate("name", ""), "name 不能为空"},
		{"bad match_target", mutate("match_target", "tool_output"), "match_target 必须是 tool_name 或 tool_input"},
		{"missing match_target", mutate("match_target", nil), "match_target 必须是"},
		{"bad match_type", mutate("match_type", "glob"), "match_type 必须是 string 或 regex"},
		{"missing pattern", mutate("pattern", nil), "pattern 不能为空"},
		{"bad action", mutate("action", "block"), "action 必须是 allow、deny 或 ask"},
		{"bad regex", mutate("match_type", "regex"), "pattern 不是有效正则"},
	}
	// The regex branch needs a pattern that does not compile.
	badRegex := map[string]any{
		"name": "validator", "match_target": "tool_name", "match_type": "regex",
		"pattern": "([a-z", "action": "allow",
	}
	if b, err := json.Marshal(badRegex); err == nil {
		cases[len(cases)-1].body = string(b)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := crudDo(t, s, http.MethodPost, "/api/intercept/rules", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body=%s want substring %q", rec.Body.String(), tc.want)
			}
		})
	}

	// A string match_type is not compiled, so regex metacharacters are legal.
	rec := crudDo(t, s, http.MethodPost, "/api/intercept/rules", mutate("pattern", "file://([a-z"))
	if rec.Code != http.StatusOK {
		t.Fatalf("string pattern with metacharacters: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created db.InterceptRule
	crudJSON(t, rec, &created)
	t.Cleanup(func() {
		_, _ = s.m.PG().Exec(`DELETE FROM intercept_rules WHERE id=$1`, created.ID)
		s.m.interceptor.Invalidate()
	})
}

// --- /api/intercept/pending ---

func TestInterceptPendingRoutes(t *testing.T) {
	m, s := newTestStack(t)
	pg := m.PG()

	// Seed the row exactly like the ASK path does, including the audit payload:
	// the decide transition patches that payload rather than the reason column.
	audit := &db.InterceptAudit{
		RunID: "run-contract", ToolUseID: "toolu-contract", Correlation: "exact",
		InputDigest: "sha256:probe", UserMessage: "清理测试文件",
		InitialAction: "ask", InitialReason: "命中高危命令规则",
		ExecutionStatus: "not_executed", CapturedAt: time.Now().UTC(),
	}
	pendingID, err := pg.CreateInterceptPending(0, 0, "", "worker", "Bash",
		[]byte(`{"command":"rm -rf /tmp/probe"}`), "命中高危命令规则", audit)
	if err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM intercept_pending WHERE id=$1`, pendingID)
	})

	var queue struct {
		Pending []db.InterceptPending `json:"pending"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/intercept/pending", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &queue)
	seen := false
	for _, row := range queue.Pending {
		if row.ID == pendingID {
			seen = true
			if row.Status != "pending" || row.ToolName != "Bash" || row.AgentName != "worker" {
				t.Fatalf("pending row mismatch: %+v", row)
			}
			if row.RuleID != nil {
				t.Fatalf("rule-less judge pending should have NULL rule_id: %+v", row.RuleID)
			}
			if row.ConversationID != nil || row.TaskID != nil {
				t.Fatalf("background pending should have NULL conversation/task: %+v", row)
			}
		}
	}
	if !seen {
		t.Fatalf("seeded pending %d missing from queue (%d rows)", pendingID, len(queue.Pending))
	}

	// Single-row read returns the same record the queue lists.
	var one db.InterceptPending
	rec = crudDo(t, s, http.MethodGet, "/api/intercept/pending/"+strconv.FormatInt(pendingID, 10), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &one)
	if one.ID != pendingID || one.Reason != "命中高危命令规则" || one.DecisionSource != "unknown" {
		t.Fatalf("pending detail mismatch: %+v", one)
	}
	// Compare the payload semantically: jsonb does not preserve source spacing.
	var input map[string]any
	if err := json.Unmarshal(one.ToolInput, &input); err != nil {
		t.Fatalf("tool_input not json: %v (%s)", err, one.ToolInput)
	}
	if input["command"] != "rm -rf /tmp/probe" {
		t.Fatalf("tool_input roundtrip=%s", one.ToolInput)
	}

	if rec := crudDo(t, s, http.MethodGet, "/api/intercept/pending/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("get pending bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/intercept/pending/999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get pending unknown: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Decide only accepts the two terminal verbs.
	if rec := crudDo(t, s, http.MethodPost, "/api/intercept/pending/"+strconv.FormatInt(pendingID, 10)+"/decide",
		`{"decision":"maybe"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("decide bad verb: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/intercept/pending/abc/decide", `{"decision":"allowed"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("decide bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = crudDo(t, s, http.MethodPost, "/api/intercept/pending/"+strconv.FormatInt(pendingID, 10)+"/decide",
		`{"decision":"allowed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide allowed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var decided db.InterceptPending
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/pending/"+strconv.FormatInt(pendingID, 10), ""), &decided)
	if decided.Status != "allowed" || decided.DecidedAt == nil {
		t.Fatalf("decided row=%+v", decided)
	}
	// Contract: `reason` keeps WHY the call was escalated (rule message / model
	// verdict); the human decision lands in the audit payload instead, so the
	// history page can show both. Overwriting `reason` would erase the trigger.
	if decided.Reason != "命中高危命令规则" {
		t.Fatalf("reason was overwritten: %q", decided.Reason)
	}
	var decisionReason, effectiveAction, executionStatus string
	if err := pg.QueryRow(`SELECT audit->>'decision_reason', audit->>'effective_action',
		audit->>'execution_status' FROM intercept_pending WHERE id=$1`, pendingID).
		Scan(&decisionReason, &effectiveAction, &executionStatus); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if decisionReason != "人工允许执行" || effectiveAction != "allow" || executionStatus != "awaiting_result" {
		t.Fatalf("audit after allow: reason=%q action=%q execution=%q",
			decisionReason, effectiveAction, executionStatus)
	}

	// Deciding twice is a conflict, not a silent second write — this is what the
	// UI relies on to detect a stale approval card.
	rec = crudDo(t, s, http.MethodPost, "/api/intercept/pending/"+strconv.FormatInt(pendingID, 10)+"/decide",
		`{"decision":"denied"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decide twice: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var conflict db.InterceptPending
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/pending/"+strconv.FormatInt(pendingID, 10), ""), &conflict)
	if conflict.Status != "allowed" {
		t.Fatalf("second decide overwrote status: %+v", conflict)
	}
}

// --- /api/intercept/tool-config ---

func TestInterceptToolConfigRoutes(t *testing.T) {
	m, s := newTestStack(t)
	const key = "intercept_enabled_tools"
	snapshotSetting(t, s, key)
	clearSetting(t, s, key)
	m.interceptor.Invalidate()

	// Unset → the hard-coded default set (Bash/Write/... enter the rule path).
	var cfg struct {
		EnabledTools []string `json:"enabled_tools"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/intercept/tool-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get tool config: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &cfg)
	defaults := map[string]bool{}
	for _, n := range cfg.EnabledTools {
		defaults[n] = true
	}
	if !defaults["Bash"] || !defaults["Write"] || !defaults["WebFetch"] {
		t.Fatalf("default enabled tools=%v", cfg.EnabledTools)
	}
	if !m.interceptor.IsToolEnabled("Bash") {
		t.Fatal("defaults not wired into the interceptor cache")
	}

	// Replace the set; the response is a bare ok and the read-back is ordered.
	rec = crudDo(t, s, http.MethodPut, "/api/intercept/tool-config", `{"enabled_tools":["Bash","Write","CustomProbe"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set tool config: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/tool-config", ""), &cfg)
	if len(cfg.EnabledTools) != 3 || cfg.EnabledTools[0] != "Bash" ||
		cfg.EnabledTools[1] != "Write" || cfg.EnabledTools[2] != "CustomProbe" {
		t.Fatalf("roundtrip enabled tools=%v", cfg.EnabledTools)
	}
	// The write must invalidate the in-memory set, not just the DB row.
	if !m.interceptor.IsToolEnabled("CustomProbe") || m.interceptor.IsToolEnabled("Edit") {
		t.Fatalf("cache not invalidated: CustomProbe=%v Edit=%v",
			m.interceptor.IsToolEnabled("CustomProbe"), m.interceptor.IsToolEnabled("Edit"))
	}

	// An empty body is an explicit "no tool enters the rule path", stored as [].
	rec = crudDo(t, s, http.MethodPut, "/api/intercept/tool-config", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear tool config: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/tool-config", ""), &cfg)
	if cfg.EnabledTools == nil || len(cfg.EnabledTools) != 0 {
		t.Fatalf("cleared enabled tools=%v (want empty, non-null)", cfg.EnabledTools)
	}
	if m.interceptor.IsToolEnabled("Bash") {
		t.Fatal("empty set still reports Bash as enabled")
	}

	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/tool-config", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// --- /api/intercept/judge ---

func TestInterceptJudgeConfigRoutes(t *testing.T) {
	_, s := newTestStack(t)
	keys := []string{
		"llm_judge_enabled", "llm_judge_profile_id", "llm_judge_prompt",
		"llm_judge_timeout_seconds", "llm_judge_fail_action",
		"llm_judge_ask_timeout_seconds", "llm_judge_ask_timeout_action",
	}
	for _, k := range keys {
		snapshotSetting(t, s, k)
		clearSetting(t, s, k)
	}

	var cfg intercept.JudgeConfig
	rec := crudDo(t, s, http.MethodGet, "/api/intercept/judge", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get judge config: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &cfg)
	if cfg.Enabled || cfg.ProfileID != 0 {
		t.Fatalf("default judge enabled/profile=%v/%d", cfg.Enabled, cfg.ProfileID)
	}
	// The unset prompt is resolved to the built-in template so the UI can prefill.
	if cfg.Prompt != intercept.DefaultJudgePrompt {
		t.Fatalf("default prompt is not the built-in template (len=%d)", len(cfg.Prompt))
	}
	if cfg.TimeoutSeconds != 15 || cfg.FailAction != "allow" ||
		cfg.AskTimeoutSeconds != 300 || cfg.AskTimeoutAction != "deny" {
		t.Fatalf("default judge timeouts=%+v", cfg)
	}

	// Enum guards: the model must not persist a verb the engine cannot honor.
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/judge",
		`{"fail_action":"ignore","ask_timeout_action":"deny"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad fail_action: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/judge",
		`{"fail_action":"ask","ask_timeout_action":"maybe"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ask_timeout_action: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/judge", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d body=%s", rec.Code, rec.Body.String())
	}

	custom := `{"enabled":true,"profile_id":42,"prompt":"你是本项目的审批裁判，只回答 JSON。",` +
		`"timeout_seconds":9,"fail_action":"ask","ask_timeout_seconds":45,"ask_timeout_action":"allow"}`
	rec = crudDo(t, s, http.MethodPut, "/api/intercept/judge", custom)
	if rec.Code != http.StatusOK {
		t.Fatalf("set judge config: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/judge", ""), &cfg)
	if !cfg.Enabled || cfg.ProfileID != 42 || cfg.TimeoutSeconds != 9 ||
		cfg.FailAction != "ask" || cfg.AskTimeoutSeconds != 45 || cfg.AskTimeoutAction != "allow" ||
		cfg.Prompt != "你是本项目的审批裁判，只回答 JSON。" {
		t.Fatalf("judge config roundtrip=%+v", cfg)
	}

	// Sending the built-in template back clears the override instead of storing a
	// frozen copy — that is what keeps future template updates flowing through.
	passthrough := `{"enabled":true,"profile_id":42,"prompt":` + strconv.Quote(intercept.DefaultJudgePrompt) + `,` +
		`"timeout_seconds":9,"fail_action":"ask","ask_timeout_seconds":45,"ask_timeout_action":"allow"}`
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/judge", passthrough); rec.Code != http.StatusOK {
		t.Fatalf("set template prompt: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if stored, ok, err := s.m.PG().GetSetting("llm_judge_prompt"); err != nil || !ok || stored != "" {
		t.Fatalf("template prompt should be stored empty, got ok=%v val=%q err=%v", ok, stored, err)
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/judge", ""), &cfg)
	if cfg.Prompt != intercept.DefaultJudgePrompt {
		t.Fatal("cleared override did not fall back to the built-in template")
	}

	// Out-of-range timeouts are clamped to defaults on read, not rejected on write.
	if rec := crudDo(t, s, http.MethodPut, "/api/intercept/judge",
		`{"fail_action":"allow","ask_timeout_action":"deny","timeout_seconds":0,"ask_timeout_seconds":-5}`); rec.Code != http.StatusOK {
		t.Fatalf("zero timeouts: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/intercept/judge", ""), &cfg)
	if cfg.TimeoutSeconds != 15 || cfg.AskTimeoutSeconds != 300 {
		t.Fatalf("zero timeouts not clamped: %+v", cfg)
	}
}

func TestInterceptJudgeUsageRoute(t *testing.T) {
	m, s := newTestStack(t)
	pg := m.PG()
	if pg == nil {
		t.Skip("postgres unavailable")
	}

	before, err := pg.JudgeUsageStats(30)
	if err != nil {
		t.Fatalf("baseline usage: %v", err)
	}
	// Totals are all-time and shared, so assert the delta this row contributes.
	model := fmt.Sprintf("judge-contract-%d", time.Now().UnixNano())
	if err := pg.InsertLLMUsage(&db.LLMUsage{
		Worker: "judge", Model: model, ProfileName: "judge", LatencyMs: 12,
		InputTokens: 123, OutputTokens: 45, Status: "ok",
	}); err != nil {
		t.Fatalf("insert judge usage: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM llm_usage WHERE worker='judge' AND model=$1`, model)
	})

	var usage db.JudgeUsage
	rec := crudDo(t, s, http.MethodGet, "/api/intercept/judge/usage?days=7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("judge usage: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &usage)
	if usage.Calls != before.Calls+1 || usage.InputTokens != before.InputTokens+123 ||
		usage.OutputTokens != before.OutputTokens+45 {
		t.Fatalf("usage delta mismatch: before=%+v after=%+v", before, usage)
	}
	today := time.Now().UTC().Format("2006-01-02")
	var day *db.JudgeDayUsage
	for i := range usage.Daily {
		if usage.Daily[i].Date == today {
			day = &usage.Daily[i]
		}
	}
	if day == nil {
		t.Fatalf("today (%s) missing from daily series: %+v", today, usage.Daily)
	}
	if day.Calls < 1 || day.InputTokens < 123 || day.OutputTokens < 45 {
		t.Fatalf("today bucket=%+v", *day)
	}

	// Non-numeric ?days falls back to the 30-day window instead of erroring.
	rec = crudDo(t, s, http.MethodGet, "/api/intercept/judge/usage?days=abc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("judge usage bad days: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var fallback db.JudgeUsage
	crudJSON(t, rec, &fallback)
	if fallback.Calls != usage.Calls || fallback.InputTokens != usage.InputTokens {
		t.Fatalf("bad days changed totals: %+v vs %+v", fallback, usage)
	}
}
