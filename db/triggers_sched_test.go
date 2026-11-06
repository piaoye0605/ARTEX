package db

import (
	"testing"
	"time"
)

// triggers.go 是调度器的持久层：触发器 CRUD、水位表、以及四类跨探索事件查询
// （新发现 / 超时任务 / 新任务 / 新工具调用）。水位单调递增、不重复触发是核心契约。

func mustTask(t *testing.T, d *DB, desc string) *Task {
	t.Helper()
	task, err := d.CreateTask(desc, "goal of "+desc, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM activity WHERE exploration_id=$1`, task.ExplorationID)
		_ = d.DeleteTask(task.ID)
	})
	return task
}

func TestTriggerCRUDAndScan(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	in := &AgentTrigger{
		AgentKey: "test-trig-agent", Enabled: true, IntervalSec: 300,
		OnFinding: true, OnGoalMet: true,
		IntervalMessage: "周期消息", FindingMessage: "发现消息",
		ToolNames: []string{"bash", "http_request"},
	}
	created, err := d.CreateTrigger(in)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTrigger(created.ID) }()
	if created.ID <= 0 || created.ToolNames == nil || len(created.ToolNames) != 2 || created.ToolNames[0] != "bash" {
		t.Fatalf("create round-trip broken: %+v", created)
	}

	// 更新全部字段（含 tool_names 清空为 nil → 列存 ""）
	created.Enabled = false
	created.OnTaskTimeout = true
	created.TaskTimeoutMessage = "超时消息"
	created.ToolNames = nil
	created.IntervalMessage = "改过的周期消息"
	if err := d.UpdateTrigger(created); err != nil {
		t.Fatal(err)
	}
	got, err := d.ListTriggersFor("test-trig-agent")
	if err != nil || len(got) != 1 {
		t.Fatalf("list after update: %v %v", got, err)
	}
	u := got[0]
	if u.Enabled || !u.OnTaskTimeout || u.TaskTimeoutMessage != "超时消息" || u.ToolNames == nil || len(u.ToolNames) != 0 {
		t.Fatalf("update round-trip broken: %+v", u)
	}

	// last_fire 触碰 + null-time 扫描路径
	if err := d.TouchTriggerFire(created.ID); err != nil {
		t.Fatal(err)
	}
	got2, _ := d.ListTriggersFor("test-trig-agent")
	if got2[0].LastFire == nil || time.Since(*got2[0].LastFire) > time.Minute {
		t.Fatalf("TouchTriggerFire must stamp now(), got %v", got2[0].LastFire)
	}

	// 启用过滤：enabled=false 不得出现在 ListEnabledTriggers
	enabledSet, err := d.ListEnabledTriggers()
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range enabledSet {
		if tr.ID == created.ID {
			t.Fatalf("disabled trigger must not appear in enabled set")
		}
	}

	if err := d.DeleteTrigger(created.ID); err != nil {
		t.Fatal(err)
	}
	if got3, _ := d.ListTriggersFor("test-trig-agent"); len(got3) != 0 {
		t.Fatalf("delete failed, still %d rows", len(got3))
	}

	// agent 级联清理
	c1, _ := d.CreateTrigger(&AgentTrigger{AgentKey: "test-trig-agent-b"})
	c2, _ := d.CreateTrigger(&AgentTrigger{AgentKey: "test-trig-agent-b"})
	if err := d.DeleteTriggersForAgent("test-trig-agent-b"); err != nil {
		t.Fatal(err)
	}
	if got4, _ := d.ListTriggersFor("test-trig-agent-b"); len(got4) != 0 {
		_ = c1
		_ = c2
		t.Fatalf("DeleteTriggersForAgent left rows behind")
	}
}

func TestSchedulerStateKV(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	key := "test.sched.watermark"
	v, err := d.GetSchedState(key)
	if err != nil || v != "" {
		t.Fatalf("missing key must return empty,nil, got %q,%v", v, err)
	}
	if err := d.SetSchedState(key, "42"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSchedState(key, "99"); err != nil { // upsert 覆盖
		t.Fatal(err)
	}
	if v, err = d.GetSchedState(key); err != nil || v != "99" {
		t.Fatalf("upsert broken: %q,%v", v, err)
	}
	if _, err := d.Exec(`DELETE FROM scheduler_state WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerEventQueries(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	task := mustTask(t, d, "sched events probe")
	exp := d.Exploration(task.ExplorationID)

	// finding 事件：payload 里的 summary/vulnclass/severity 要展开
	fid, err := exp.AddNode(KindFinding, map[string]any{
		"summary": "SQL 注入点", "vulnclass": "SQLI", "severity": "high",
	}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := d.NewFindingsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var found *TaskEvent
	for i := range findings {
		if findings[i].NodeID == fid {
			found = &findings[i]
		}
	}
	if found == nil || found.Summary != "SQL 注入点" || found.VulnClass != "SQLI" || found.Severity != "high" ||
		found.TaskID != task.ID || found.TaskDesc != "sched events probe" {
		t.Fatalf("NewFindingsSince broken: %+v", found)
	}

	// goal met 事件：payload text 优先于 summary
	gid, err := exp.AddNode(KindGoal, map[string]any{"text": "拿到 flag"}, 0, "met", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	goals, err := d.MetGoals()
	var g *TaskEvent
	for i := range goals {
		if goals[i].NodeID == gid {
			g = &goals[i]
		}
	}
	if g == nil || g.Summary != "拿到 flag" || g.TaskID != task.ID {
		t.Fatalf("MetGoals broken: %+v", g)
	}

	// 工具调用事件：tool_result 行 + 配对 tool_use 行（detail=入参）
	useID, err := exp.AppendActivity(Activity{Kind: "tool_use", Tool: "bash", ToolUseID: "tu-1", Detail: `{"command":"id"}`})
	if err != nil {
		t.Fatal(err)
	}
	_ = useID
	resID, err := exp.AppendActivity(Activity{Kind: "tool_result", Tool: "bash", ToolUseID: "tu-1", Detail: "uid=0", IsError: false})
	if err != nil {
		t.Fatal(err)
	}
	calls, err := d.NewToolCallsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var call *TaskEvent
	for i := range calls {
		if calls[i].NodeID == resID {
			call = &calls[i]
		}
	}
	if call == nil || call.Tool != "bash" || call.ToolInput != `{"command":"id"}` || call.ToolOutput != "uid=0" || call.ToolIsErr {
		t.Fatalf("NewToolCallsSince broken: %+v", call)
	}

	// 新任务事件 + 超时事件
	tasks, err := d.NewTasksSince(task.ID - 1)
	var newTask *TaskEvent
	for i := range tasks {
		if tasks[i].TaskID == task.ID {
			newTask = &tasks[i]
		}
	}
	if newTask == nil || newTask.Summary != "goal of sched events probe" {
		t.Fatalf("NewTasksSince broken: %+v", newTask)
	}
	if _, err := d.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	timed, err := d.TimedOutTasksSince(task.ID - 1)
	if err != nil || len(timed) == 0 || timed[len(timed)-1].TaskID != task.ID {
		t.Fatalf("TimedOutTasksSince broken: %v %v", timed, err)
	}

	// 水位推进后不再重复返回旧事件
	if later, _ := d.NewFindingsSince(fid); len(later) > 0 {
		for _, e := range later {
			if e.NodeID == fid {
				t.Fatalf("watermark must exclude consumed ids")
			}
		}
	}
}
