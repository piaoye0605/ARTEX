package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file pins the contracts of the agent management control plane
// (server_agents.go): custom-agent CRUD, the runtime config patch, and the
// per-agent visibility set. Everything runs against a real Manager so the
// assertions describe persistence rather than handler plumbing.
//
// Shared-DB discipline: custom agents are created with a unique key
// (artx_<tag>_<unixnano>) and removed in cleanup; built-in agents are only ever
// read or expected to be rejected, never mutated.

// agentProbeKey builds a DB-legal agent key: lowercase start, then lowercase
// letters / digits / underscores (mirrors the agents.key CHECK).
func agentProbeKey(tag string) string {
	return fmt.Sprintf("artx_%s_%d", tag, time.Now().UnixNano())
}

// agentDetailDTO mirrors the GET /api/agents/{key} envelope. Fields whose shape
// is not under test stay raw so a DTO reshape doesn't break unrelated asserts.
type agentDetailDTO struct {
	Agent struct {
		ID               string `json:"id"`
		Key              string `json:"key"`
		Name             string `json:"name"`
		Description      string `json:"description"`
		Role             string `json:"role"`
		Builtin          bool   `json:"builtin"`
		Enabled          bool   `json:"enabled"`
		MaxTurns         int    `json:"max_turns"`
		RunSecs          int    `json:"run_seconds"`
		WebSearch        bool   `json:"web_search"`
		InteractiveShell bool   `json:"interactive_shell"`
		LLMProfileID     *int64 `json:"llm_profile_id"`
		TriggerRunMode   string `json:"trigger_run_mode"`
		TriggerMergeMode string `json:"trigger_merge_mode"`
		TriggerMaxPar    int    `json:"trigger_max_parallel"`
	} `json:"agent"`
	Prompt     string          `json:"prompt"`
	Variables  json.RawMessage `json:"variables"`
	Versions   []any           `json:"versions"`
	Visibility struct {
		MCP   []int64  `json:"mcp"`
		Skill []string `json:"skill"`
	} `json:"visibility"`
	LLMProfiles []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Model     string `json:"model"`
		IsDefault bool   `json:"is_default"`
	} `json:"llm_profiles"`
	WrapupPrompt               string `json:"wrapup_prompt"`
	WrapupDefault              string `json:"wrapup_default"`
	TaskTimeoutWrapupSupported bool   `json:"task_timeout_wrapup_supported"`
}

func TestAgentCreateReadUpdateDeleteRoutes(t *testing.T) {
	m, s := newTestStack(t)
	key := agentProbeKey("crud")
	t.Cleanup(func() {
		_ = m.pg.DeleteAgent(key)
		_, _ = m.pg.Exec(`DELETE FROM triggers WHERE agent_key=$1`, key)
	})

	// Key/name validation runs before anything is written. The key regex is the
	// same one the DB CHECK enforces, so 400s here can't become 500s later.
	for _, bad := range []string{`{"key":"Artx_Upper","name":"x"}`, `{"key":"9leading","name":"x"}`, `{"key":"has-dash","name":"x"}`, `{"key":"","name":"x"}`} {
		if rec := crudDo(t, s, http.MethodPost, "/api/agents", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad key %s: status=%d body=%s", bad, rec.Code, rec.Body.String())
		}
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/agents", `{"key":"`+key+`","name":"   "}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "名称不能为空") {
		t.Fatalf("blank name: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/agents", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}

	// Create: custom agents are non-builtin 'assistant' rows, enabled by default.
	rec := crudDo(t, s, http.MethodPost, "/api/agents",
		`{"key":"`+key+`","name":"  契约探针  ","description":"created by test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID          string `json:"id"`
		Key         string `json:"key"`
		Name        string `json:"name"`
		Role        string `json:"role"`
		Builtin     bool   `json:"builtin"`
		Enabled     bool   `json:"enabled"`
		Description string `json:"description"`
	}
	crudJSON(t, rec, &created)
	if created.ID == "" || created.Key != key || created.Name != "契约探针" {
		t.Fatalf("created=%+v", created)
	}
	if created.Role != "assistant" || created.Builtin || !created.Enabled {
		t.Fatalf("custom agent defaults wrong: %+v", created)
	}

	// A duplicate key is rejected up front with 409 (not a DB unique violation 500).
	if rec := crudDo(t, s, http.MethodPost, "/api/agents", `{"key":"`+key+`","name":"dup"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// GET returns the detail envelope: starter prompt seeded, visibility arrays
	// non-null, profile candidates present, wrapup defaults from code.
	rec = crudDo(t, s, http.MethodGet, "/api/agents/"+key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail agentDetailDTO
	crudJSON(t, rec, &detail)
	if detail.Agent.Key != key || detail.Agent.Description != "created by test" {
		t.Fatalf("detail agent=%+v", detail.Agent)
	}
	if detail.Prompt == "" {
		t.Fatal("custom agent was created without a starter prompt")
	}
	if len(detail.Versions) == 0 {
		t.Fatal("starter prompt produced no version row")
	}
	// Visibility is asymmetric on purpose-but-undocumented: skill names are
	// normalized to [] while the mcp id list is passed straight through from
	// AgentVisible, whose empty result is a nil slice → JSON null. Pinned here so
	// that normalizing it later is a deliberate, visible change.
	if detail.Visibility.Skill == nil {
		t.Fatal("skill visibility must be [] not null")
	}
	if len(detail.Visibility.MCP) != 0 {
		t.Fatalf("fresh agent already sees MCP servers: %+v", detail.Visibility.MCP)
	}
	if len(detail.LLMProfiles) == 0 {
		t.Fatal("llm_profiles candidate list is empty; the profile dropdown would render blank")
	}
	if detail.Agent.TriggerRunMode != "serial" || detail.Agent.TriggerMergeMode != "all" || detail.Agent.TriggerMaxPar != 5 {
		t.Fatalf("trigger defaults=%+v", detail.Agent)
	}

	if rec := crudDo(t, s, http.MethodGet, "/api/agents/"+agentProbeKey("missing"), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key: status=%d", rec.Code)
	}

	// PATCH updates name/description; the description is stored NULLIF'd, so an
	// empty string clears it.
	if rec := crudDo(t, s, http.MethodPatch, "/api/agents/"+key, `{"name":"改名后","description":""}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := m.pg.GetAgentByKey(key)
	if err != nil || got == nil {
		t.Fatalf("reload agent: %v", err)
	}
	if got.Name != "改名后" || got.Description != "" {
		t.Fatalf("after patch: name=%q description=%q", got.Name, got.Description)
	}
	if rec := crudDo(t, s, http.MethodPatch, "/api/agents/"+key, `{"name":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch blank name: status=%d", rec.Code)
	}

	// Built-in agents are read-only through this API.
	if rec := crudDo(t, s, http.MethodPatch, "/api/agents/planner", `{"name":"hijack"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "内置 agent 不可修改") {
		t.Fatalf("patch builtin: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/agents/planner", ""); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "内置 agent 不可删除") {
		t.Fatalf("delete builtin: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The list endpoint advertises the custom agent with binding counters.
	var list struct {
		Agents []struct {
			Key        string `json:"key"`
			McpCount   int    `json:"mcp_count"`
			SkillCount int    `json:"skill_count"`
			ToolCount  int    `json:"tool_count"`
		} `json:"agents"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/agents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	found := false
	for _, a := range list.Agents {
		if a.Key == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("created agent missing from list: %+v", list.Agents)
	}

	// Delete returns the key and physically removes the row (FK cascades handle
	// prompts/vars/visibility).
	rec = crudDo(t, s, http.MethodDelete, "/api/agents/"+key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var deleted struct {
		Deleted string `json:"deleted"`
	}
	crudJSON(t, rec, &deleted)
	if deleted.Deleted != key {
		t.Fatalf("deleted=%q", deleted.Deleted)
	}
	if again, err := m.pg.GetAgentByKey(key); err != nil || again != nil {
		t.Fatalf("agent survived delete: %+v %v", again, err)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/agents/"+key, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete: status=%d", rec.Code)
	}
}

func TestAgentConfigRoutes(t *testing.T) {
	m, s := newTestStack(t)
	key := agentProbeKey("cfg")
	if _, err := m.pg.CreateAgent(key, "config probe", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteAgent(key) })
	cfg := "/api/agents/" + key + "/config"

	// Every field is optional: an empty body is a no-op that still reports ok.
	if rec := crudDo(t, s, http.MethodPut, cfg, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("empty patch: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Negative budgets are echoed raw in the response but clamped to 0 when
	// stored — the DTO reports the request, the DB reports the effective value.
	rec := crudDo(t, s, http.MethodPut, cfg, `{"max_turns":-3,"run_seconds":-1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("negative budgets: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var echoed map[string]any
	crudJSON(t, rec, &echoed)
	if echoed["max_turns"] != float64(-3) || echoed["run_seconds"] != float64(-1) {
		t.Fatalf("response must echo the request: %+v", echoed)
	}
	stored, _ := m.pg.GetAgentByKey(key)
	if stored.MaxTurns != 0 || stored.RunSecs != 0 {
		t.Fatalf("negative budgets not clamped: max_turns=%d run_seconds=%d", stored.MaxTurns, stored.RunSecs)
	}

	// Tool toggles are independent pointers.
	if rec := crudDo(t, s, http.MethodPut, cfg, `{"web_search":true,"interactive_shell":true}`); rec.Code != http.StatusOK {
		t.Fatalf("toggles: status=%d", rec.Code)
	}
	stored, _ = m.pg.GetAgentByKey(key)
	if !stored.WebSearch || !stored.InteractiveShell {
		t.Fatalf("toggles not stored: %+v", stored)
	}

	// llm_profile_id is three-state: absent = untouched, null = unbind, number =
	// bind after validating the target profile exists.
	if rec := crudDo(t, s, http.MethodPut, cfg, `{"llm_profile_id":"abc"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "格式错误") {
		t.Fatalf("bad profile id: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, cfg, `{"llm_profile_id":987654321}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "LLM 配置不存在") {
		t.Fatalf("unknown profile: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, cfg, `{"llm_profile_id":null}`); rec.Code != http.StatusOK {
		t.Fatalf("unbind: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ = m.pg.GetAgentByKey(key)
	if stored.LLMProfileID != nil {
		t.Fatalf("unbind left a profile: %v", *stored.LLMProfileID)
	}

	// Trigger behavior is written as a group: a partial patch backfills the
	// missing two fields from the current row instead of resetting them.
	if rec := crudDo(t, s, http.MethodPut, cfg,
		`{"trigger_run_mode":"parallel","trigger_merge_mode":"by_task","trigger_max_parallel":7}`); rec.Code != http.StatusOK {
		t.Fatalf("trigger full: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, cfg, `{"trigger_run_mode":"serial"}`); rec.Code != http.StatusOK {
		t.Fatalf("trigger partial: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ = m.pg.GetAgentByKey(key)
	if stored.TriggerRunMode != "serial" || stored.TriggerMergeMode != "by_task" || stored.TriggerMaxParallel != 7 {
		t.Fatalf("partial trigger patch clobbered siblings: %+v", stored)
	}

	if rec := crudDo(t, s, http.MethodPut, cfg, `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/agents/"+agentProbeKey("nope")+"/config", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent config: status=%d", rec.Code)
	}
}

func TestAgentVisibilityRoutes(t *testing.T) {
	m, s := newTestStack(t)
	key := agentProbeKey("vis")
	if _, err := m.pg.CreateAgent(key, "visibility probe", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteAgent(key) })
	vis := "/api/agents/" + key + "/visibility"

	// A fresh agent has no bindings. skill is normalized to []; mcp is the raw
	// AgentVisible slice, which is nil when empty and therefore serializes as null
	// (same asymmetry as the agent detail envelope).
	var empty struct {
		MCP   []int64  `json:"mcp"`
		Skill []string `json:"skill"`
	}
	rec := crudDo(t, s, http.MethodGet, vis, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get empty: status=%d", rec.Code)
	}
	crudJSON(t, rec, &empty)
	if empty.Skill == nil {
		t.Fatalf("empty skill visibility must be [] not null: %s", rec.Body.String())
	}
	if len(empty.MCP) != 0 || len(empty.Skill) != 0 {
		t.Fatalf("new agent already has bindings: %+v", empty)
	}

	// PUT replaces the full set for both kinds in one call.
	if rec := crudDo(t, s, http.MethodPut, vis, `{"mcp":[11,12],"skill":["skill_probe_a","skill_probe_b"]}`); rec.Code != http.StatusOK {
		t.Fatalf("put: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var after struct {
		MCP   []int64  `json:"mcp"`
		Skill []string `json:"skill"`
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, vis, ""), &after)
	if len(after.MCP) != 2 || len(after.Skill) != 2 {
		t.Fatalf("after put: %+v", after)
	}
	mcpSet := map[int64]bool{}
	for _, id := range after.MCP {
		mcpSet[id] = true
	}
	if !mcpSet[11] || !mcpSet[12] {
		t.Fatalf("mcp binding lost: %+v", after.MCP)
	}

	// A second PUT is a replace, not a merge.
	if rec := crudDo(t, s, http.MethodPut, vis, `{"mcp":[12],"skill":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("replace: status=%d", rec.Code)
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, vis, ""), &after)
	if len(after.MCP) != 1 || after.MCP[0] != 12 || len(after.Skill) != 0 {
		t.Fatalf("replace did not clear stale rows: %+v", after)
	}

	if rec := crudDo(t, s, http.MethodPut, vis, `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/agents/"+agentProbeKey("nope")+"/visibility", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent visibility: status=%d", rec.Code)
	}
}
