package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Coverage for the RUN half of conversations.go — the parts the guard tests in
// conversations_routes_test.go cannot reach: the send route's success path
// (202 + background turn + auto-title) and the P3 trigger queue end to end.
// The LLM side is a real provider pointed at a local httptest server that
// answers one valid non-streaming Anthropic message, so the turn completes in
// a single round-trip with no network.

// newScriptedLLMProfile saves an anthropic-format profile whose BaseURL is a
// local server returning a minimal valid completion, with every retry layer
// disabled for determinism. Returns the profile id.
func newScriptedLLMProfile(t *testing.T, m *Manager) int64 {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"收到，模拟回复"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	id, err := m.pg.SaveProfile(&db.LLMProfile{
		Name:      fmt.Sprintf("conv-run-%d", time.Now().UnixNano()),
		Format:    "anthropic",
		Model:     "claude-test",
		BaseURL:   srv.URL,
		APIKey:    "sk-test-scripted",
		Streaming: false, // force Provider.Complete: one JSON round-trip
		Retry: db.RetryOverride{
			Connect: db.RetryRule{Attempts: -1},
			Empty:   db.RetryRule{Attempts: -1},
			Stream:  db.RetryRule{Attempts: -1},
		},
	})
	if err != nil {
		t.Fatalf("save profile: %v", err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(id) })
	return id
}

// waitConvActivity polls the conversation's incremental tail until cond sees
// what it waits for (or the deadline passes).
func waitConvActivity(t *testing.T, m *Manager, convID int64, cond func([]db.Activity) bool) []db.Activity {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		items, _, err := m.pg.ConvActivityList(convID, 0, 1000)
		if err != nil {
			t.Fatalf("conv activity list: %v", err)
		}
		if cond(items) {
			return items
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for condition; activity=%+v", items)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConversationSendRunsAgentTurn(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	profID := newScriptedLLMProfile(t, m)

	// Empty title → the auto-title path runs after the first send.
	c := mustCreateConversation(t, s, agentKey, "")
	defer cleanupConversations(t, m, c.ID)
	if rec := crudDo(t, s, http.MethodPatch, fmt.Sprintf("/api/conversations/%d/profile", c.ID),
		fmt.Sprintf(`{"llm_profile_id":%d}`, profID)); rec.Code != http.StatusOK {
		t.Fatalf("pin profile: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The send route answers 202 immediately; the turn runs in the background
	// and completes against the scripted endpoint.
	rec := crudDo(t, s, http.MethodPost, fmt.Sprintf("/api/conversations/%d/messages", c.ID), `{"message":"你好 巡查现场"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: status=%d body=%s (want 202)", rec.Code, rec.Body.String())
	}
	var ack struct {
		Status string `json:"status"`
	}
	crudJSON(t, rec, &ack)
	if ack.Status != "accepted" {
		t.Fatalf("ack=%+v", ack)
	}

	// Wait for the turn to finish (busy marker cleared), then verify the
	// transcript: the human turn first, then the assistant reply.
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.chatMu.Lock()
		busy := s.chatBusy[s.convBusyKey(c.ID)]
		s.chatMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never finished (busy marker stuck)")
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitConvActivity(t, m, c.ID, func(items []db.Activity) bool {
		var user, reply bool
		for _, it := range items {
			if it.Worker == agentKey && it.Kind == "user" && strings.Contains(it.Summary, "你好 巡查现场") {
				user = true
			}
			if !it.IsError && strings.Contains(it.Summary, "收到，模拟回复") {
				reply = true
			}
		}
		return user && reply
	})

	// Auto-title: an untitled conversation takes its name from the first line.
	updated, err := m.pg.GetConversation(c.ID)
	if err != nil || updated == nil {
		t.Fatalf("reload conversation: %v", err)
	}
	if !strings.Contains(updated.Title, "你好 巡查现场") {
		t.Fatalf("auto title=%q", updated.Title)
	}

	// The turn end also dropped the cancel registration.
	s.chatMu.Lock()
	_, hadCancel := s.chatCancel[s.convBusyKey(c.ID)]
	s.chatMu.Unlock()
	if hadCancel {
		t.Fatal("cancel registration survived the finished turn")
	}
}

func TestTriggeredRunEndToEnd(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	profID := newScriptedLLMProfile(t, m)

	// Build the chat runner the scheduler requires and install it as the global
	// chat agent (StartTriggeredRun bails out early without one).
	ca := s.chatAgentForProfile(profID)
	if ca == nil {
		t.Fatal("chatAgentForProfile returned nil for a saved profile")
	}
	s.cfgMu.Lock()
	prevAgent := s.chatAgent
	s.chatAgent = ca
	s.cfgMu.Unlock()
	t.Cleanup(func() {
		s.cfgMu.Lock()
		s.chatAgent = prevAgent
		s.cfgMu.Unlock()
	})

	// One serial fire with task context. runTriggeredRun creates the
	// conversation, records the human turn (header + body) and runs the agent,
	// which fails fast against the broken provider.
	s.StartTriggeredRun(agentKey, "巡检触发标题", "触发事件正文", 42, true, "目标描述四十二", "目标goal四十二")

	// Find the conversation the scheduler created for our agent key.
	deadline := time.Now().Add(15 * time.Second)
	var conv *db.Conversation
	for conv == nil {
		cs, err := m.pg.ListConversations()
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		for _, c := range cs {
			if c.AgentKey == agentKey {
				conv = c
				break
			}
		}
		if conv != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conv == nil {
		t.Fatal("triggered run never created a conversation")
	}
	defer cleanupConversations(t, m, conv.ID)

	// Title is the capped first line of the fire title.
	if conv.Title != "巡检触发标题" {
		t.Fatalf("trigger conversation title=%q", conv.Title)
	}

	// The turn ran to completion: the human turn carries the task-context
	// header (the list endpoint is summary-only; Detail is lazy), and the
	// assistant reply followed.
	var userStepID int64
	waitConvActivity(t, m, conv.ID, func(items []db.Activity) bool {
		for _, it := range items {
			if it.Kind == "user" && strings.Contains(it.Summary, "【任务 #42 目标描述四十二（目标：目标goal四十二）】") {
				userStepID = it.ID
			}
			if !it.IsError && strings.Contains(it.Summary, "收到，模拟回复") {
				return userStepID > 0
			}
		}
		return false
	})
	// The lazy detail of the user turn carries the full body after the header.
	detail, err := m.pg.ConvActivityDetail(conv.ID, userStepID)
	if err != nil {
		t.Fatalf("user turn detail: %v", err)
	}
	if !strings.Contains(detail, "【任务 #42 目标描述四十二（目标：目标goal四十二）】") ||
		!strings.Contains(detail, "触发事件正文") {
		t.Fatalf("user turn detail=%q", detail)
	}

	// The queue drains asynchronously after the turn's last activity lands —
	// poll until runAndPump has decremented the active counter.
	deadline = time.Now().Add(10 * time.Second)
	for {
		s.queueMu.Lock()
		active, queued := s.triggerActive[agentKey], len(s.triggerQ[agentKey])
		s.queueMu.Unlock()
		if active == 0 && queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue not drained: active=%d queued=%d", active, queued)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConversationStopWithInFlightTurn(t *testing.T) {
	m, s := newTestStack(t)
	agentKey := convTestAgent(t, m)
	c := mustCreateConversation(t, s, agentKey, "stop inflight")
	defer cleanupConversations(t, m, c.ID)

	// Register a live cancel like a running turn would (conversationRunContext).
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatCancel[busyKey] = cancel
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()
	t.Cleanup(func() {
		s.chatMu.Lock()
		delete(s.chatCancel, busyKey)
		delete(s.chatBusy, busyKey)
		s.chatMu.Unlock()
	})

	var resp struct {
		Status string `json:"status"`
	}
	rec := crudDo(t, s, http.MethodPost, fmt.Sprintf("/api/conversations/%d/stop", c.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &resp)
	if resp.Status != "stopping" {
		t.Fatalf("status=%q, want stopping", resp.Status)
	}
}
