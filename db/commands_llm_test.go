package db

import (
	"fmt"
	"testing"
	"time"
)

// commands.go：工具执行记录（tool_use+tool_result 配对）与 LLM 调用台账。
// 过滤参数拼接（expID/q 可选）与分页默认值是 UI 查询的公共路径。

func cmdFixture(t *testing.T, d *DB) (*ExplorationStore, *Task) {
	t.Helper()
	task, err := d.CreateTask("commands probe", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// DeleteTask 会级联清 exploration 子图；活动行再兜一层，防止共享测试库留下
		// 全局 /api/commands 可见的残留（server 的 commands 用例断言全库为空）。
		_, _ = d.Exec(`DELETE FROM activity WHERE exploration_id=$1`, task.ExplorationID)
		_ = d.DeleteTask(task.ID)
	})
	return d.Exploration(task.ExplorationID), task
}

func appendCmd(t *testing.T, es *ExplorationStore, worker, tool, input, output string, isErr bool) (int64, int64) {
	t.Helper()
	// tool_use_id 全表 JOIN 用，必须每次唯一，否则会跨探索配对残留行。
	tuID := fmt.Sprintf("%s-%s-%d", worker, tool, time.Now().UnixNano())
	useID, err := es.AppendActivity(Activity{Worker: worker, Kind: "tool_use", Tool: tool, ToolUseID: tuID, Detail: input})
	if err != nil {
		t.Fatal(err)
	}
	resID, err := es.AppendActivity(Activity{Worker: worker, Kind: "tool_result", Tool: tool, ToolUseID: tuID, Detail: output, IsError: isErr})
	if err != nil {
		t.Fatal(err)
	}
	return useID, resID
}

func TestListCommandsAndToolStats(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })
	es, task := cmdFixture(t, d)

	appendCmd(t, es, "main", "bash", "id -u", "uid=0", false)
	appendCmd(t, es, "main", "bash", "rm -rf /", "denied", true)
	appendCmd(t, es, "w1", "http_request", "GET /admin", "200 OK", false)

	expID := task.ExplorationID

	// expID 过滤（测试库共享，必须用 API 的 expID 条件隔离本用例数据）
	recs, total, err := d.ListCommands(&expID, "", 0, 0) // size<=0 → 默认 50
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(recs) != 3 {
		t.Fatalf("total=%d len=%d want 3/3", total, len(recs))
	}
	if recs[0].ID < recs[len(recs)-1].ID {
		t.Fatalf("records must be newest-first")
	}
	var bashRecs []*CommandRecord
	for i := range recs {
		if recs[i].Tool == "bash" {
			bashRecs = append(bashRecs, &recs[i])
		}
	}
	if len(bashRecs) != 2 {
		t.Fatalf("expected 2 bash records, got %d", len(bashRecs))
	}
	bad := bashRecs[0]
	if bad.Command != "rm -rf /" || bad.Output != "denied" || !bad.IsError || bad.Worker != "main" {
		t.Fatalf("paired record broken: %+v", bad)
	}

	// q 过滤（匹配工具名）
	only, total2, err := d.ListCommands(&expID, "http", 0, 50)
	if err != nil || total2 != 1 || len(only) != 1 || only[0].Tool != "http_request" {
		t.Fatalf("filtered list: %v %d %v", only, total2, err)
	}

	// 分页：page=1 只拿第二条
	page1, _, err := d.ListCommands(nil, "bash", 1, 1)
	if err != nil || len(page1) != 1 {
		t.Fatalf("pagination broken: %v %v", page1, err)
	}

	// 统计口径与过滤一致：bash 2 次 1 错
	stats, err := d.ToolStats(&expID, "")
	if err != nil {
		t.Fatal(err)
	}
	var bash *ToolStat
	for i := range stats {
		if stats[i].Tool == "bash" {
			bash = &stats[i]
		}
	}
	if bash == nil || bash.Total != 2 || bash.Errors != 1 {
		t.Fatalf("ToolStats broken: %+v", bash)
	}
	var http *ToolStat
	for i := range stats {
		if stats[i].Tool == "http_request" {
			http = &stats[i]
		}
	}
	if http == nil || http.Total != 1 || http.Errors != 0 {
		t.Fatalf("ToolStats http broken: %+v", http)
	}
}

func TestLLMRecordsLifecycle(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	taskKey := "test-llm-task-42"
	rec := &LLMRecord{
		Model: "test-model-x", SessionID: "sess-abc", TaskID: taskKey, Worker: "planner",
		LatencyMs: 1200, InputTokens: 100, OutputTokens: 50, CacheRead: 10, CacheWrite: 20,
		Status: "ok", ResponseBody: `{"x":1}`, RawRequest: "RAW-REQ", RawResponse: "RAW-RESP",
	}
	if err := d.InsertLLMRecord(rec); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := d.QueryRow(`SELECT id FROM llm_records WHERE task_id=$1 AND model='test-model-x' ORDER BY id DESC LIMIT 1`, taskKey).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// 列表 + 过滤
	list, total, err := d.ListLLMRecords("test-model-x", "sess-abc", taskKey, 0, 0)
	if err != nil || total < 1 || len(list) < 1 {
		t.Fatalf("list: %v %d %v", list, total, err)
	}
	if list[0].ID != id || list[0].Worker != "planner" || list[0].InputTokens != 100 || list[0].CacheRead != 10 {
		t.Fatalf("record fields broken: %+v", list[0])
	}
	if none, _, _ := d.ListLLMRecords("no-such-model", "", "", 0, 50); len(none) != 0 {
		t.Fatalf("model filter broken")
	}
	// 越界页 → 空
	if far, _, _ := d.ListLLMRecords("", "", taskKey, 99, 10); len(far) != 0 {
		t.Fatalf("far page must be empty")
	}

	// 详情（含 raw bodies）
	got, err := d.GetLLMRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RawRequest != "RAW-REQ" || got.RawResponse != "RAW-RESP" || got.ResponseBody != `{"x":1}` {
		t.Fatalf("GetLLMRecord bodies broken: %+v", got)
	}
	if _, err := d.GetLLMRecord(-1); err == nil {
		t.Fatalf("missing record must error")
	}

	// 任务选择器
	tasks, err := d.LLMTasks()
	found := false
	for _, lt := range tasks {
		if lt.TaskID == taskKey && lt.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("LLMTasks missing %s", taskKey)
	}

	// 按 task 删除（含空 task 不误删分支）
	n, err := d.DeleteLLMRecords(taskKey)
	if err != nil || n < 1 {
		t.Fatalf("DeleteLLMRecords: %d %v", n, err)
	}
	if after, _, _ := d.ListLLMRecords("", "", taskKey, 0, 50); len(after) != 0 {
		t.Fatalf("records survived delete")
	}
}
