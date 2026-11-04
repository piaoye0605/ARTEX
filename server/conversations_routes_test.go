package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// This file pins the chat-page conversation contracts: the CRUD routes, the
// batch-delete envelope, the messages poll/page/detail trio, the stop endpoint's
// idle contract, and the send-message guards that all fire BEFORE any LLM call
// (so they are deterministic with no profile configured).

func convTestAgent(t *testing.T, m *Manager) string {
	t.Helper()
	key := fmt.Sprintf("conv_t%d", time.Now().UnixNano())
	if _, err := m.pg.CreateAgent(key, "conv test agent", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return key
}

func cleanupConversations(t *testing.T, m *Manager, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if id > 0 {
			_ = m.pg.DeleteConversation(id)
		}
	}
}

func mustCreateConversation(t *testing.T, s *Server, agentKey, title string) *db.Conversation {
	t.Helper()
	var c db.Conversation
	rec := crudDo(t, s, http.MethodPost, "/api/conversations",
		`{"agent_key":"`+agentKey+`","title":"`+title+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create conversation: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &c)
	return &c
}

func TestConversationCRUDRoutes(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)

	// Create: empty title defaults to 新对话; bad payloads are rejected up front.
	var c db.Conversation
	rec := crudDo(t, s, http.MethodPost, "/api/conversations", `{"agent_key":"`+agentKey+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create default title: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &c)
	if c.Title != "新对话" || c.AgentKey != agentKey || c.ID <= 0 {
		t.Fatalf("created=%+v", c)
	}
	defer cleanupConversations(t, m, c.ID)

	if rec := crudDo(t, s, http.MethodPost, "/api/conversations", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/conversations", `{"agent_key":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank agent_key: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/conversations",
		`{"agent_key":"no_such_agent_xyz"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status=%d", rec.Code)
	}
	badProfile := int64(987654321)
	if rec := crudDo(t, s, http.MethodPost, "/api/conversations",
		fmt.Sprintf(`{"agent_key":"%s","llm_profile_id":%d}`, agentKey, badProfile)); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// List wraps rows with the live running flag (false — nothing in flight).
	var list struct {
		Conversations []struct {
			ID      int64 `json:"id"`
			Running bool  `json:"running"`
		} `json:"conversations"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/conversations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	crudJSON(t, rec, &list)
	found := false
	for _, it := range list.Conversations {
		if it.ID == c.ID {
			found = true
			if it.Running {
				t.Fatal("idle conversation reported running")
			}
		}
	}
	if !found {
		t.Fatalf("list missing conversation %d", c.ID)
	}

	// Rename: title + pinned in one patch; validations per field.
	patch := `{"title":"renamed conv","pinned":true}`
	rec = crudDo(t, s, http.MethodPatch, fmt.Sprintf("/api/conversations/%d", c.ID), patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var renamed db.Conversation
	crudJSON(t, rec, &renamed)
	if renamed.Title != "renamed conv" || !renamed.Pinned || renamed.PinnedAt == nil {
		t.Fatalf("renamed=%+v", renamed)
	}
	if rec := crudDo(t, s, http.MethodPatch, fmt.Sprintf("/api/conversations/%d", c.ID), `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, fmt.Sprintf("/api/conversations/%d", c.ID), `{"title":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank title: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, "/api/conversations/999999999", `{"title":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("rename unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, "/api/conversations/abc", `{"title":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("rename bad id: status=%d", rec.Code)
	}

	// Profile override: setting an unknown profile is a 400; null clears it.
	path := fmt.Sprintf("/api/conversations/%d/profile", c.ID)
	if rec := crudDo(t, s, http.MethodPatch, path, fmt.Sprintf(`{"llm_profile_id":%d}`, badProfile)); rec.Code != http.StatusBadRequest {
		t.Fatalf("profile unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, path, `{"llm_profile_id":null}`); rec.Code != http.StatusOK {
		t.Fatalf("profile clear: status=%d", rec.Code)
	}
	got, err := m.pg.GetConversation(c.ID)
	if err != nil || got.LLMProfileID != nil {
		t.Fatalf("profile not cleared: %+v err=%v", got, err)
	}

	// Delete: {deleted:id}; a second delete is a 404.
	if rec := crudDo(t, s, http.MethodDelete, fmt.Sprintf("/api/conversations/%d", c.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, fmt.Sprintf("/api/conversations/%d", c.ID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete twice: status=%d", rec.Code)
	}
}

func TestConversationBatchDeleteRoutes(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	a := mustCreateConversation(t, s, agentKey, "batch a")
	b := mustCreateConversation(t, s, agentKey, "batch b")
	defer cleanupConversations(t, m, a.ID, b.ID)

	// Guards: empty, invalid ids, over the 100 cap (101 DISTINCT ids — dupes fold
	// before the cap check, so repeats would slip through).
	if rec := crudDo(t, s, http.MethodPost, "/api/conversations/delete/batch", `{"ids":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty ids: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/conversations/delete/batch", `{"ids":[0,-5]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ids: status=%d", rec.Code)
	}
	var many []string
	for i := 1; i <= 101; i++ {
		many = append(many, fmt.Sprintf("%d", i))
	}
	over := `{"ids":[` + strings.Join(many, ",") + `]}`
	if rec := crudDo(t, s, http.MethodPost, "/api/conversations/delete/batch", over); rec.Code != http.StatusBadRequest {
		t.Fatalf("over cap: status=%d", rec.Code)
	}

	// Real batch: duplicates fold, unknown ids report per-item failure.
	body := fmt.Sprintf(`{"ids":[%d,%d,%d,%d]}`, a.ID, b.ID, a.ID, 424242424)
	var resp struct {
		Items []struct {
			ID    int64  `json:"id"`
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
		} `json:"items"`
	}
	rec := crudDo(t, s, http.MethodPost, "/api/conversations/delete/batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &resp)
	if len(resp.Items) != 3 { // duplicate folded
		t.Fatalf("items=%+v", resp.Items)
	}
	for _, it := range resp.Items {
		switch it.ID {
		case a.ID, b.ID:
			if !it.OK || it.Error != "" {
				t.Fatalf("existing id not deleted: %+v", it)
			}
		case 424242424:
			if it.OK || it.Error != "conversation not found" {
				t.Fatalf("unknown id contract: %+v", it)
			}
		default:
			t.Fatalf("unexpected id %d", it.ID)
		}
	}
}

func TestConversationMessagesRoutes(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	c := mustCreateConversation(t, s, agentKey, "messages")
	defer cleanupConversations(t, m, c.ID)

	// Seed three turns directly (the send route needs an LLM; persistence shape
	// is what the poll/page/detail trio exposes).
	for i := 1; i <= 3; i++ {
		_, err := m.pg.AppendConvActivity(c.ID, db.Activity{
			Worker: agentKey, Kind: "user",
			Summary: fmt.Sprintf("turn %d", i), Detail: fmt.Sprintf("full text %d", i),
		})
		if err != nil {
			t.Fatalf("seed activity %d: %v", i, err)
		}
	}

	// History page: latest page first, cursor = newest id on the page, hasMore false.
	// Activity DTOs carry their id as `seq` (same shape as the task stream).
	var page struct {
		Items []struct {
			Seq     int64  `json:"seq"`
			Summary string `json:"summary"`
		} `json:"items"`
		Cursor  int64 `json:"cursor"`
		HasMore bool  `json:"hasMore"`
		Running bool  `json:"running"`
	}
	rec := crudDo(t, s, http.MethodGet, fmt.Sprintf("/api/conversations/%d/messages", c.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &page)
	if len(page.Items) != 3 || page.Cursor != page.Items[2].Seq || page.HasMore {
		t.Fatalf("page=%+v", page)
	}
	if page.Items[0].Summary != "turn 1" || page.Items[2].Summary != "turn 3" {
		t.Fatalf("page order wrong: %+v", page.Items)
	}

	// Incremental tail: since=<first id> returns only the later steps.
	var tail struct {
		Items []struct {
			Summary string `json:"summary"`
		} `json:"items"`
		Cursor int64 `json:"cursor"`
	}
	rec = crudDo(t, s, http.MethodGet, fmt.Sprintf("/api/conversations/%d/messages?since=%d", c.ID, page.Items[0].Seq), "")
	crudJSON(t, rec, &tail)
	if len(tail.Items) != 2 || tail.Items[0].Summary != "turn 2" {
		t.Fatalf("tail=%+v", tail)
	}

	// Lazy detail returns the full blob per seq.
	var detail struct {
		Detail string `json:"detail"`
	}
	rec = crudDo(t, s, http.MethodGet, fmt.Sprintf("/api/conversations/%d/messages/%d", c.ID, page.Items[2].Seq), "")
	crudJSON(t, rec, &detail)
	if detail.Detail != "full text 3" {
		t.Fatalf("detail=%q", detail.Detail)
	}
	if rec := crudDo(t, s, http.MethodGet, fmt.Sprintf("/api/conversations/%d/messages/abc", c.ID), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad seq: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/conversations/999999999/messages", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("messages unknown conv: status=%d", rec.Code)
	}
}

func TestConversationStopRoute(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	c := mustCreateConversation(t, s, agentKey, "stop me")
	defer cleanupConversations(t, m, c.ID)

	// With nothing in flight the stop endpoint reports idle, not an error.
	var resp struct {
		Status string `json:"status"`
	}
	rec := crudDo(t, s, http.MethodPost, fmt.Sprintf("/api/conversations/%d/stop", c.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop idle: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &resp)
	if resp.Status != "idle" {
		t.Fatalf("status=%q", resp.Status)
	}
}

func TestConversationSendGuards(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	c := mustCreateConversation(t, s, agentKey, "send guards")
	defer cleanupConversations(t, m, c.ID)
	send := fmt.Sprintf("/api/conversations/%d/messages", c.ID)

	// Force the global fallback off so behavior is deterministic regardless of
	// whether the shared test DB happens to hold an active LLM config.
	s.cfgMu.Lock()
	s.chatAgent = nil
	s.cfgMu.Unlock()

	// Empty message (no attachments) is rejected before anything else.
	if rec := crudDo(t, s, http.MethodPost, send, `{"message":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty message: status=%d", rec.Code)
	}

	// A conversation with no profile pin fails resolution with 503 — it never
	// silently proceeds without an agent.
	if rec := crudDo(t, s, http.MethodPost, send, `{"message":"hello"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-agent send: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Busy guard: pin a (fake-key) profile so resolution succeeds, then a second
	// send while a turn is marked in-flight is a 409. The 409 fires BEFORE any
	// LLM call, so the fake key is never exercised.
	profID, err := m.pg.SaveProfile(&db.LLMProfile{Name: fmt.Sprintf("conv-send-%d", time.Now().UnixNano()), Format: "anthropic", Model: "claude-x", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("save profile: %v", err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(profID) })
	if rec := crudDo(t, s, http.MethodPatch, fmt.Sprintf("/api/conversations/%d/profile", c.ID),
		fmt.Sprintf(`{"llm_profile_id":%d}`, profID)); rec.Code != http.StatusOK {
		t.Fatalf("pin profile: status=%d body=%s", rec.Code, rec.Body.String())
	}
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()
	if rec := crudDo(t, s, http.MethodPost, send, `{"message":"second"}`); rec.Code != http.StatusConflict {
		t.Fatalf("busy send: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTriggerRunHelpers(t *testing.T) {
	// taskContextHeader: goal variant, desc-only variant, empty context.
	if h := taskContextHeader(7, "desc", "goal"); h != "【任务 #7 desc（目标：goal）】" {
		t.Fatalf("header with goal=%q", h)
	}
	if h := taskContextHeader(7, "desc", ""); h != "【任务 #7 desc】" {
		t.Fatalf("header desc only=%q", h)
	}
	if h := taskContextHeader(0, "", ""); h != "" {
		t.Fatalf("empty header=%q", h)
	}

	// finalTriggerMessage prepends the header for single fires only.
	single := triggeredRun{taskID: 3, taskDesc: "d", message: "body"}
	if m := finalTriggerMessage(single); m != "【任务 #3 d】\nbody" {
		t.Fatalf("single message=%q", m)
	}

	// mergeTriggeredRuns: same-task burst → one header, numbered bodies.
	merged := mergeTriggeredRuns([]triggeredRun{
		{agentKey: "a", taskID: 5, taskDesc: "task five", message: "e1"},
		{agentKey: "a", taskID: 5, message: "e2"},
	})
	if merged.taskID != 5 || !merged.mergeable || merged.taskDesc != "" {
		t.Fatalf("merged meta=%+v", merged)
	}
	if !strings.Contains(merged.message, "【本会话合并了任务 #5 的 2 条触发事件，请一并处理】") ||
		!strings.Contains(merged.message, "【任务 #5 task five】") ||
		!strings.Contains(merged.message, "── 触发 1 ──\ne1") ||
		!strings.Contains(merged.message, "── 触发 2 ──\ne2") {
		t.Fatalf("merged body=%q", merged.message)
	}

	// mergeAllRuns: groups by task in first-appearance order, interleaved input ok.
	all := mergeAllRuns([]triggeredRun{
		{agentKey: "a", taskID: 1, taskDesc: "one", message: "a1"},
		{agentKey: "a", taskID: 2, taskDesc: "two", message: "b1"},
		{agentKey: "a", taskID: 1, message: "a2"},
	})
	if all.taskID != 1 || !all.mergeable || !strings.Contains(all.title, "3 条") {
		t.Fatalf("all meta title=%q taskID=%d", all.title, all.taskID)
	}
	// task#1's header appears once, its two events consecutive despite interleave.
	if got := all.message; strings.Count(got, "【任务 #1 one】") != 1 ||
		strings.Count(got, "【任务 #2 two】") != 1 ||
		!strings.Contains(got, "── 触发 2（task#1）──\na2") {
		t.Fatalf("all body=%q", got)
	}

	// firstLine: trims, cuts at the first newline, caps with an ellipsis on runes.
	if got := firstLine("  hi\r\nthere  ", 100); got != "hi" {
		t.Fatalf("firstLine crlf=%q", got)
	}
	if got := firstLine(strings.Repeat("汉", 60), 10); got != strings.Repeat("汉", 10)+"…" {
		t.Fatalf("firstLine cap=%q", got)
	}

	// userActivityWithAttachments: plain message keeps Detail empty at build time
	// (send route fills it from msg) — verified via JSON round-trip of the activity.
	ua := userActivityWithAttachments("agent_x", "hello world", nil)
	blob, err := json.Marshal(ua)
	if err != nil {
		t.Fatalf("marshal activity: %v", err)
	}
	if !strings.Contains(string(blob), `"kind":"user"`) || !strings.Contains(string(blob), `"worker":"agent_x"`) {
		t.Fatalf("activity json=%s", blob)
	}
}
