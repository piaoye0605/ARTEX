package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
)

// This file pins the Run closures of the orchestration tools in
// orchestration.go — the bodies the built-in orchestrator agent actually
// executes (list_tasks / list_llm_profiles / spawn_task / pause_task /
// delegate guards / update_finding_report) plus the deriveTaskStatus matrix.
// The tool *definitions* are covered elsewhere; the seed migrations (reseed*/
// upgrade*/seed*Bindings) deliberately are NOT tested here because they mutate
// library-wide agents, prompts and bindings.
//
// Tools are invoked directly via CoreTool.Call(ctx, input, nil): the same entry
// the agent harness uses, minus the LLM.

func callOrchTool(t *testing.T, tool actool.CoreTool, in string) (string, bool) {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(in), nil)
	if err != nil {
		t.Fatalf("%s call error: %v", tool.Name(), err)
	}
	return res.Flatten(), res.IsError
}

// newSpawnProfile seeds a profile WITH an api key (loadProfileConfig only
// accepts keyed profiles) and deletes it on cleanup.
func newSpawnProfile(t *testing.T, m *Manager, name string) *db.LLMProfile {
	t.Helper()
	return newTestProfile(t, m, name)
}

func TestDeriveTaskStatusMatrix(t *testing.T) {
	m, s := newTestStack(t)

	// A freshly created, never-run task is "created" — not terminal, not
	// paused, and engine.Started is still false.
	task, err := m.CreateTask("derive status", "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if got := s.deriveTaskStatus(task); got != "created" {
		t.Fatalf("fresh task status=%q want created", got)
	}

	// Paused wins over everything below terminal.
	if _, err := s.applyTaskControl(task, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := s.deriveTaskStatus(task); got != "paused" {
		t.Fatalf("paused task status=%q want paused", got)
	}

	// Terminal DB status is reported verbatim, even while paused.
	if _, err := m.SetTaskStatusGuarded(task.ID, "done"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got := s.deriveTaskStatus(task); got != "done" {
		t.Fatalf("done task status=%q want done", got)
	}
}

func TestToolListTasksRows(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	prof := newSpawnProfile(t, m, fmt.Sprintf("spawn-list-%d", stamp))
	// Descriptions carry the run stamp: the tasks table is shared and older runs
	// leave same-named rows behind, so matching by description alone would make
	// the row lookup pick up a stale task (and its stale pin).
	pinnedDesc := fmt.Sprintf("pinned runner %d", stamp)
	plainDesc := fmt.Sprintf("plain runner %d", stamp)

	pinned, err := m.CreateTask(pinnedDesc, "goal", &prof.ID, 0, 0)
	if err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if _, err := m.CreateTask(plainDesc, "goal", nil, 0, 0); err != nil {
		t.Fatalf("create plain: %v", err)
	}

	out, isErr := callOrchTool(t, s.toolListTasks(), `{}`)
	if isErr {
		t.Fatalf("list_tasks errored: %s", out)
	}
	var rows []struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		Goal        string `json:"goal"`
		Status      string `json:"status"`
		RunSeconds  int64  `json:"run_seconds"`
		ParentRef   string `json:"parent_ref"`
		LLMProfile  string `json:"llm_profile"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	byDesc := map[string]string{}
	for _, r := range rows {
		byDesc[r.Description] = r.LLMProfile
		if r.ID == "" || r.Goal == "" || r.Status == "" {
			t.Fatalf("row missing fields: %+v", r)
		}
		// run_seconds must be present (created-now tasks report >= 0) but never negative.
		if r.RunSeconds < 0 {
			t.Fatalf("negative run_seconds: %+v", r)
		}
	}
	if got := byDesc[pinnedDesc]; got != prof.Name {
		t.Fatalf("pinned llm_profile=%q want %q", got, prof.Name)
	}
	if got := byDesc[plainDesc]; got != "(激活配置)" {
		t.Fatalf("unpinned llm_profile=%q want (激活配置)", got)
	}

	// A pinned-but-deleted profile degrades to "#id(已删除)" instead of empty.
	if err := m.pg.DeleteProfile(prof.ID); err != nil {
		t.Fatalf("delete profile: %v", err)
	}
	out, _ = callOrchTool(t, s.toolListTasks(), `{}`)
	rows = nil
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("redecode: %v", err)
	}
	for _, r := range rows {
		if r.Description == pinnedDesc && !strings.HasPrefix(r.LLMProfile, fmt.Sprintf("#%d(", *pinned.LLMProfileID)) {
			t.Fatalf("deleted profile label=%q", r.LLMProfile)
		}
	}
}

func TestToolListLLMProfilesOutput(t *testing.T) {
	m, s := newTestStack(t)
	prof := newSpawnProfile(t, m, fmt.Sprintf("spawn-profiles-%d", time.Now().UnixNano()))

	out, isErr := callOrchTool(t, s.toolListLLMProfiles(), `{}`)
	if isErr {
		t.Fatalf("list_llm_profiles errored: %s", out)
	}
	if strings.Contains(out, "api_key") || strings.Contains(out, "sk-test-") {
		t.Fatalf("profile list leaked key material: %s", out)
	}
	var env struct {
		Profiles []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Model    string `json:"model"`
			Format   string `json:"format"`
			IsActive bool   `json:"is_active"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, p := range env.Profiles {
		if p.ID == prof.ID {
			found = true
			if p.Name != prof.Name || p.Model != "gpt-test" || p.Format != "openai" {
				t.Fatalf("profile row mismatch: %+v", p)
			}
			if p.IsActive {
				t.Fatalf("non-activated probe reported active: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("probe profile %d missing from %q", prof.ID, out)
	}
}

func TestToolSpawnTaskValidation(t *testing.T) {
	m, s := newTestStack(t)
	spawn := s.toolSpawnTask()

	// goal is mandatory; description falls back to 未命名任务 only on the
	// success path (validation errors never reach CreateTask).
	out, isErr := callOrchTool(t, spawn, `{"description":"x"}`)
	if !isErr || !strings.Contains(out, "goal 为必填") {
		t.Fatalf("missing goal: err=%v out=%s", isErr, out)
	}

	// Source list over the cap fails before any id is validated.
	tooMany := make([]string, db.MaxTaskSourceCount+1)
	for i := range tooMany {
		tooMany[i] = "1"
	}
	payload, _ := json.Marshal(map[string]any{"description": "x", "goal": "g", "source_task_ids": tooMany})
	out, isErr = callOrchTool(t, spawn, string(payload))
	if !isErr || !strings.Contains(out, fmt.Sprintf("关联任务最多选择 %d 个", db.MaxTaskSourceCount)) {
		t.Fatalf("over cap: err=%v out=%s", isErr, out)
	}

	// Duplicate / invalid / unknown source ids.
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"duplicate", []string{"1", "1"}, "关联任务 id 无效或重复"},
		{"non-numeric", []string{"abc"}, "关联任务 id 无效或重复"},
		{"non-positive", []string{"0"}, "关联任务 id 无效或重复"},
	} {
		payload, _ := json.Marshal(map[string]any{"description": "x", "goal": "g", "source_task_ids": tc.ids})
		out, isErr = callOrchTool(t, spawn, string(payload))
		if !isErr || !strings.Contains(out, tc.want) {
			t.Fatalf("%s: err=%v out=%s", tc.name, isErr, out)
		}
	}
	payload, _ = json.Marshal(map[string]any{"description": "x", "goal": "g", "source_task_ids": []string{"999999999"}})
	out, isErr = callOrchTool(t, spawn, string(payload))
	if !isErr || !strings.Contains(out, "关联任务 #999999999 不存在") {
		t.Fatalf("unknown source: err=%v out=%s", isErr, out)
	}

	// A keyless profile id fails loadProfileConfig with the combined message.
	stamp := time.Now().UnixNano()
	keylessID, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("spawn-keyless-%d", stamp), Format: "openai", Model: "m", APIKey: "",
	})
	if err != nil {
		t.Fatalf("seed keyless: %v", err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(keylessID) })
	payload, _ = json.Marshal(map[string]any{"description": "x", "goal": "g", "llm_profile_id": keylessID})
	out, isErr = callOrchTool(t, spawn, string(payload))
	if !isErr || !strings.Contains(out, fmt.Sprintf("LLM 配置 #%d 不存在或未设置 API Key", keylessID)) {
		t.Fatalf("keyless profile: err=%v out=%s", isErr, out)
	}

	// A numeric string is accepted for llm_profile_id (parseProfileID contract).
	missing := int64(987654321)
	payload, _ = json.Marshal(map[string]any{"description": "x", "goal": "g", "llm_profile_id": fmt.Sprint(missing)})
	out, isErr = callOrchTool(t, spawn, string(payload))
	if !isErr || !strings.Contains(out, fmt.Sprintf("LLM 配置 #%d 不存在", missing)) {
		t.Fatalf("string id: err=%v out=%s", isErr, out)
	}
}

func TestToolSpawnTaskCreatesAndLaunches(t *testing.T) {
	m, s := newTestStack(t)
	prof := newSpawnProfile(t, m, fmt.Sprintf("spawn-run-%d", time.Now().UnixNano()))

	parent, err := m.CreateTask("orchestrator parent", "goal", &prof.ID, 0, 0)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// parent_ref inherits the parent's pinned profile when no explicit id given.
	out, isErr := callOrchTool(t, s.toolSpawnTask(), fmt.Sprintf(
		`{"description":"spawned child","goal":"scan target","parent_ref":%q}`, parent.ID))
	if isErr {
		t.Fatalf("spawn: %s", out)
	}
	var created string
	if _, err := fmt.Sscanf(out, "task created: %s", &created); err != nil {
		t.Fatalf("spawn output %q: %v", out, err)
	}
	child, ok := m.Task(created)
	if !ok {
		t.Fatalf("spawned task %s not registered", created)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(created, DeleteTaskOptions{}) })
	if child.ParentRef != parent.ID {
		t.Fatalf("parent_ref=%q want %q", child.ParentRef, parent.ID)
	}
	if child.LLMProfileID == nil || *child.LLMProfileID != prof.ID {
		t.Fatalf("inherited pin=%v want %d", child.LLMProfileID, prof.ID)
	}
	if child.Description != "spawned child" || child.Goal != "scan target" {
		t.Fatalf("spawned fields: %q / %q", child.Description, child.Goal)
	}

	// Explicit llm_profile_id beats the inherited pin; persisted row agrees.
	prof2 := newSpawnProfile(t, m, fmt.Sprintf("spawn-run2-%d", time.Now().UnixNano()))
	out, isErr = callOrchTool(t, s.toolSpawnTask(), fmt.Sprintf(
		`{"description":"explicit pin","goal":"g","parent_ref":%q,"llm_profile_id":%d}`, parent.ID, prof2.ID))
	if isErr {
		t.Fatalf("spawn 2: %s", out)
	}
	if _, err := fmt.Sscanf(out, "task created: %s", &created); err != nil {
		t.Fatalf("spawn 2 output %q: %v", out, err)
	}
	child2, ok := m.Task(created)
	if !ok {
		t.Fatalf("spawned task 2 %s not registered", created)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(created, DeleteTaskOptions{}) })
	if child2.LLMProfileID == nil || *child2.LLMProfileID != prof2.ID {
		t.Fatalf("explicit pin=%v want %d", child2.LLMProfileID, prof2.ID)
	}

	// Empty description degrades to the documented fallback title.
	out, isErr = callOrchTool(t, s.toolSpawnTask(), `{"goal":"g"}`)
	if isErr {
		t.Fatalf("spawn 3: %s", out)
	}
	if _, err := fmt.Sscanf(out, "task created: %s", &created); err != nil {
		t.Fatalf("spawn 3 output %q: %v", out, err)
	}
	child3, ok := m.Task(created)
	if !ok {
		t.Fatalf("spawned task 3 %s not registered", created)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(created, DeleteTaskOptions{}) })
	if child3.Description != "未命名任务" {
		t.Fatalf("fallback description=%q", child3.Description)
	}
}

func TestToolPauseTaskRoute(t *testing.T) {
	m, s := newTestStack(t)

	out, isErr := callOrchTool(t, s.toolPauseTask(), `{"task_id":"999999999"}`)
	if !isErr || !strings.Contains(out, "task 不存在: 999999999") {
		t.Fatalf("unknown: err=%v out=%s", isErr, out)
	}

	task, err := m.CreateTask("pause me", "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, isErr = callOrchTool(t, s.toolPauseTask(), fmt.Sprintf(`{"task_id":%q}`, task.ID))
	if isErr || out != "task paused: "+task.ID {
		t.Fatalf("pause: err=%v out=%s", isErr, out)
	}
	if !task.lifecycleSnapshot().Paused {
		t.Fatal("task not actually paused")
	}
	// Re-pause surfaces the control layer's conflict as a tool error.
	out, isErr = callOrchTool(t, s.toolPauseTask(), fmt.Sprintf(`{"task_id":%q}`, task.ID))
	if !isErr {
		t.Fatalf("double pause accepted: %s", out)
	}
}

func TestDelegateToTaskGuards(t *testing.T) {
	_, s := newTestStack(t)

	out, isErr := callOrchTool(t, s.toolGetTaskGraph(), `{}`)
	if !isErr || !strings.Contains(out, "task_id 为必填") {
		t.Fatalf("empty task_id: err=%v out=%s", isErr, out)
	}
	out, isErr = callOrchTool(t, s.toolGetTaskGraph(), `{"task_id":"424242"}`)
	if !isErr || !strings.Contains(out, "task 不存在: 424242") {
		t.Fatalf("unknown task_id: err=%v out=%s", isErr, out)
	}
}

func TestToolUpdateFindingReportFlow(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "orchreport", true)
	update := s.toolUpdateFindingReport()

	// Invalid ids: non-numeric and non-positive.
	for _, in := range []string{`{"finding_id":"abc","report":"r"}`, `{"finding_id":0,"report":"r"}`, `{"report":"r"}`} {
		out, isErr := callOrchTool(t, update, in)
		if !isErr || !strings.Contains(out, "finding_id 无效") {
			t.Fatalf("invalid id %s: err=%v out=%s", in, isErr, out)
		}
	}

	// Unknown but well-formed id.
	out, isErr := callOrchTool(t, update, `{"finding_id":987654321,"report":"r"}`)
	if !isErr || !strings.Contains(out, "未找到 finding_id=987654321") {
		t.Fatalf("unknown finding: err=%v out=%s", isErr, out)
	}

	// The tool takes the finding NODE id (what report_finding returns), not the
	// findings-row id: SetFindingReportVersionByNodeID resolves by node_id.
	report := "# 详细报告\n\n复现步骤..."
	out, isErr = callOrchTool(t, update, fmt.Sprintf(`{"finding_id":%q,"report":%q}`, fmt.Sprint(fx.node), report))
	if isErr || !strings.Contains(out, fmt.Sprintf("finding %d report updated (%d chars)", fx.node, len(report))) {
		t.Fatalf("string id update: err=%v out=%s", isErr, out)
	}
	row, err := m.pg.GetFinding(fx.finding)
	if err != nil || row == nil {
		t.Fatalf("reload finding: %v", err)
	}
	if row.Report != report {
		t.Fatalf("report not persisted: %q", row.Report)
	}

	// evidence_version conflict is rejected and leaves the old report intact.
	bad := int64(999)
	out, isErr = callOrchTool(t, update, fmt.Sprintf(`{"finding_id":%d,"report":"clobber","evidence_version":%d}`, fx.node, bad))
	if !isErr {
		t.Fatalf("stale version accepted: %s", out)
	}
	row, _ = m.pg.GetFinding(fx.finding)
	if row.Report != report {
		t.Fatal("conflicted write clobbered the report")
	}
}
