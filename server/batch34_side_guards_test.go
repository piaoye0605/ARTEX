package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// TestSideRouteAndPayloadGuards exercises the guard branches of the side-question
// routes: parent resolution errors (bad id / not found / stopped worker), payload
// validation (bad json, length, request id), snapshot/provider admission errors,
// and request-id idempotency (same id: same question replay, different question 409).
func TestSideRouteAndPayloadGuards(t *testing.T) {
	f := newSideHTTPFixture(t)

	// --- parent resolution guards ---
	f.call(t, "GET", "/api/conversations/0/side-questions", nil, 400)
	f.call(t, "GET", "/api/conversations/abc/side-questions", nil, 400)
	f.call(t, "GET", "/api/conversations/999999999/side-questions", nil, 404)
	f.call(t, "GET", "/api/tasks/99999999/chat/side-questions", nil, 404)

	task, err := f.m.CreateTask("side guards task", "context", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { _ = f.m.pg.DeleteTask(tid) })
	f.call(t, "GET", "/api/tasks/"+task.ID+"/intents/abc/side-questions", nil, 400)
	f.call(t, "GET", "/api/tasks/"+task.ID+"/intents/0/side-questions", nil, 400)

	iid, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "guard worker"}, 1, "paused", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", fmt.Sprintf("/api/tasks/%s/intents/%d/side-questions", task.ID, iid+500000), nil, 404)
	// a stopped worker (soft-stopped via SetNodeState) must be rejected with 409.
	if err := task.Store.SetNodeState(iid, "stopped"); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", fmt.Sprintf("/api/tasks/%s/intents/%d/side-questions", task.ID, iid), nil, 409)

	// --- payload guards ---
	p, path := f.conversation(t)
	f.checkpoint(t, p)

	// malformed json → 400 "bad json"
	req := httptest.NewRequest("POST", path, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	if w.Code != 400 || strings.Contains(w.Body.String(), "问题须为") {
		t.Fatalf("bad json: code=%d body=%s", w.Code, w.Body.String())
	}

	f.call(t, "POST", path, map[string]string{"question": "   ", "client_request_id": "ok-id"}, 400)
	f.call(t, "POST", path, map[string]string{"question": strings.Repeat("长", 4001), "client_request_id": "ok-id"}, 400)
	f.call(t, "POST", path, map[string]string{"question": "q", "client_request_id": "bad id!"}, 400)

	// no snapshot yet → 409
	_, emptyPath := f.conversation(t)
	f.call(t, "POST", emptyPath, map[string]string{"question": "q", "client_request_id": "nosnap"}, 409)

	// provider unavailable → 409
	f.s.cfgMu.Lock()
	f.s.llmOn = false
	f.s.cfgMu.Unlock()
	f.call(t, "POST", path, map[string]string{"question": "q", "client_request_id": "nollm"}, 409)
	f.s.cfgMu.Lock()
	f.s.llmOn = true
	f.s.cfgMu.Unlock()

	// admission idempotency: replay semantics on client_request_id.
	e := decodeSide(t, f.call(t, "POST", path, map[string]string{"question": "replay me", "client_request_id": "replay-1"}, 202))
	f.call(t, "POST", path, map[string]string{"question": "a DIFFERENT question", "client_request_id": "replay-1"}, 409)
	w2 := f.call(t, "POST", path, map[string]string{"question": "replay me", "client_request_id": "replay-1"}, 200)
	var replayed sidequestionExchange
	if err := json.Unmarshal(w2.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != e.ID {
		t.Fatalf("replay returned different exchange %s != %s", replayed.ID, e.ID)
	}

	f.call(t, "POST", "/api/side-questions/"+e.ID+"/cancel", nil, 200)
	waitSide(t, f.m.pg, e.ID, "cancelled")

	// ghost request id → 404 on both cancel and events.
	f.call(t, "POST", "/api/side-questions/ghost-request/cancel", nil, 404)
	f.call(t, "GET", "/api/side-questions/ghost-request/events", nil, 404)
}

// sidequestionExchange avoids importing sidequestion here (already imported by
// side_questions_test.go in the same package); only the id field is needed.
type sidequestionExchange struct {
	ID string `json:"id"`
}

// TestSideHistoryPaginationCursors covers the GET listing branch: >20 items are
// truncated to 20 with a next_cursor, and the before cursor slices the window.
func TestSideHistoryPaginationCursors(t *testing.T) {
	f := newSideHTTPFixture(t)
	p, path := f.conversation(t)
	for i := 0; i < 23; i++ {
		e, _, err := f.m.pg.StartSideRequest(t.Context(), f.checkpoint(t, p), fmt.Sprintf("hist-%d", i), fmt.Sprintf("q %d", i))
		if err != nil {
			t.Fatal(err)
		}
		e.Answer = "a"
		e.Sequence = 1
		e.Status = "completed"
		if _, err = f.m.pg.UpdateSideRequest(t.Context(), *e); err != nil {
			t.Fatal(err)
		}
	}

	w := f.call(t, "GET", path, nil, 200)
	var page struct {
		Items      []struct{ Ordinal int64 } `json:"items"`
		NextCursor int64                     `json:"next_cursor"`
		Snapshot   map[string]any            `json:"snapshot"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 20 || page.NextCursor == 0 {
		t.Fatalf("first page = %d items, cursor=%d", len(page.Items), page.NextCursor)
	}
	if snap := page.Snapshot; snap == nil || snap["available"] != true {
		t.Fatalf("snapshot meta = %+v", page.Snapshot)
	}

	// follow the cursor: remaining 3 items, no further cursor.
	w2 := f.call(t, "GET", path+"?before="+strconv.FormatInt(page.NextCursor, 10), nil, 200)
	var page2 struct {
		Items      []struct{ Ordinal int64 } `json:"items"`
		NextCursor int64                     `json:"next_cursor"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) != 3 || page2.NextCursor != 0 {
		t.Fatalf("second page = %d items, cursor=%d", len(page2.Items), page2.NextCursor)
	}

	// provider gone at read time → snapshot.available flips false with a reason.
	f.s.cfgMu.Lock()
	f.s.llmOn = false
	f.s.cfgMu.Unlock()
	w3 := f.call(t, "GET", path, nil, 200)
	f.s.cfgMu.Lock()
	f.s.llmOn = true
	f.s.cfgMu.Unlock()
	var page3 struct {
		Snapshot map[string]any `json:"snapshot"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &page3); err != nil {
		t.Fatal(err)
	}
	if page3.Snapshot == nil || page3.Snapshot["available"] != false || page3.Snapshot["reason"] == "" {
		t.Fatalf("unavailable snapshot meta = %+v", page3.Snapshot)
	}
}

// TestSideCancelUnknownAndIdleDelete covers cancel on an already-finished
// request (still 200 — cancel is a DB-touching idempotent op) and DELETE
// clearing of the conversation history.
func TestSideCancelFinishedAndClearHistory(t *testing.T) {
	f := newSideHTTPFixture(t)
	p, path := f.conversation(t)
	e, _, err := f.m.pg.StartSideRequest(t.Context(), f.checkpoint(t, p), "done-1", "already done")
	if err != nil {
		t.Fatal(err)
	}
	e.Status = "completed"
	if _, err = f.m.pg.UpdateSideRequest(t.Context(), *e); err != nil {
		t.Fatal(err)
	}
	// cancel on a finished (not running) request still succeeds.
	f.call(t, "POST", "/api/side-questions/"+e.ID+"/cancel", nil, 200)
	// clear history for the parent.
	f.call(t, "DELETE", path, nil, 200)
	// history is now empty.
	w := f.call(t, "GET", path, nil, 200)
	var page struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("history not cleared: %d items", len(page.Items))
	}
	_ = time.Now()
}
