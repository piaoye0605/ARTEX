package server

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Coverage for the source-task half of server_exploration.go: everything the
// local-task tests in exploration_routes_test.go cannot reach. A child task
// (CreateTaskWithOptions with SourceTaskIDs) reads its source's TERMINAL intent
// history — open/running intents and their transcripts stay hidden.

// seedSourceTask builds a source task holding a goal root, a terminal ("done")
// and a blocked intent plus an open intent, and worker activity bound to each.
// Only the terminal intents' transcripts may leak into a child task.
func seedSourceTask(t *testing.T, m *Manager) (*Task, int64, int64, int64, int64) {
	t.Helper()
	task, err := m.CreateTask(fmt.Sprintf("inherited source %d", time.Now().UnixNano()), "source goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create source task: %v", err)
	}
	root, err := task.Store.AddNode(db.KindGoal, map[string]any{"text": "source root"}, 0, "open", "human", nil)
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	doneIntent, err := task.Store.AddIntent(map[string]any{"title": "done recon"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add done intent: %v", err)
	}
	if err := task.Store.SetNodeState(doneIntent, "done"); err != nil {
		t.Fatalf("finish intent: %v", err)
	}
	blockedIntent, err := task.Store.AddIntent(map[string]any{"title": "blocked recon"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add blocked intent: %v", err)
	}
	if err := task.Store.SetNodeState(blockedIntent, "blocked"); err != nil {
		t.Fatalf("block intent: %v", err)
	}
	openIntent, err := task.Store.AddIntent(map[string]any{"title": "still running"}, 0, nil, "planner")
	if err != nil {
		t.Fatalf("add open intent: %v", err)
	}
	if err := task.Store.Link(root, db.RelSpawns, doneIntent); err != nil {
		t.Fatalf("link root: %v", err)
	}
	// Two transcript steps on the terminal intent (for ?before paging), one on
	// the open intent (must never be readable through the child).
	if _, err := task.Store.AppendActivity(db.Activity{NodeID: &doneIntent, Worker: "work#1", Kind: "result", Summary: "first source step", Detail: "first source detail"}); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if _, err := task.Store.AppendActivity(db.Activity{NodeID: &doneIntent, Worker: "work#1", Kind: "result", Summary: "second source step"}); err != nil {
		t.Fatalf("append second: %v", err)
	}
	if _, err := task.Store.AppendActivity(db.Activity{NodeID: &openIntent, Worker: "work#2", Kind: "result", Summary: "open intent secret"}); err != nil {
		t.Fatalf("append open: %v", err)
	}
	return task, root, doneIntent, blockedIntent, openIntent
}

func childTaskOf(t *testing.T, m *Manager, sourceID int64) *Task {
	t.Helper()
	child, err := m.CreateTaskWithOptions(fmt.Sprintf("inherited child %d", time.Now().UnixNano()), "child goal", db.TaskCreateOptions{
		SourceTaskIDs: []int64{sourceID},
	})
	if err != nil {
		t.Fatalf("create child task: %v", err)
	}
	return child
}

func TestIntentsInheritedFromSource(t *testing.T) {
	m, s := newTestStack(t)
	source, _, doneIntent, blockedIntent, openIntent := seedSourceTask(t, m)
	child := childTaskOf(t, m, taskIDOf(source))

	// Paged form on the child: its own frontier is empty, so the page carries
	// ONLY the source's terminal intents (newest id first), stamped inherited.
	var paged struct {
		Items []map[string]any `json:"items"`
		More  bool             `json:"has_more"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/exploration/intents?task="+child.ID+"&before=0&limit=50", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("child intents: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &paged)
	ids := map[string]bool{}
	for _, it := range paged.Items {
		ids[it["id"].(string)] = true
		if it["inherited"] != true || it["source_task_id"] != strconv.FormatInt(taskIDOf(source), 10) {
			t.Fatalf("item not stamped inherited: %v", it)
		}
	}
	if !ids[strconv.FormatInt(doneIntent, 10)] {
		t.Fatalf("terminal intent %d missing from %v", doneIntent, ids)
	}
	if ids[strconv.FormatInt(openIntent, 10)] {
		t.Fatal("open source intent leaked into child history")
	}
	if paged.More {
		t.Fatal("has_more should be false with everything on one page")
	}

	// A tight limit truncates the merged page and reports has_more.
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/intents?task="+child.ID+"&before=0&limit=1", "")
	crudJSON(t, rec, &paged)
	if len(paged.Items) != 1 || !paged.More {
		t.Fatalf("tight limit: items=%d has_more=%v, want 1/true", len(paged.Items), paged.More)
	}
	// The one surviving item is the newest terminal intent by id. Both source
	// intents are terminal; the larger id wins.
	want := strconv.FormatInt(doneIntent, 10)
	if blockedIntent > doneIntent {
		want = strconv.FormatInt(blockedIntent, 10)
	}
	if paged.Items[0]["id"].(string) != want {
		t.Fatalf("wrong survivor: %v (want %s)", paged.Items[0]["id"], want)
	}
}

func TestExplorationGraphInherited(t *testing.T) {
	m, s := newTestStack(t)
	source, root, doneIntent, _, openIntent := seedSourceTask(t, m)
	child := childTaskOf(t, m, taskIDOf(source))

	rec := crudDo(t, s, http.MethodGet, "/api/exploration/graph?task="+child.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("child graph: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var graph struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	crudJSON(t, rec, &graph)

	seen := map[string]map[string]any{}
	for _, n := range graph.Nodes {
		seen[n["id"].(string)] = n
	}
	doneDTO, ok := seen[strconv.FormatInt(doneIntent, 10)]
	if !ok {
		t.Fatalf("terminal intent missing from child graph: %v", seen)
	}
	if doneDTO["inherited"] != true || doneDTO["source_task_id"] != strconv.FormatInt(taskIDOf(source), 10) {
		t.Fatalf("done intent not stamped: %v", doneDTO)
	}
	if _, leaking := seen[strconv.FormatInt(openIntent, 10)]; leaking {
		t.Fatal("open source intent leaked into child graph")
	}
	// The root→intent edge survived the visibility filter (both ends visible).
	edgeOK := false
	for _, e := range graph.Edges {
		if e["src"] == strconv.FormatInt(root, 10) && e["dst"] == strconv.FormatInt(doneIntent, 10) {
			edgeOK = true
		}
	}
	if !edgeOK {
		t.Fatalf("inherited edge missing from %+v", graph.Edges)
	}
}

func TestActivityHistoryInheritedIntentSession(t *testing.T) {
	m, s := newTestStack(t)
	source, _, doneIntent, _, openIntent := seedSourceTask(t, m)
	child := childTaskOf(t, m, taskIDOf(source))

	// The terminal source intent's transcript opens through the child.
	var page struct {
		Items []map[string]any `json:"items"`
		More  bool             `json:"has_more"`
	}
	rec := crudDo(t, s, http.MethodGet,
		"/api/exploration/activity/history?task="+child.ID+"&session=intent:"+strconv.FormatInt(doneIntent, 10)+"&limit=50", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("inherited session: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &page)
	if len(page.Items) != 2 {
		t.Fatalf("inherited items=%d, want both source steps", len(page.Items))
	}
	for _, it := range page.Items {
		if it["source_task_id"] != strconv.FormatInt(taskIDOf(source), 10) || it["inherited"] != true {
			t.Fatalf("step not stamped: %v", it)
		}
	}
	// The inherited transcript page comes back oldest-first.
	if page.Items[0]["summary"] != "first source step" {
		t.Fatalf("page order wrong: %+v", page.Items)
	}

	// ?before pages past the newer step: before=<first id> yields nothing newer
	// here, so instead page with before=<second id> to get the first step only.
	secondSeq := page.Items[1]["seq"].(float64)
	rec = crudDo(t, s, http.MethodGet,
		"/api/exploration/activity/history?task="+child.ID+"&session=intent:"+strconv.FormatInt(doneIntent, 10)+
			"&limit=50&before="+strconv.FormatInt(int64(secondSeq), 10), "")
	crudJSON(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0]["summary"] != "first source step" {
		t.Fatalf("older page=%+v", page.Items)
	}

	// The open intent has no readable session from the child.
	if rec := crudDo(t, s, http.MethodGet,
		"/api/exploration/activity/history?task="+child.ID+"&session=intent:"+strconv.FormatInt(openIntent, 10), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("open intent session: status=%d body=%s (want 404)", rec.Code, rec.Body.String())
	}
}

func TestActivityDetailInherited(t *testing.T) {
	m, s := newTestStack(t)
	source, _, doneIntent, _, _ := seedSourceTask(t, m)
	child := childTaskOf(t, m, taskIDOf(source))

	// Resolve the terminal intent's first step id via the child's inherited page.
	var page struct {
		Items []map[string]any `json:"items"`
	}
	rec := crudDo(t, s, http.MethodGet,
		"/api/exploration/activity/history?task="+child.ID+"&session=intent:"+strconv.FormatInt(doneIntent, 10)+"&limit=50", "")
	crudJSON(t, rec, &page)
	var firstStepID int64
	for _, it := range page.Items {
		if it["summary"] == "first source step" {
			firstStepID = int64(it["seq"].(float64))
		}
	}
	if firstStepID == 0 {
		t.Fatalf("first source step missing: %+v", page.Items)
	}

	// Lazy detail of an inherited step resolves through the source store.
	var detail struct {
		Detail string `json:"detail"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/activity/"+strconv.FormatInt(firstStepID, 10)+"?task="+child.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("inherited detail: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &detail)
	if detail.Detail != "first source detail" {
		t.Fatalf("detail=%q", detail.Detail)
	}
}

func TestExplorationNodesAnchoredAssets(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()

	// One asset to anchor to (created through the real insert route).
	rec := crudDo(t, s, http.MethodPost, "/api/assets",
		fmt.Sprintf(`{"task_id":0,"assets":[{"type":"root_domain","domain":"anchor-%d.example.com"}]}`, stamp))
	if rec.Code != http.StatusOK {
		t.Fatalf("insert asset: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Results []struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"results"`
		Errors []map[string]any `json:"errors"`
	}
	crudJSON(t, rec, &env)
	if len(env.Results) != 1 || len(env.Errors) != 0 || env.Results[0].ID == 0 {
		t.Fatalf("asset envelope=%+v", env)
	}
	assetID := env.Results[0].ID

	task, err := m.CreateTask(fmt.Sprintf("anchored nodes %d", stamp), "anchor goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	anchored, err := task.Store.AddNode(db.KindFact, map[string]any{"text": "anchored fact"}, 0, "confirmed", "human", []int64{assetID})
	if err != nil {
		t.Fatalf("add anchored node: %v", err)
	}
	bare, err := task.Store.AddNode(db.KindFact, map[string]any{"text": "plain fact"}, 0, "confirmed", "human", nil)
	if err != nil {
		t.Fatalf("add plain node: %v", err)
	}

	var board struct {
		Items  []map[string]any `json:"items"`
		Assets map[string][]struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"assets"`
	}
	rec = crudDo(t, s, http.MethodGet, "/api/exploration/nodes?task="+task.ID+"&page=1&size=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("board: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &board)
	got, ok := board.Assets[strconv.FormatInt(anchored, 10)]
	if !ok || len(got) != 1 || got[0].ID != strconv.FormatInt(assetID, 10) {
		t.Fatalf("anchored assets=%v (node %d, asset %d)", board.Assets, anchored, assetID)
	}
	if _, still := board.Assets[strconv.FormatInt(bare, 10)]; still {
		t.Fatal("unanchored node has an assets entry")
	}
}
