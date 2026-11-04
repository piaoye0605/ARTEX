package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// Coverage for server_exploration.go — the exploration read surface: intents,
// graph, 播报板 (nodes), activity list/history/detail, token stats and log
// endpoints. Seeding goes through the real ExplorationStore so assertions
// describe actual persistence, and the SSE handlers run against a pre-canceled
// request context so they replay history and return instead of streaming.

// seedExploration builds one task with a goal root, a terminal + an open intent,
// and activity rows covering every session kind (planner, mainagent segments,
// worker with detail + token usage).
func seedExploration(t *testing.T, m *Manager) (*Task, int64, int64, int64) {
	t.Helper()
	task, err := m.CreateTask("exploration routes", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	root, err := task.Store.AddNode(db.KindGoal, map[string]any{"text": "seed goal"}, 0, "open", "human", nil)
	if err != nil {
		t.Fatalf("add goal: %v", err)
	}
	doneIntent, err := task.Store.AddIntent(map[string]any{"title": "recon done"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add intent: %v", err)
	}
	if err := task.Store.SetNodeState(doneIntent, "done"); err != nil {
		t.Fatalf("finish intent: %v", err)
	}
	openIntent, err := task.Store.AddIntent(map[string]any{"title": "recon open"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add open intent: %v", err)
	}
	if err := task.Store.Link(root, db.RelSpawns, doneIntent); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Planner session row (session=plan).
	if _, err := task.Store.AppendActivity(db.Activity{Worker: "planner", Kind: "llm", Summary: "planner step"}); err != nil {
		t.Fatalf("append planner activity: %v", err)
	}
	// Mainagent original segment (session=main with COALESCE(seg,0)=0).
	if _, err := task.Store.AppendActivity(db.Activity{Worker: "mainagent", Kind: "note", Summary: "main segment zero"}); err != nil {
		t.Fatalf("append main activity: %v", err)
	}
	// Mainagent second segment (session=main:1).
	seg, err := task.Store.NewMainSession()
	if err != nil {
		t.Fatalf("new main session: %v", err)
	}
	if _, err := task.Store.AppendActivity(db.Activity{Worker: "mainagent", Kind: "note", Summary: "main segment one", MainSeg: &seg.Seq}); err != nil {
		t.Fatalf("append segmented activity: %v", err)
	}
	// Worker step bound to the terminal intent, with a lazy detail and tokens.
	in, out := 100, 200
	if _, err := task.Store.AppendActivity(db.Activity{
		NodeID: &doneIntent, Worker: "work#1", Kind: "result", Tool: "nmap",
		Summary: "scanned host", Detail: "full detail blob",
		InputTokens: &in, OutputTokens: &out,
	}); err != nil {
		t.Fatalf("append worker activity: %v", err)
	}
	return task, root, doneIntent, openIntent
}

// --- pure helpers ---

func TestInheritedIntentHelpers(t *testing.T) {
	for _, state := range []string{"done", "blocked", "exhausted", "stopped"} {
		if !inheritedIntentResult(state) {
			t.Fatalf("state %q must count as an inherited result", state)
		}
	}
	for _, state := range []string{"open", "running", ""} {
		if inheritedIntentResult(state) {
			t.Fatalf("state %q must not count as an inherited result", state)
		}
	}

	nodes := []*db.Node{
		{ID: 1, Kind: db.KindFact, State: "origin"},
		{ID: 2, Kind: db.KindIntent, State: "done"},
		{ID: 3, Kind: db.KindIntent, State: "running"},
		nil,
	}
	edges := []db.Edge{
		{From: 1, To: 2},
		{From: 2, To: 3}, // dangling once the running intent is hidden
		{From: 3, To: 2},
	}
	gotNodes, gotEdges := inheritedGraphSnapshot(nodes, edges, 77)
	if len(gotNodes) != 2 {
		t.Fatalf("nodes=%d, want 2 (fact + done intent)", len(gotNodes))
	}
	for _, n := range gotNodes {
		if n.SourceTaskID != 77 || !n.Inherited {
			t.Fatalf("node %d not stamped: source=%d inherited=%v", n.ID, n.SourceTaskID, n.Inherited)
		}
	}
	if len(gotEdges) != 1 || gotEdges[0].From != 1 || gotEdges[0].To != 2 {
		t.Fatalf("edges=%+v, want only 1->2", gotEdges)
	}
}

func TestParseActivitySession(t *testing.T) {
	// Bare "main" resolves to the current segment in the store.
	if f, ok := parseActivitySession(""); !ok || !f.Main || f.MainSeg != nil {
		t.Fatalf("empty session=%+v ok=%v", f, ok)
	}
	if f, ok := parseActivitySession("main"); !ok || !f.Main {
		t.Fatalf("main session=%+v ok=%v", f, ok)
	}
	// main:<seg> and intent:<id> parse their payloads.
	if f, ok := parseActivitySession("main:2"); !ok || f.MainSeg == nil || *f.MainSeg != 2 {
		t.Fatalf("main:2 → %+v ok=%v", f, ok)
	}
	if _, ok := parseActivitySession("main:x"); ok {
		t.Fatal("main:x must be rejected")
	}
	if _, ok := parseActivitySession("main:-1"); ok {
		t.Fatal("main:-1 must be rejected")
	}
	if f, ok := parseActivitySession("plan"); !ok || f.Worker != "planner" {
		t.Fatalf("plan → %+v ok=%v", f, ok)
	}
	if f, ok := parseActivitySession("intent:7"); !ok || f.NodeID == nil || *f.NodeID != 7 {
		t.Fatalf("intent:7 → %+v ok=%v", f, ok)
	}
	if _, ok := parseActivitySession("intent:abc"); ok {
		t.Fatal("intent:abc must be rejected")
	}
	if _, ok := parseActivitySession("weird"); ok {
		t.Fatal("unknown session must be rejected")
	}
}

func TestCSVValues(t *testing.T) {
	if got := csvValues("intent, goal ,,state"); len(got) != 3 || got[0] != "intent" || got[2] != "state" {
		t.Fatalf("csvValues=%+v", got)
	}
	if got := csvValues(""); got == nil || len(got) != 0 {
		t.Fatalf("empty csvValues=%#v, want empty non-nil slice", got)
	}
}

// --- intents ---

func TestIntentsEndpoint(t *testing.T) {
	m, s := newTestStack(t)
	task, _, doneIntent, openIntent := seedExploration(t, m)

	// Unresolvable task keeps the bare-array back-compat shape.
	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/intents?task=999", ""); rec.Code != http.StatusOK ||
		!strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Fatalf("unknown task: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Legacy form (no paging params) returns the bare array of intent DTOs.
	rec := crudDo(t, s, http.MethodGet, "/api/exploration/intents?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var legacy []map[string]any
	crudJSON(t, rec, &legacy)
	if len(legacy) != 2 {
		t.Fatalf("legacy intents=%d, want 2", len(legacy))
	}
	for _, it := range legacy {
		if it["type"] != "intent" {
			t.Fatalf("dto type=%v", it["type"])
		}
	}

	// Paged form returns {items, has_more} including the terminal intent.
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/intents?task="+task.ID+"&before=0&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("paged list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var paged struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	crudJSON(t, rec, &paged)
	if len(paged.Items) != 2 || paged.HasMore {
		t.Fatalf("paged items=%d has_more=%v", len(paged.Items), paged.HasMore)
	}
	// Newest first: the open intent leads.
	if paged.Items[0]["id"] != strconv.FormatInt(openIntent, 10) ||
		paged.Items[1]["id"] != strconv.FormatInt(doneIntent, 10) {
		t.Fatalf("order=%v/%v", paged.Items[0]["id"], paged.Items[1]["id"])
	}
	_ = doneIntent
}

// --- graph / 播报板 ---

func TestExplorationGraphEndpoint(t *testing.T) {
	m, s := newTestStack(t)
	task, root, doneIntent, _ := seedExploration(t, m)

	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/graph?task=999", ""); rec.Code != http.StatusOK {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}

	rec := crudDo(t, s, http.MethodGet, "/api/exploration/graph?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("graph: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var graph struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	crudJSON(t, rec, &graph)
	if len(graph.Nodes) < 3 {
		t.Fatalf("nodes=%d, want >= 3", len(graph.Nodes))
	}
	// Edge DTOs travel as {src, rel, dst} with string ids.
	foundEdge := false
	for _, e := range graph.Edges {
		if e["src"] == strconv.FormatInt(root, 10) &&
			e["dst"] == strconv.FormatInt(doneIntent, 10) && e["rel"] == db.RelSpawns {
			foundEdge = true
		}
	}
	if !foundEdge {
		t.Fatalf("root->intent edge missing from %+v (root=%d, done=%d)", graph.Edges, root, doneIntent)
	}
}

func TestExplorationNodesEndpoint(t *testing.T) {
	m, s := newTestStack(t)
	task, _, _, _ := seedExploration(t, m)

	// Unknown task → fully-shaped empty page.
	rec := crudDo(t, s, http.MethodGet, "/api/exploration/nodes?task=999&page=1&size=20", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	var empty struct {
		Total int `json:"total"`
	}
	crudJSON(t, rec, &empty)
	if empty.Total != 0 {
		t.Fatalf("unknown task total=%d", empty.Total)
	}

	base := "/api/exploration/nodes?task=" + task.ID
	// Unfiltered page reports every seeded node.
	var board struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
		Refs  map[string]any   `json:"refs"`
	}
	rec = crudDo(t, s, http.MethodGet, base+"&page=1&size=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("board: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &board)
	if board.Total < 3 || len(board.Items) < 3 {
		t.Fatalf("board total=%d items=%d", board.Total, len(board.Items))
	}

	// Kind filter narrows to intents only.
	rec = crudDo(t, s, http.MethodGet, base+"&page=1&size=10&kind=intent", "")
	crudJSON(t, rec, &board)
	if board.Total != 2 {
		t.Fatalf("intent filter total=%d, want 2", board.Total)
	}

	// State filter picks just the terminal intent.
	rec = crudDo(t, s, http.MethodGet, base+"&page=1&size=10&kind=intent&state=done", "")
	crudJSON(t, rec, &board)
	if board.Total != 1 || board.Items[0]["state"] != "done" {
		t.Fatalf("state filter total=%d items=%+v", board.Total, board.Items)
	}

	// Payload substring search.
	rec = crudDo(t, s, http.MethodGet, base+"&page=1&size=10&q=recon+done", "")
	crudJSON(t, rec, &board)
	if board.Total != 1 {
		t.Fatalf("query filter total=%d, want 1", board.Total)
	}

	// Ascending order flips the page order.
	rec = crudDo(t, s, http.MethodGet, base+"&page=1&size=10&order=asc", "")
	crudJSON(t, rec, &board)
	if len(board.Items) < 2 && board.Items[len(board.Items)-1]["id"] == board.Items[0]["id"] {
		t.Fatalf("asc order not applied: %+v", board.Items)
	}
}

// --- activity list / history / detail ---

func TestActivityEndpoint(t *testing.T) {
	m, s := newTestStack(t)
	task, _, doneIntent, _ := seedExploration(t, m)

	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity?task=999", ""); rec.Code != http.StatusOK {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}

	rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items  []map[string]any `json:"items"`
		Cursor int64            `json:"cursor"`
	}
	crudJSON(t, rec, &list)
	if len(list.Items) != 4 || list.Cursor <= 0 {
		t.Fatalf("items=%d cursor=%d, want 4 rows and a cursor", len(list.Items), list.Cursor)
	}

	// Intent-scoped read only returns that session's steps.
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/activity?task="+task.ID+"&intent="+strconv.FormatInt(doneIntent, 10), "")
	crudJSON(t, rec, &list)
	if len(list.Items) != 1 || list.Items[0]["summary"] != "scanned host" {
		t.Fatalf("intent scope items=%+v", list.Items)
	}
}

func TestActivityHistoryEndpoint(t *testing.T) {
	m, s := newTestStack(t)
	task, _, doneIntent, _ := seedExploration(t, m)

	// Unknown task and malformed session reject early.
	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity/history?task=999&session=main", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity/history?task="+task.ID+"&session=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad session: status=%d", rec.Code)
	}

	var page struct {
		Items    []map[string]any `json:"items"`
		Snapshot int64            `json:"snapshot_cursor"`
		HasMore  bool             `json:"has_more"`
		Earliest int64            `json:"earliest_cursor"`
	}
	open := func(session string) {
		t.Helper()
		rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity/history?task="+task.ID+"&session="+session+"&limit=50", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("session %s: status=%d body=%s", session, rec.Code, rec.Body.String())
		}
		crudJSON(t, rec, &page)
	}
	// Bare "main" resolves the current segment (1 after NewMainSession) and must
	// return only that segment's row.
	open("main")
	if len(page.Items) != 1 || page.Items[0]["summary"] != "main segment one" {
		t.Fatalf("main session items=%+v", page.Items)
	}
	if page.Snapshot <= 0 || page.Earliest <= 0 {
		t.Fatalf("cursors snapshot=%d earliest=%d", page.Snapshot, page.Earliest)
	}
	// main:0 reaches the original segment's legacy row.
	open("main:0")
	if len(page.Items) != 1 || page.Items[0]["summary"] != "main segment zero" {
		t.Fatalf("main:0 items=%+v", page.Items)
	}
	// plan scopes to the planner worker.
	open("plan")
	if len(page.Items) != 1 || page.Items[0]["summary"] != "planner step" {
		t.Fatalf("plan items=%+v", page.Items)
	}
	// intent:<id> opens the worker transcript.
	open("intent:" + strconv.FormatInt(doneIntent, 10))
	if len(page.Items) != 1 || page.Items[0]["summary"] != "scanned host" {
		t.Fatalf("intent session items=%+v", page.Items)
	}
	// A non-intent node id has no session.
	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity/history?task="+task.ID+"&session=intent:424242", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing intent session: status=%d", rec.Code)
	}
}

func TestActivityDetailEndpoint(t *testing.T) {
	m, s := newTestStack(t)
	task, _, doneIntent, _ := seedExploration(t, m)

	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity/1?task=999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}

	// Find the worker step's activity id via the intent-scoped list.
	rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity?task="+task.ID+"&intent="+strconv.FormatInt(doneIntent, 10), "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	crudJSON(t, rec, &list)
	if len(list.Items) != 1 {
		t.Fatalf("cannot locate worker step: %+v", list.Items)
	}
	// The activity DTO exposes its id as "seq".
	seq := int64(list.Items[0]["seq"].(float64))

	rec = crudDo(t, s, http.MethodGet, "/api/exploration/activity/"+strconv.FormatInt(seq, 10)+"?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Detail string `json:"detail"`
	}
	crudJSON(t, rec, &detail)
	if detail.Detail != "full detail blob" {
		t.Fatalf("detail=%q", detail.Detail)
	}
}

// --- token stats ---

func TestTokenStatsEndpoints(t *testing.T) {
	m, s := newTestStack(t)
	task, _, _, _ := seedExploration(t, m)

	// Unknown task keeps the empty shape.
	rec := crudDo(t, s, http.MethodGet, "/api/exploration/tokens?task=999", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}
	var emptyStats struct {
		Workers []map[string]any `json:"workers"`
	}
	crudJSON(t, rec, &emptyStats)
	if len(emptyStats.Workers) != 0 {
		t.Fatalf("unknown task workers=%+v", emptyStats.Workers)
	}

	rec = crudDo(t, s, http.MethodGet, "/api/exploration/tokens?task="+task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tokens: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var stats struct {
		Workers []map[string]any `json:"workers"`
		Total   map[string]int   `json:"total"`
	}
	crudJSON(t, rec, &stats)
	found := false
	for _, w := range stats.Workers {
		if w["worker"] == "work#1" {
			found = true
			if w["input_tokens"].(float64) != 100 || w["output_tokens"].(float64) != 200 {
				t.Fatalf("work#1 tokens=%+v", w)
			}
		}
	}
	if !found {
		t.Fatalf("workers=%+v, want work#1", stats.Workers)
	}
	if stats.Total["input_tokens"] != 100 || stats.Total["output_tokens"] != 200 {
		t.Fatalf("total=%+v", stats.Total)
	}

	// Global dashboards stay well-formed on an empty ledger.
	if rec := crudDo(t, s, http.MethodGet, "/api/tokens/daily?days=7", ""); rec.Code != http.StatusOK {
		t.Fatalf("daily: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/tokens/conversations", ""); rec.Code != http.StatusOK {
		t.Fatalf("conversations: status=%d", rec.Code)
	}
}

// --- logs ---

func TestLogsEndpoints(t *testing.T) {
	_, s := newTestStack(t)
	rec := crudDo(t, s, http.MethodGet, "/api/logs?since=0&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs: status=%d", rec.Code)
	}
	var logs struct {
		Items  []map[string]any `json:"items"`
		Cursor int64            `json:"cursor"`
	}
	crudJSON(t, rec, &logs)

	rec = crudDo(t, s, http.MethodGet, "/api/logs/history?before=0&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs history: status=%d", rec.Code)
	}
	var history struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	crudJSON(t, rec, &history)
}

// --- SSE streams (replay-only via pre-canceled context) ---

func TestSSEStreamReplay(t *testing.T) {
	m, s := newTestStack(t)
	task, _, _, _ := seedExploration(t, m)

	// Unknown task is a plain 404, no stream.
	if rec := crudDo(t, s, http.MethodGet, "/api/exploration/activity/stream?task=999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d", rec.Code)
	}

	// Pre-canceled context: the handler replays the DB backlog, then the select
	// loop sees ctx.Done() and returns. The body must carry the seeded rows.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/exploration/activity/stream?task="+task.ID, nil).
		WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+mustToken(t, s))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"summary":"planner step"`) ||
		!strings.Contains(body, `"summary":"scanned host"`) {
		t.Fatalf("stream replay missing seeded rows: %s", body)
	}
	// SSE frames carry the event id for Last-Event-ID reconnects.
	if !strings.Contains(body, "\nid: ") {
		t.Fatalf("stream missing id lines: %s", body[:min(len(body), 400)])
	}

	// Log stream sets the SSE headers even with nothing to replay.
	req = httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+mustToken(t, s))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("log stream: status=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func mustToken(t *testing.T, s *Server) string {
	t.Helper()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return token
}
