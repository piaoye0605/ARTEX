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

// This file pins the contracts of the findings read/write control plane
// (server_findings.go): the dual-mode list (bare array vs paged envelope), the
// stats card aggregates, per-finding read/patch/delete and the lineage sub-DAG.
//
// Every fixture is uniquely marked (vulnclass carries a unixnano suffix) because
// the findings table is shared: the global list/stat endpoints aggregate the
// whole table, so assertions must scope themselves to the marker.

type findingFixture struct {
	task    *Task
	taskID  int64
	marker  string
	finding int64
	node    int64
	asset   int64
}

// newFindingFixture seeds one task with one finding node + one anchored IP asset.
// withNode=false seeds a finding row with no originating node/task (the shape
// left behind after a task is deleted).
func newFindingFixture(t *testing.T, m *Manager, tag string, withNode bool) findingFixture {
	t.Helper()
	fx := findingFixture{marker: fmt.Sprintf("__finding_probe_%s_%d__", tag, time.Now().UnixNano())}

	var nodeID int64
	if withNode {
		task, err := m.CreateTask("finding fixture "+tag, "goal", nil, 0, 0)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		fx.task = task
		fx.taskID = mustTaskID(t, task.ID)
		t.Cleanup(func() {
			_, _ = m.pg.Exec(`DELETE FROM findings WHERE vulnclass=$1`, fx.marker)
			_, _ = m.pg.Exec(`UPDATE tasks SET deleted_at=NULL WHERE id=$1`, fx.taskID)
			_, _ = m.DeleteTask(task.ID, DeleteTaskOptions{})
		})
		root, err := task.Store.AddNode(db.KindGoal, map[string]any{"text": "root"}, 0, "open", "human", nil)
		if err != nil {
			t.Fatalf("add goal: %v", err)
		}
		nodeID, err = task.Store.AddNode(db.KindFinding, map[string]any{
			"summary":  "sql injection in login",
			"severity": db.SeverityHigh,
			"vulnclass": fx.marker,
		}, 5, "confirmed", "worker", nil)
		if err != nil {
			t.Fatalf("add finding node: %v", err)
		}
		if err := task.Store.Link(root, "yields", nodeID); err != nil {
			t.Fatalf("link: %v", err)
		}
		fx.node = nodeID
		fx.asset, err = m.pg.Assets().UpsertIP(db.UpsertIPReq{IP: "10.99.7.21", TaskID: fx.taskID})
		if err != nil {
			t.Fatalf("seed asset: %v", err)
		}
	} else {
		t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM findings WHERE vulnclass=$1`, fx.marker) })
	}

	assets := []int64(nil)
	if fx.asset > 0 {
		assets = []int64{fx.asset}
	}
	id, err := m.pg.AddFinding(fx.taskID, nodeID, fx.marker, "登录处 SQL 注入", db.SeverityHigh,
		"summary text", "evidence text", "worker", assets)
	if err != nil {
		t.Fatalf("add finding: %v", err)
	}
	fx.finding = id
	return fx
}

func TestFindingsListRoutes(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "list", true)
	base := "/api/exploration/findings"

	// No page/limit → bare array (dashboard/legacy clients). The envelope would
	// break those callers, so this shape is a contract, not an accident.
	rec := crudDo(t, s, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bare list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var bare []struct {
		ID        string `json:"id"`
		FindingID string `json:"finding_id"`
		VulnClass string `json:"vulnclass"`
		Severity  string `json:"severity"`
	}
	crudJSON(t, rec, &bare)
	if len(bare) == 0 {
		t.Fatal("bare list returned no rows")
	}
	for _, f := range bare {
		if f.FindingID == "" {
			t.Fatalf("bare list row without finding_id: %+v", f)
		}
	}

	// page/limit switches to the envelope; filters push down to SQL.
	rec = crudDo(t, s, http.MethodGet, base+"?vulnclass="+fx.marker+"&page=1&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("paged: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var paged struct {
		Items    []map[string]any `json:"items"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
	}
	crudJSON(t, rec, &paged)
	if paged.Total != 1 || len(paged.Items) != 1 || paged.Page != 1 || paged.PageSize != 10 {
		t.Fatalf("paged=%+v", paged)
	}
	if paged.Items[0]["vulnclass"] != fx.marker {
		t.Fatalf("filter leaked: %+v", paged.Items[0])
	}

	// "all" is the frontend sentinel for "no filter" and must not be sent to SQL.
	crudJSON(t, crudDo(t, s, http.MethodGet, base+"?vulnclass="+fx.marker+"&severity=all&status=all&limit=10", ""), &paged)
	if paged.Total != 1 {
		t.Fatalf("severity=all/status=all must be a no-op filter: total=%d", paged.Total)
	}

	// Pagination params are clamped: page<1 → 1, limit>200 → 200, limit<1 → default 20.
	crudJSON(t, crudDo(t, s, http.MethodGet, base+"?page=-4&limit=500", ""), &paged)
	if paged.Page != 1 || paged.PageSize != 200 {
		t.Fatalf("clamp page=%d size=%d", paged.Page, paged.PageSize)
	}
	crudJSON(t, crudDo(t, s, http.MethodGet, base+"?page=1&limit=0", ""), &paged)
	if paged.PageSize != 20 {
		t.Fatalf("limit=0 must fall back to 20, got %d", paged.PageSize)
	}

	// task= switches to the per-task exploration-node view and resolves assets.
	rec = crudDo(t, s, http.MethodGet, base+"?task="+fx.task.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("task view: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var taskView []struct {
		ID        string `json:"id"`
		FindingID string `json:"finding_id"`
		TaskID    string `json:"task_id"`
		Severity  string `json:"severity"`
		Assets    []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Label string `json:"label"`
		} `json:"assets"`
	}
	crudJSON(t, rec, &taskView)
	if len(taskView) != 1 {
		t.Fatalf("task view rows=%d body=%s", len(taskView), rec.Body.String())
	}
	if taskView[0].TaskID != fx.task.ID || taskView[0].FindingID != i64s(fx.finding) {
		t.Fatalf("task view row=%+v", taskView[0])
	}
	if len(taskView[0].Assets) != 1 || taskView[0].Assets[0].Type != "ip" || taskView[0].Assets[0].Label == "" {
		t.Fatalf("asset anchoring lost: %+v", taskView[0].Assets)
	}

	// Unknown task id degrades to an empty array, not a 404 — the overview tab
	// polls this while a task is being archived.
	rec = crudDo(t, s, http.MethodGet, base+"?task=999999999", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("unknown task: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFindingStatsRoute(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "stats", true)

	rec := crudDo(t, s, http.MethodGet, "/api/exploration/findings/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var st struct {
		Total       int      `json:"total"`
		Pending     int      `json:"pending"`
		High        int      `json:"high"`
		VulnClasses []string `json:"vulnclasses"`
		Tasks       []struct {
			ID int64 `json:"id"`
		} `json:"tasks"`
	}
	crudJSON(t, rec, &st)
	if st.Total < 1 || st.Pending < 1 || st.High < 1 {
		t.Fatalf("stats aggregates missing the seeded finding: %+v", st)
	}
	hasMarker := false
	for _, v := range st.VulnClasses {
		if v == fx.marker {
			hasMarker = true
		}
	}
	if !hasMarker {
		t.Fatalf("vulnclass filter options missing %q: %+v", fx.marker, st.VulnClasses)
	}
	if st.VulnClasses == nil || st.Tasks == nil {
		t.Fatal("stats slices must be [] not null")
	}
}

func TestGetFindingRoute(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "get", true)
	base := "/api/exploration/findings/"

	for _, bad := range []string{"abc", "0", "-3"} {
		if rec := crudDo(t, s, http.MethodGet, base+bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("id %q: status=%d", bad, rec.Code)
		}
	}
	if rec := crudDo(t, s, http.MethodGet, base+"999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status=%d", rec.Code)
	}

	var dto struct {
		ID        string `json:"id"`
		FindingID string `json:"finding_id"`
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Status    string `json:"status"`
		Assets    []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"assets"`
		SourceTaskID string `json:"source_task_id"`
		Inherited    bool   `json:"inherited"`
	}
	rec := crudDo(t, s, http.MethodGet, base+i64s(fx.finding), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &dto)
	if dto.FindingID != i64s(fx.finding) || dto.Name != "登录处 SQL 注入" || dto.Severity != db.SeverityHigh {
		t.Fatalf("dto=%+v", dto)
	}
	if dto.Status != db.FindingPending {
		t.Fatalf("new finding status=%q", dto.Status)
	}
	if len(dto.Assets) != 1 || dto.Assets[0].Type != "ip" {
		t.Fatalf("assets=%+v", dto.Assets)
	}

	// context_task annotates provenance: Nonexistent context → 404; a task that
	// is neither this finding's owner nor one of its sources → 404 (the finding
	// must not be reachable through an unrelated task).
	if rec := crudDo(t, s, http.MethodGet, base+i64s(fx.finding)+"?context_task=999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown context task: status=%d body=%s", rec.Code, rec.Body.String())
	}
	other, err := m.CreateTask("unrelated context", "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}
	otherID := mustTaskID(t, other.ID)
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`UPDATE tasks SET deleted_at=NULL WHERE id=$1`, otherID)
		_, _ = m.DeleteTask(other.ID, DeleteTaskOptions{})
	})
	if rec := crudDo(t, s, http.MethodGet, base+i64s(fx.finding)+"?context_task="+other.ID, ""); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("foreign context task: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Own task → allowed, no provenance fields.
	crudJSON(t, crudDo(t, s, http.MethodGet, base+i64s(fx.finding)+"?context_task="+fx.task.ID, ""), &dto)
	if dto.SourceTaskID != "" || dto.Inherited {
		t.Fatalf("own-task context must not look inherited: %+v", dto)
	}
}

func TestFindingLineageRoute(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "lineage", true)
	base := "/api/exploration/findings/"

	if rec := crudDo(t, s, http.MethodGet, base+"abc/lineage", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, base+"999999999/lineage", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status=%d", rec.Code)
	}

	var lineage struct {
		Nodes []struct {
			ID   string `json:"id"`
			Type string `json:"type"` // db Node.Kind travels as "type" in node DTOs
		} `json:"nodes"`
		Edges []struct {
			Src string `json:"src"`
			Rel string `json:"rel"`
			Dst string `json:"dst"`
		} `json:"edges"`
	}
	rec := crudDo(t, s, http.MethodGet, base+i64s(fx.finding)+"/lineage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("lineage: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &lineage)
	if len(lineage.Nodes) < 2 || len(lineage.Edges) < 1 {
		t.Fatalf("lineage should contain the finding + its ancestor: %s", rec.Body.String())
	}
	sawFinding := false
	for _, n := range lineage.Nodes {
		if n.Type == db.KindFinding {
			sawFinding = true
		}
	}
	if !sawFinding {
		t.Fatalf("lineage missing the finding node: %+v", lineage.Nodes)
	}

	// A finding with no originating node (task long gone) returns empty arrays,
	// not an error — the detail page renders "no lineage" instead of failing.
	orphan := newFindingFixture(t, m, "orphan", false)
	rec = crudDo(t, s, http.MethodGet, base+i64s(orphan.finding)+"/lineage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("orphan lineage: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Nodes []any `json:"nodes"`
		Edges []any `json:"edges"`
	}
	crudJSON(t, rec, &empty)
	if len(empty.Nodes) != 0 || len(empty.Edges) != 0 || empty.Nodes == nil || empty.Edges == nil {
		t.Fatalf("orphan lineage=%s", rec.Body.String())
	}
}

func TestPatchFindingRoutes(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "patch", true)
	base := "/api/exploration/findings/"

	if rec := crudDo(t, s, http.MethodPatch, base+i64s(fx.finding), `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, base+i64s(fx.finding), `{}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "nothing to update") {
		t.Fatalf("empty patch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPatch, base+i64s(fx.finding), `{"status":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPatch, base+i64s(fx.finding), `{"severity":"urgent"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad severity: status=%d", rec.Code)
	}
	// Unknown ids are 404 from every field's own guard.
	for _, body := range []string{`{"status":"fixed"}`, `{"severity":"low"}`, `{"name":"x"}`, `{"vulnclass":"x"}`} {
		if rec := crudDo(t, s, http.MethodPatch, base+"999999999", body); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown id %s: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}

	// A combined patch applies every provided field and returns the fresh DTO;
	// name/vulnclass are trimmed before storage.
	rec := crudDo(t, s, http.MethodPatch, base+i64s(fx.finding),
		`{"status":"fixed","severity":"low","name":"  改名注入  ","vulnclass":"`+fx.marker+`_renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		FindingID string `json:"finding_id"`
		Status    string `json:"status"`
		Severity  string `json:"severity"`
		Name      string `json:"name"`
		VulnClass string `json:"vulnclass"`
	}
	crudJSON(t, rec, &dto)
	if dto.Status != db.FindingFixed || dto.Severity != db.SeverityLow ||
		dto.Name != "改名注入" || dto.VulnClass != fx.marker+"_renamed" {
		t.Fatalf("patched dto=%+v", dto)
	}

	// Severity edits are mirrored onto the originating exploration node so the
	// per-task view stays in sync with the standalone findings table.
	nodes, err := fx.task.Store.ListByKind(db.KindFinding, 10)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("finding nodes=%d", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("decode node payload: %v", err)
	}
	if sev, _ := payload["severity"].(string); sev != db.SeverityLow {
		t.Fatalf("node payload severity=%v want %q", payload["severity"], db.SeverityLow)
	}
}

func TestDeleteFindingRoute(t *testing.T) {
	m, s := newTestStack(t)
	fx := newFindingFixture(t, m, "delete", true)
	base := "/api/exploration/findings/"

	if rec := crudDo(t, s, http.MethodDelete, base+"abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, base+"999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status=%d", rec.Code)
	}

	rec := crudDo(t, s, http.MethodDelete, base+i64s(fx.finding), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deleted bool  `json:"deleted"`
		ID      int64 `json:"id"`
	}
	crudJSON(t, rec, &resp)
	if !resp.Deleted || resp.ID != fx.finding {
		t.Fatalf("delete response=%+v", resp)
	}
	if f, err := m.pg.GetFinding(fx.finding); err != nil || f != nil {
		t.Fatalf("finding survived delete: %+v %v", f, err)
	}
	if rec := crudDo(t, s, http.MethodDelete, base+i64s(fx.finding), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete: status=%d", rec.Code)
	}
}
