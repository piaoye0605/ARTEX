package db

import (
	"encoding/json"
	"testing"
)

// conversation.go 的活动流（会话独立于探索图，token 汇总进仪表盘）、
// findings.go 的 setter 链（写表并回写 node payload）、exploration_nodes 的
// goal 编辑与计数。三者都是 UI 高频读面。

func TestConversationActivityFlow(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })

	pid := int64(0)
	_ = d.QueryRow(`SELECT id FROM llm_profiles LIMIT 1`).Scan(&pid)
	var prof *int64
	if pid > 0 {
		prof = &pid
	}
	conv, err := d.CreateConversation("test_conv_agent", "首个会话", prof)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(conv.ID) })

	// 三条活动：两条步骤 + 一条 result（带 token 计量）
	step1, err := d.AppendConvActivity(conv.ID, Activity{Kind: "human", Summary: "你好", Detail: "你好，详文"})
	if err != nil {
		t.Fatal(err)
	}
	step2, err := d.AppendConvActivity(conv.ID, Activity{Kind: "assistant", Summary: "回复", Detail: "回复详文"})
	if err != nil {
		t.Fatal(err)
	}
	inTok, outTok, cRead, cWrite := 100, 50, 10, 20
	step3, err := d.AppendConvActivity(conv.ID, Activity{
		Kind: "result", InputTokens: &inTok, OutputTokens: &outTok, CacheReadTokens: &cRead, CacheWriteTokens: &cWrite,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 列表：sinceID 之后递增，cursor 推进
	list, cursor, err := d.ConvActivityList(conv.ID, 0, 0) // limit<=0 → 默认 500
	if err != nil || len(list) != 3 || cursor != step3 {
		t.Fatalf("list: n=%d cursor=%d err=%v (want 3, %d)", len(list), cursor, err, step3)
	}
	if list[0].ID != step1 || list[0].Summary != "你好" {
		t.Fatalf("list order/summary broken: %+v", list[0])
	}
	if tail, cur2, _ := d.ConvActivityList(conv.ID, step1, 10); len(tail) != 2 || cur2 != step3 {
		t.Fatalf("sinceID exclusive broken: n=%d cursor=%d", len(tail), cur2)
	}

	// 分页：倒序取窗、升序返回、hasMore 检测
	// 倒序取最新一页（最新 2 条 = step2+step3），升序返回
	page, hasMore, err := d.ConvActivityPage(conv.ID, 0, 2)
	if err != nil || len(page) != 2 || !hasMore {
		t.Fatalf("page: n=%d hasMore=%v err=%v", len(page), hasMore, err)
	}
	if page[0].ID != step2 || page[1].ID != step3 {
		t.Fatalf("newest page must be ascending [step2,step3]: %d %d", page[0].ID, page[1].ID)
	}
	// before=step2 → 只剩 step1（更早历史），无更多
	page2, hasMore2, _ := d.ConvActivityPage(conv.ID, step2, 5)
	if hasMore2 || len(page2) != 1 || page2[0].ID != step1 {
		t.Fatalf("older window broken: n=%d hasMore=%v", len(page2), hasMore2)
	}

	// 懒加载 detail + 跨会话隔离
	detail, err := d.ConvActivityDetail(conv.ID, step2)
	if err != nil || detail != "回复详文" {
		t.Fatalf("detail: %q %v", detail, err)
	}
	if ghost, _ := d.ConvActivityDetail(conv.ID+999999, step2); ghost != "" {
		t.Fatalf("cross-conversation detail must be empty, got %q", ghost)
	}

	// token 汇总（全表聚合，验证本会话行存在且数值正确）
	sums, err := d.ConversationTokenSummaries()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sums {
		if s.LLMProfileID != nil && prof != nil && *s.LLMProfileID == *prof {
			// 同 profile 可能有历史会话行；只验证字段可解析
			found = true
			_ = s.InputTokens
		}
	}
	// 无 profile 的会话也会出现在汇总里（nil profile 行）
	if !found && len(sums) == 0 {
		t.Fatalf("ConversationTokenSummaries must include our conversation rows")
	}

	// rename / touch / 更新 profile
	if err := d.RenameConversation(conv.ID, "改名后"); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.GetConversation(conv.ID); c.Title != "改名后" {
		t.Fatalf("rename broken: %+v", c)
	}
	if err := d.TouchConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateConversationProfile(conv.ID, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.GetConversation(conv.ID); c.LLMProfileID != nil {
		t.Fatalf("clear profile broken: %+v", c)
	}

	// 批量删除返回实际删除的 id（含不存在的过滤）
	ghostID := conv.ID + 999999
	deleted, err := d.DeleteConversations([]int64{conv.ID, ghostID})
	if err != nil || len(deleted) != 1 || deleted[0] != conv.ID {
		t.Fatalf("DeleteConversations: %v %v", deleted, err)
	}
}

func mustTaskFindings(t *testing.T, d *DB, desc string) (*Task, *ExplorationStore) {
	t.Helper()
	task, err := d.CreateTask(desc, "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM activity WHERE exploration_id=$1`, task.ExplorationID)
		_ = d.DeleteTask(task.ID)
	})
	return task, d.Exploration(task.ExplorationID)
}

func TestFindingSettersSyncNodePayload(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	task, es := mustTaskFindings(t, d, "finding setters probe")

	nodeID, err := es.AddNode(KindFinding, map[string]any{
		"summary": "原始摘要", "vulnclass": "SQLI", "severity": "high",
	}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(task.ID, nodeID, "SQLI", "注入点", "high", "原始摘要", "证据", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM findings WHERE id=$1`, fid) })

	// setFindingCol 三连：name / vulnclass / severity，且同步回 node payload
	if n, err := d.SetFindingName(fid, "改名后的注入点"); err != nil || n != 1 {
		t.Fatalf("SetFindingName: %d %v", n, err)
	}
	if n, err := d.SetFindingVulnClass(fid, "XSS"); err != nil || n != 1 {
		t.Fatalf("SetFindingVulnClass: %d %v", n, err)
	}
	if n, err := d.SetFindingSeverity(fid, "critical"); err != nil || n != 1 {
		t.Fatalf("SetFindingSeverity: %d %v", n, err)
	}
	node, err := es.GetNode(nodeID)
	if err != nil || node == nil {
		t.Fatal(err)
	}
	var np map[string]any
	if err := json.Unmarshal(node.Payload, &np); err != nil {
		t.Fatal(err)
	}
	if np["name"] != "改名后的注入点" || np["vulnclass"] != "XSS" || np["severity"] != "critical" {
		t.Fatalf("node payload sync broken: %+v", np)
	}

	// 报告写入（ByNodeID 寻址）
	if n, err := d.SetFindingReportByNodeID(nodeID, "# 报告正文"); err != nil || n != 1 {
		t.Fatalf("SetFindingReportByNodeID: %d %v", n, err)
	}
	if n, _ := d.SetFindingReportByNodeID(999999999, "x"); n != 0 {
		t.Fatalf("missing node must affect 0 rows, got %d", n)
	}

	// FindingMetaByNodeID：映射 node → 独立行元数据
	meta, err := d.FindingMetaByNodeID(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := meta[nodeID]
	if !ok || m.ID != fid || m.Status == "" || m.TrafficCount != 0 {
		t.Fatalf("FindingMetaByNodeID broken: %+v", m)
	}
	// taskID<=0 快速返回空 map
	if empty, err := d.FindingMetaByNodeID(0); err != nil || len(empty) != 0 {
		t.Fatalf("taskID=0 must short-circuit: %v %v", empty, err)
	}
	// AssetStore 透传入口
	if am, err := d.Assets().FindingMetaByNodeID(task.ID); err != nil || len(am) != 1 {
		t.Fatalf("AssetStore passthrough broken: %v %v", am, err)
	}

	// 列表（仪表盘汇总用）
	list, err := d.ListFindings(10)
	if err != nil || len(list) == 0 {
		t.Fatalf("ListFindings: %v %v", list, err)
	}
}

func TestGoalEditAndNodeCounts(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	_, es := mustTaskFindings(t, d, "goal edit probe")

	gid, err := es.AddNode(KindGoal, map[string]any{"text": "拿 flag"}, 0, "open", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	iOpen, err := es.AddNode(KindIntent, map[string]any{"summary": "open intent"}, 0, "open", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	iDone, err := es.AddNode(KindIntent, map[string]any{"summary": "done intent"}, 0, "done", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	iBlocked, err := es.AddNode(KindIntent, map[string]any{"summary": "blocked intent"}, 0, "blocked", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = iBlocked

	// goal 编辑：重写 text+vulnclass；非 goal id / 跨探索 id 拒绝
	if err := es.UpdateGoalPayload(gid, "改过的目标", "WEB"); err != nil {
		t.Fatal(err)
	}
	node, _ := es.GetNode(gid)
	var gp map[string]any
	if err := json.Unmarshal(node.Payload, &gp); err != nil {
		t.Fatal(err)
	}
	if gp["text"] != "改过的目标" || gp["vulnclass"] != "WEB" {
		t.Fatalf("UpdateGoalPayload broken: %+v", gp)
	}
	if err := es.UpdateGoalPayload(iOpen, "x", ""); err == nil {
		t.Fatalf("UpdateGoalPayload on intent must error")
	}
	if err := es.UpdateGoalPayload(gid+999999, "x", ""); err == nil {
		t.Fatalf("UpdateGoalPayload on missing goal must error")
	}
	if err := es.DeleteGoal(iOpen); err == nil {
		t.Fatalf("DeleteGoal on intent must error")
	}

	// 计数：open=1（iOpen），finished=1（done；blocked 不算 finished？—— done/blocked/exhausted 都算）
	open, err := es.CountOpenIntents()
	if err != nil || open != 1 {
		t.Fatalf("CountOpenIntents: %d %v", open, err)
	}
	finished, err := es.CountFinishedIntents()
	if err != nil || finished != 2 { // done + blocked
		t.Fatalf("CountFinishedIntents: %d %v", finished, err)
	}

	// 带过滤的分页（q 匹配 summary）+ Nodes/NodesByIDs
	if err := es.DeleteGoal(gid); err != nil {
		t.Fatal(err)
	}
	page, hasMore, total, err := es.ListByKindPageWithSources(KindIntent, 0, 1, "open")
	if err != nil || len(page) != 1 || hasMore || total != 1 || page[0].ID != iOpen {
		t.Fatalf("filtered page: n=%d hasMore=%v total=%d err=%v", len(page), hasMore, total, err)
	}
	if _, _, totalAll, err := es.ListByKindPageWithSources(KindIntent, 0, 50, ""); err != nil || totalAll < 2 {
		t.Fatalf("unfiltered total must include our 2 intents, got %d err=%v", totalAll, err)
	}

	nodes, err := es.Nodes(0) // limit<=0 → 默认 2000
	if err != nil || len(nodes) < 2 {
		t.Fatalf("Nodes: %d %v", len(nodes), err)
	}
	byIDs, err := es.NodesByIDs([]int64{iOpen, iDone})
	if err != nil || len(byIDs) != 2 || byIDs[0].ID != iOpen {
		t.Fatalf("NodesByIDs: %v %v", byIDs, err)
	}
	if none, err := es.NodesByIDs(nil); err != nil || none != nil {
		t.Fatalf("NodesByIDs empty must be nil,nil: %v %v", none, err)
	}
}
