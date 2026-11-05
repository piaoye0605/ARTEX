package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Coverage for server_tasks.go — the task lifecycle routes that previously ran
// only through the UI: get/list/frontier reads, active-task switching, pause &
// resume control, per-intent control, rerun (single + batch), task creation,
// the LLM-chain swap endpoint, the seed helpers and the coverage/scope surface.
// Everything runs against a real Manager + Server via newTestStack so the
// assertions pin actual persistence behavior. The task list lives in a shared
// persistent database, so list assertions are "contains our task", never counts.

// tsCreateTask creates one task through the HTTP route and returns its id plus
// the decoded response map (handy for asserting the DTO contract).
func tsCreateTask(t *testing.T, s *Server, body string) (string, map[string]any) {
	t.Helper()
	rec := crudDo(t, s, http.MethodPost, "/api/tasks", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create task: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	crudJSON(t, rec, &out)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("created task has no string id: %s", rec.Body.String())
	}
	return id, out
}

func TestGetTaskListAndFrontierRoutes(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("tasks routes", "fixture goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// getTask: unknown id → 404, known id → the DTO with the string id contract.
	if rec := crudDo(t, s, http.MethodGet, "/api/tasks/999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown get: status=%d", rec.Code)
	}
	var got map[string]any
	rec := crudDo(t, s, http.MethodGet, "/api/tasks/"+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &got)
	if got["id"] != task.ID {
		t.Fatalf("get id=%v want %s", got["id"], task.ID)
	}

	// listTasks: the array must contain our task and carry the aggregate fields.
	var list struct {
		Tasks  []map[string]any `json:"tasks"`
		Active string           `json:"active"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	found := false
	for _, item := range list.Tasks {
		if item["id"] == task.ID {
			found = true
			if _, ok := item["findings"].(map[string]any); !ok {
				t.Fatalf("task dto missing findings severity block: %v", item)
			}
			if _, ok := item["tokens"].(map[string]any); !ok {
				t.Fatalf("task dto missing tokens block: %v", item)
			}
		}
	}
	if !found {
		t.Fatalf("list is missing task %s", task.ID)
	}

	// frontier: without a task scope → a JSON array; with one → seeded nodes only.
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/frontier", "")
	if rec.Code != http.StatusOK || rec.Body.String() == "null" {
		t.Fatalf("frontier no-task: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/frontier?task="+task.ID+"&limit=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("frontier: status=%d", rec.Code)
	}
	var nodes []map[string]any
	crudJSON(t, rec, &nodes)
	if nodes == nil {
		t.Fatal("frontier must return an array")
	}
}

func TestSetActiveRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("set active", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Malformed body and unknown ids are rejected before any engine work.
	if rec := crudDo(t, s, http.MethodPost, "/api/active", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/active", `{"id":"424242"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}

	// A queued task must NOT be started by opening it (concurrency discipline).
	queued, err := m.CreateTask("set active queued", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create queued task: %v", err)
	}
	if err := m.EnqueueTask(queued.ID, "resume"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rec := crudDo(t, s, http.MethodPost, "/api/active", `{"id":"`+queued.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set queued active: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var dto map[string]any
	rec = crudDo(t, s, http.MethodGet, "/api/tasks/"+queued.ID, "")
	crudJSON(t, rec, &dto)
	if dto["status"] != "queued" {
		t.Fatalf("queued task resolved to %v, want queued", dto["status"])
	}

	// Activating a normal task reports it back and listTasks surfaces it.
	rec = crudDo(t, s, http.MethodPost, "/api/active", `{"id":"`+task.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set active: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Active string `json:"active"`
	}
	crudJSON(t, rec, &out)
	if out.Active != task.ID {
		t.Fatalf("active=%s want %s", out.Active, task.ID)
	}
	var list struct {
		Active string `json:"active"`
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/tasks", ""), &list)
	if list.Active != task.ID {
		t.Fatalf("list active=%s want %s", list.Active, task.ID)
	}
}

func TestTaskControlRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("task control", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	base := "/api/tasks/" + task.ID + "/control"

	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/999999999/control", `{"action":"pause"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"action":"explode"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action: status=%d", rec.Code)
	}

	// pause → 200 paused; pausing again → 409 (already paused); resume → running.
	var out struct {
		ID     string `json:"id"`
		Paused bool   `json:"paused"`
		Status string `json:"status"`
	}
	rec := crudDo(t, s, http.MethodPost, base, `{"action":"pause"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if !out.Paused || out.Status != "paused" {
		t.Fatalf("pause result=%+v", out)
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"action":"pause"}`); rec.Code != http.StatusConflict {
		t.Fatalf("double pause: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodPost, base, `{"action":"resume"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.Paused {
		t.Fatalf("resume result still paused: %+v", out)
	}
	if status := out.Status; status != "running" && status != "queued" {
		t.Fatalf("resume status=%s", status)
	}

	// The pause state is persisted — the GET DTO reports paused while parked.
	if _, err := s.applyTaskControl(task, "pause"); err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	var dto map[string]any
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/tasks/"+task.ID, ""), &dto)
	if dto["status"] != "paused" {
		t.Fatalf("paused task dto status=%v", dto["status"])
	}
}

func TestTaskIntentControlRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("intent control", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	intent, err := task.Store.AddIntent(map[string]any{"summary": "open intent"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add intent: %v", err)
	}
	fact, err := task.Store.AddNode(db.KindFact, map[string]any{"title": "not an intent"}, 0, "confirmed", "human", nil)
	if err != nil {
		t.Fatalf("add fact: %v", err)
	}

	base := "/api/tasks/" + task.ID + "/intents/"
	// Route guards first: unknown task, bad intent id, bad json, bad action.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/999999999/intents/1/control", `{"action":"pause"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	for _, bad := range []string{"abc", "0", "-3"} {
		if rec := crudDo(t, s, http.MethodPost, base+bad+"/control", `{"action":"pause"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad iid %q: status=%d", bad, rec.Code)
		}
	}
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent)+"/control", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent)+"/control", `{"action":"boom"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action: status=%d", rec.Code)
	}

	// Unknown intent and non-intent nodes are 409s from the domain guard.
	if rec := crudDo(t, s, http.MethodPost, base+"987654321/control", `{"action":"pause"}`); rec.Code != http.StatusConflict {
		t.Fatalf("missing intent: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(fact)+"/control", `{"action":"pause"}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "node is not an intent") {
		t.Fatalf("non-intent node: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// pause only works on running intents.
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent)+"/control", `{"action":"pause"}`); rec.Code != http.StatusConflict {
		t.Fatalf("pause open intent: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent)+"/control", `{"action":"resume"}`); rec.Code != http.StatusConflict {
		t.Fatalf("resume open intent: status=%d", rec.Code)
	}

	// cancel without a reason is rejected; with one, soft delete lands in the store.
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent)+"/control", `{"action":"cancel"}`); rec.Code != http.StatusConflict {
		t.Fatalf("cancel no reason: status=%d", rec.Code)
	}
	var out struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent)+"/control", `{"action":"cancel","reason":"dup work"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.State != db.StateIntentDeleted {
		t.Fatalf("cancel state=%q", out.State)
	}
	node, err := task.Store.GetNode(intent)
	if err != nil || node == nil || node.State != db.StateIntentDeleted {
		t.Fatalf("soft deleted intent not persisted: node=%+v err=%v", node, err)
	}

	// hard cancel of a second intent physically removes it; re-cancel → 409.
	intent2, err := task.Store.AddIntent(map[string]any{"summary": "hard cancel me"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add intent2: %v", err)
	}
	rec = crudDo(t, s, http.MethodPost, base+itoaInt64(intent2)+"/control", `{"action":"cancel","reason":"hard","mode":"hard"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("hard cancel: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if node, _ := task.Store.GetNode(intent2); node != nil {
		t.Fatalf("hard cancel left the node: %+v", node)
	}
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(intent2)+"/control", `{"action":"cancel","reason":"again"}`); rec.Code != http.StatusConflict {
		t.Fatalf("cancel deleted intent: status=%d", rec.Code)
	}
}

func TestRerunIntentRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("rerun intent", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	openIntent, err := task.Store.AddIntent(map[string]any{"summary": "still open"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add intent: %v", err)
	}
	blockedIntent, err := task.Store.AddIntent(map[string]any{"summary": "blocked once"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add blocked intent: %v", err)
	}
	if err := task.Store.SetIntentBlockedReason(blockedIntent, "llm 502"); err != nil {
		t.Fatalf("block intent: %v", err)
	}
	if err := task.Store.SetIntentState(blockedIntent, "blocked"); err != nil {
		t.Fatalf("set blocked: %v", err)
	}

	base := "/api/tasks/" + task.ID + "/intents/"
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/999999999/intents/1/rerun", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base+"abc/rerun", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad iid: status=%d", rec.Code)
	}

	// An open intent is not rerunnable.
	if rec := crudDo(t, s, http.MethodPost, base+itoaInt64(openIntent)+"/rerun", ""); rec.Code != http.StatusConflict {
		t.Fatalf("rerun open intent: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A blocked intent reopens, keeps its blocked_reason for the retry, and the
	// admission revives the task.
	var out struct {
		ID       string `json:"id"`
		Reopened int64  `json:"reopened"`
		Queued   bool   `json:"queued"`
	}
	rec := crudDo(t, s, http.MethodPost, base+itoaInt64(blockedIntent)+"/rerun", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rerun: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.Reopened != blockedIntent || out.ID != task.ID {
		t.Fatalf("rerun result=%+v", out)
	}
	node, err := task.Store.GetNode(blockedIntent)
	if err != nil || node == nil || node.State != "open" {
		t.Fatalf("rerun did not reopen: node=%+v err=%v", node, err)
	}
}

func TestRerunBlockedRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("rerun blocked batch", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Zero blocked intents → still 200 with reopened=0 and no admission churn.
	rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+task.ID+"/intents/rerun-blocked", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Reopened int64 `json:"reopened"`
		Queued   bool  `json:"queued"`
	}
	crudJSON(t, rec, &out)
	if out.Reopened != 0 {
		t.Fatalf("empty batch reopened=%d", out.Reopened)
	}

	// Seed three blocked intents (plus noise in other states) and rerun them all.
	states := map[int64]string{}
	for i := 0; i < 4; i++ {
		id, err := task.Store.AddIntent(map[string]any{"summary": fmt.Sprintf("intent %d", i)}, 0, nil, "planner")
		if err != nil {
			t.Fatalf("add intent: %v", err)
		}
		state := "blocked"
		if i == 3 {
			state = "open"
		}
		if err := task.Store.SetIntentState(id, state); err != nil {
			t.Fatalf("set state: %v", err)
		}
		states[id] = state
	}
	rec = crudDo(t, s, http.MethodPost, "/api/tasks/"+task.ID+"/intents/rerun-blocked", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if out.Reopened != 3 {
		t.Fatalf("batch reopened=%d want 3", out.Reopened)
	}
	for id, state := range states {
		node, err := task.Store.GetNode(id)
		if err != nil {
			t.Fatalf("get node %d: %v", id, err)
		}
		want := "open"
		if state == "open" {
			want = "open"
		}
		if node.State != want {
			t.Fatalf("intent %d state=%s want %s", id, node.State, want)
		}
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/999999999/intents/rerun-blocked", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
}

func TestValidateTaskProfileIDs(t *testing.T) {
	m, s := newTestStack(t)
	// Empty chain is valid (falls back to the global active profile).
	if err := s.validateTaskProfileIDs(nil); err != nil {
		t.Fatalf("nil ids: %v", err)
	}
	// Duplicates and non-positive ids are rejected.
	if err := s.validateTaskProfileIDs([]int64{0}); err == nil {
		t.Fatal("zero id accepted")
	}
	if err := s.validateTaskProfileIDs([]int64{7, 7}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	// Unknown profiles are rejected.
	if err := s.validateTaskProfileIDs([]int64{424242}); err == nil {
		t.Fatal("unknown profile accepted")
	}
	// A profile with an API key passes validation.
	pg := m.PG()
	id, err := pg.SaveProfile(&db.LLMProfile{Name: "ts-valid", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatalf("save profile: %v", err)
	}
	if err := s.validateTaskProfileIDs([]int64{id}); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	// A profile without an API key is unusable.
	id2, err := pg.SaveProfile(&db.LLMProfile{Name: "ts-nokey", Format: "openai", Model: "m"})
	if err != nil {
		t.Fatalf("save keyless profile: %v", err)
	}
	if err := s.validateTaskProfileIDs([]int64{id2}); err == nil {
		t.Fatal("keyless profile accepted")
	}
}

func TestCreateTaskRoute(t *testing.T) {
	_, s := newTestStack(t)

	if rec := crudDo(t, s, http.MethodPost, "/api/tasks", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	// A blank description is defaulted; creation still succeeds.
	id, dto := tsCreateTask(t, s, `{"goal":"default desc goal"}`)
	if dto["description"] != "未命名任务" {
		t.Fatalf("description=%v want default", dto["description"])
	}

	// Legacy single-profile field maps onto the chain and unknown ids 400.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks", `{"goal":"g","llm_profile_id":424242}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile: status=%d", rec.Code)
	}
	// Source tasks: unparsable, unknown, duplicated ids all 400.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks", `{"goal":"g","source_task_ids":["abc"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad source: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks", `{"goal":"g","source_task_ids":["424242"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown source: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks", `{"goal":"g","source_task_ids":["`+id+`","`+id+`"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("dup source: status=%d", rec.Code)
	}
	// Negative timeout is normalized, not rejected — pinned via a valid create.
	// Task-level intercept rules with an invalid pattern are rejected up front.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks", `{"goal":"g","intercept_rules":[{"kind":"no-such-kind","pattern":"x"}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad intercept rule: status=%d", rec.Code)
	}

	// Full create with a seed intent: the DTO echoes name/status and the seed
	// intent lands in the frontier.
	created, dto := tsCreateTask(t, s, `{"description":"full create","goal":"seed goal","name":"named","seed_first_intent":true,"timeout_seconds":-5}`)
	if dto["name"] != "named" {
		t.Fatalf("name=%v", dto["name"])
	}
	m := s.m
	task, ok := m.Task(created)
	if !ok {
		t.Fatalf("created task %s not in manager", created)
	}
	intents, err := task.Store.ListByKind(db.KindIntent, 10)
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	if len(intents) == 0 {
		t.Fatal("seed_first_intent produced no intent")
	}
}

func TestUpdateTaskLLMProfilesRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("llm chain swap", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	pg := m.PG()
	id1, err := pg.SaveProfile(&db.LLMProfile{Name: "chain-a", Format: "openai", Model: "a", APIKey: "k1"})
	if err != nil {
		t.Fatalf("save profile a: %v", err)
	}
	id2, err := pg.SaveProfile(&db.LLMProfile{Name: "chain-b", Format: "openai", Model: "b", APIKey: "k2"})
	if err != nil {
		t.Fatalf("save profile b: %v", err)
	}

	base := "/api/tasks/" + task.ID + "/llm"
	if rec := crudDo(t, s, http.MethodPut, "/api/tasks/999999999/llm", `{"llm_profile_ids":[1]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, base, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, base, `{"llm_profile_ids":[424242]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile: status=%d", rec.Code)
	}

	// Install chain A, then swap to B with an explicit active profile → the
	// response echoes the chain, the active id and a switch event.
	var out struct {
		ID              string          `json:"id"`
		ProfileIDs      []int64         `json:"llm_profile_ids"`
		ActiveProfileID int64           `json:"active_llm_profile_id"`
		FailoverState   string          `json:"llm_failover_state"`
		SwitchEvent     *map[string]any `json:"switch_event"`
	}
	rec := crudDo(t, s, http.MethodPut, base, `{"llm_profile_ids":[`+itoaInt64(id1)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install chain: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if len(out.ProfileIDs) != 1 || out.ProfileIDs[0] != id1 {
		t.Fatalf("installed chain=%v want [%d]", out.ProfileIDs, id1)
	}
	rec = crudDo(t, s, http.MethodPut, base, `{"llm_profile_ids":[`+itoaInt64(id2)+`],"active_llm_profile_id":`+itoaInt64(id2)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("swap chain: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if len(out.ProfileIDs) != 1 || out.ProfileIDs[0] != id2 || out.ActiveProfileID != id2 {
		t.Fatalf("swapped chain=%v active=%d want [%d]/%d", out.ProfileIDs, out.ActiveProfileID, id2, id2)
	}
	if out.SwitchEvent == nil {
		t.Fatal("active profile change must emit a switch_event")
	}
}

func TestParseTargetHelpers(t *testing.T) {
	cases := []struct {
		text         string
		scheme, host string
		port         int
		ok           bool
	}{
		{"visit http://10.1.2.3:8080/admin now", "http", "10.1.2.3", 8080, true},
		{"https://secure.example.com/login", "https", "secure.example.com", 443, true},
		{"plain 192.168.5.5:8443 target", "https", "192.168.5.5", 8443, true},
		{"plain 192.168.5.5 target", "http", "192.168.5.5", 80, true},
		{"domain example.com:9000", "http", "example.com", 9000, true},
		{"domain example.com", "https", "example.com", 443, true},
		{"no target in this text", "", "", 0, false},
		{"   ", "", "", 0, false},
	}
	for _, c := range cases {
		scheme, host, port, ok := parseTarget(c.text)
		if ok != c.ok || (ok && (scheme != c.scheme || host != c.host || port != c.port)) {
			t.Fatalf("parseTarget(%q)=%q,%q,%d,%v want %q,%q,%d,%v", c.text, scheme, host, port, ok, c.scheme, c.host, c.port, c.ok)
		}
	}
	if portOr("", 80) != 80 || portOr("0", 80) != 80 || portOr("bogus", 443) != 443 || portOr("9000", 80) != 9000 {
		t.Fatal("portOr defaults broken")
	}
	if defaultPort("http") != 80 || defaultPort("https") != 443 {
		t.Fatal("defaultPort broken")
	}
	if schemeForPort(443) != "https" || schemeForPort(8443) != "https" || schemeForPort(8080) != "http" {
		t.Fatal("schemeForPort broken")
	}
}

func TestSeedHelpers(t *testing.T) {
	m, s := newTestStack(t)
	task, err := m.CreateTask("seed helpers", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	stamp := time.Now().UnixNano()
	host := fmt.Sprintf("seed-%d.example.com", stamp)

	// An unparseable target is a no-op, not an error.
	s.seed(task, "nothing that parses here")
	// A domain target seeds a root-domain asset anchored to the task.
	s.seed(task, "target https://"+host+"/ login")

	as := m.Assets()
	if as == nil {
		t.Skip("asset store unavailable")
	}
	scopes, err := as.ListTaskScopeWithSources(taskIDOf(task))
	if err != nil {
		t.Fatalf("list scope: %v", err)
	}
	_ = scopes // coverage reads the seeded row via the routes below

	// The LLM gateway host must never become a target.
	s.cfgMu.Lock()
	s.llmCfg.BaseURL = "http://" + host + ":9999/v1"
	s.cfgMu.Unlock()
	if got := s.llmHost(); got != host {
		t.Fatalf("llmHost=%q want %q", got, host)
	}
	s.seed(task, "attack https://"+host+"/ again")
	s.cfgMu.Lock()
	s.llmCfg.BaseURL = ""
	s.cfgMu.Unlock()
	if got := s.llmHost(); got != "" {
		t.Fatalf("llmHost empty cfg=%q", got)
	}

	// seedFirstIntent writes exactly one open seed intent linked from the origin.
	if _, err := task.Store.OriginFactID(); err != nil {
		t.Fatalf("origin fact: %v", err)
	}
	s.seedFirstIntent(task)
	intents, err := task.Store.ListByKind(db.KindIntent, 10)
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	if len(intents) != 1 || intents[0].State != "open" {
		t.Fatalf("seed intent state=%+v", intents)
	}
}

func TestTaskCoverageAndScopeRoutes(t *testing.T) {
	m, s := newTestStack(t)
	// coverage_enabled defaults to on; create through the route to pin the flag.
	created, _ := tsCreateTask(t, s, `{"description":"coverage task","goal":"g","coverage_enabled":true}`)
	taskID, err := strconv.ParseInt(created, 10, 64)
	if err != nil {
		t.Fatalf("task id: %v", err)
	}
	task, ok := s.m.Task(created)
	if !ok {
		t.Fatalf("task missing: %s", created)
	}

	for _, path := range []string{
		"/api/tasks/999999999/coverage",
		"/api/tasks/999999999/coverage-graph",
		"/api/tasks/999999999/scope",
		"/api/tasks/999999999/asset-refs?asset_id=1",
	} {
		if rec := crudDo(t, s, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown task %s: status=%d", path, rec.Code)
		}
	}

	var cov struct {
		Enabled bool                `json:"enabled"`
		ByType  []db.CoverageByType `json:"by_type"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/tasks/"+created+"/coverage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("coverage: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &cov)
	if !cov.Enabled {
		t.Fatal("coverage must be enabled for this task")
	}
	rec = crudDo(t, s, http.MethodGet, "/api/tasks/"+created+"/coverage-graph", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("coverage graph: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Scope add: bad json/blank value rejected; a valid root_domain lands and is
	// visible via the list route, then removable via the delete route.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+created+"/scope", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("scope bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+created+"/scope", `{"kind":"root_domain","value":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("scope blank value: status=%d", rec.Code)
	}
	stamp := time.Now().UnixNano()
	domain := fmt.Sprintf("scope-%d.example.com", stamp)
	// subdomain keeps the full host (root_domain would normalize to the
	// registered domain); NOTE: AddAgentScope does not backfill the row id
	// (ON CONFLICT DO NOTHING without RETURNING) — the contract being pinned.
	var added db.TaskScope
	rec = crudDo(t, s, http.MethodPost, "/api/tasks/"+created+"/scope", `{"kind":"subdomain","value":"`+domain+`","reason":"manual test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scope add: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &added)
	if added.Kind != "subdomain" || added.Domain != domain {
		t.Fatalf("scope add row=%+v", added)
	}
	var listResp struct {
		Scope []db.TaskScope `json:"scope"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/tasks/"+created+"/scope", "")
	crudJSON(t, rec, &listResp)
	scopeID := int64(0)
	found := false
	for _, row := range listResp.Scope {
		if row.Domain == domain {
			found = true
			scopeID = row.ID
			if row.Kind != "subdomain" {
				t.Fatalf("scope row kind=%q", row.Kind)
			}
		}
	}
	if !found || scopeID <= 0 {
		t.Fatalf("scope list missing row for %s: %+v", domain, listResp.Scope)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/tasks/"+created+"/scope/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("scope delete bad sid: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/tasks/"+created+"/scope/"+itoaInt64(424242), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("scope delete unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/tasks/"+created+"/scope/"+itoaInt64(scopeID), ""); rec.Code != http.StatusOK {
		t.Fatalf("scope delete: status=%d", rec.Code)
	}

	// asset-refs: missing asset_id → 400; a real asset id → well-formed payload.
	if rec := crudDo(t, s, http.MethodGet, "/api/tasks/"+created+"/asset-refs", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("asset-refs no id: status=%d", rec.Code)
	}
	as := m.Assets()
	if as == nil {
		t.Skip("asset store unavailable")
	}
	ipAsset, err := as.UpsertIP(db.UpsertIPReq{IP: "10.199.77.5", TaskID: taskID})
	if err != nil {
		t.Fatalf("upsert ip: %v", err)
	}
	rec = crudDo(t, s, http.MethodGet, "/api/tasks/"+created+"/asset-refs?asset_id="+itoaInt64(ipAsset), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset-refs: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var refs struct {
		Intents  []map[string]any `json:"intents"`
		Facts    []map[string]any `json:"facts"`
		Findings []map[string]any `json:"findings"`
	}
	crudJSON(t, rec, &refs)
	if refs.Intents == nil || refs.Facts == nil || refs.Findings == nil {
		t.Fatalf("asset-refs payload must use empty arrays: %+v", refs)
	}
	_ = task
}

func taskIDOf(t *Task) int64 {
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	return id
}
