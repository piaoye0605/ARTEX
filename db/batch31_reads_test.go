package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestExportFindingsAndMentionSearch covers the export query (both the explicit
// id list and the filter path) and the chat-mention search: kind scoping,
// literal-% escaping, cursor validation and keyset pagination.
func TestExportFindingsAndMentionSearch(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tag := fmt.Sprintf("batch31-%d", time.Now().UnixNano())
	// 清掉历史失败运行残留的本批次数据,保证字面量通配符断言确定。
	_, _ = d.Exec(`DELETE FROM findings WHERE name LIKE 'batch31-%'`)
	task, err := d.CreateTaskWithOptions(tag, "export findings", TaskCreateOptions{Name: tag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.DeleteFindingsByTask(task.ID) //nolint:errcheck
		_, _ = d.Exec(`DELETE FROM assets WHERE root_domain=$1`, tag+".example")
		_ = d.DeleteTask(task.ID)
	})
	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: tag + ".example", TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}

	highID, err := d.AddFinding(task.ID, 0, "SQL注入", tag+" 高危 100%_flat", "high", "summary", "evidence", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	lowID, err := d.AddFinding(task.ID, 0, "Info", tag+" 低危", "low", "summary", "evidence", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	byIDs, err := d.ListFindingsForExport(FindingFilter{}, []int64{highID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byIDs) != 1 || byIDs[0].ID != highID || byIDs[0].Report != "" || len(byIDs[0].AssetIDs) != 1 {
		t.Fatalf("export by ids = %+v", byIDs)
	}
	byFilter, err := d.ListFindingsForExport(FindingFilter{TaskID: fmt.Sprint(task.ID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(byFilter) != 2 || byFilter[0].ID != highID || byFilter[1].ID != lowID {
		t.Fatalf("export by filter order = %+v", byFilter)
	}

	if _, err := d.SearchChatMentionsPage(t.Context(), "bogus-kind", tag, ""); err == nil {
		t.Fatal("unsupported mention kind must be rejected")
	}
	page, err := d.SearchChatMentionsPage(t.Context(), "finding", tag, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, item := range page.Items {
		if item.Kind != "finding" || item.Label == "" || item.Description == "" {
			t.Fatalf("mention item = %+v", item)
		}
		seen[item.ID] = true
	}
	if len(page.Items) != 2 || !seen[highID] || !seen[lowID] {
		t.Fatalf("mention search = %+v", page)
	}
	exact, err := d.SearchChatMentionsPage(t.Context(), "finding", fmt.Sprint(lowID), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Items) == 0 || exact.Items[0].ID != lowID {
		t.Fatalf("exact id match should sort first: %+v", exact.Items)
	}
	// `%` 与 `_` 是字面量: 只有含 "100%_" 的名字命中。
	escaped, err := d.SearchChatMentionsPage(t.Context(), "finding", "100%_", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(escaped.Items) != 1 || escaped.Items[0].ID != highID {
		t.Fatalf("literal wildcard search = %+v", escaped.Items)
	}
	byAsset, err := d.SearchChatMentionsPage(t.Context(), "root_domain", tag, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byAsset.Items) != 1 || byAsset.Items[0].ID != assetID {
		t.Fatalf("asset mention search = %+v", byAsset.Items)
	}
	if none, err := d.SearchChatMentionsPage(t.Context(), "company", tag+"-no-company", ""); err != nil || len(none.Items) != 0 || none.NextCursor != "" {
		t.Fatalf("empty mention search = %+v, %v", none, err)
	}

	for _, cursor := range []string{strings.Repeat("a", 2100), "!!!", "eyJpZCI6MH0"} {
		if _, err := d.SearchChatMentionsPage(t.Context(), "finding", tag, cursor); !errors.Is(err, ErrInvalidChatMentionCursor) {
			t.Fatalf("cursor %q = %v", cursor[:12], err)
		}
	}

	// 22 条命中 → 第一页 20 条 + 游标，第二页剩余 2 条且无游标。
	for i := 0; i < 22; i++ {
		if _, err := d.AddFinding(task.ID, 0, "Info", fmt.Sprintf("%s-page-%02d", tag, i), "medium", "s", "e", "w", nil); err != nil {
			t.Fatal(err)
		}
	}
	first, err := d.SearchChatMentionsPage(t.Context(), "finding", tag+"-page", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 20 || first.NextCursor == "" {
		t.Fatalf("first page items=%d cursor=%q", len(first.Items), first.NextCursor)
	}
	second, err := d.SearchChatMentionsPage(t.Context(), "finding", tag+"-page", first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.NextCursor != "" {
		t.Fatalf("second page items=%d cursor=%q", len(second.Items), second.NextCursor)
	}
	if second.Items[0].ID >= first.Items[len(first.Items)-1].ID {
		t.Fatalf("second page must continue after %d, got %d", first.Items[len(first.Items)-1].ID, second.Items[0].ID)
	}
	if _, err := d.SearchChatMentionsPage(t.Context(), "finding", tag+"-pagex", first.NextCursor); !errors.Is(err, ErrInvalidChatMentionCursor) {
		t.Fatalf("cursor bound to another query = %v", err)
	}
}

// TestAgentBindingCountsWithVisibility covers the per-agent MCP/skill/tool
// binding counters used by the access-control overview.
func TestAgentBindingCountsWithVisibility(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	var agentID int64
	if err := d.QueryRow(`SELECT id FROM agents ORDER BY id LIMIT 1`).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	mcp, skill, tools, err := d.AgentBindingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if mcp == nil || skill == nil || tools == nil {
		t.Fatalf("nil maps: %v %v %v", mcp, skill, tools)
	}
	baseMCP, baseSkill := mcp[agentID], skill[agentID]

	resourceID := time.Now().UnixNano() % 1_000_000_000
	skillName := fmt.Sprintf("batch31-skill-%d", resourceID)
	if _, err := d.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,mcp_tool_name,enabled)
VALUES($1,'mcp',$2,'',true) ON CONFLICT DO NOTHING`, agentID, resourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled)
VALUES($1,$2,true) ON CONFLICT DO NOTHING`, agentID, skillName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_id=$2`, agentID, resourceID)
		_, _ = d.Exec(`DELETE FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name=$2`, agentID, skillName)
	})

	mcp, skill, _, err = d.AgentBindingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if mcp[agentID] <= baseMCP || skill[agentID] <= baseSkill {
		t.Fatalf("binding counts did not grow: mcp %d->%d skill %d->%d", baseMCP, mcp[agentID], baseSkill, skill[agentID])
	}
}

// TestFindingAssetTreeCompanyLayer covers the company tier that only appears
// when an asset is actually owned by a company.
func TestFindingAssetTreeCompanyLayer(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tag := fmt.Sprintf("batch31-corp-%d", time.Now().UnixNano())
	task, err := d.CreateTaskWithOptions(tag, "company tree", TaskCreateOptions{Name: tag})
	if err != nil {
		t.Fatal(err)
	}
	companyID, _, err := d.Companies().UpsertCompany(tag+" 企业", "")
	if err != nil {
		t.Fatal(err)
	}
	root := tag + ".corp.example"
	t.Cleanup(func() {
		d.DeleteFindingsByTask(task.ID) //nolint:errcheck
		_, _ = d.Exec(`DELETE FROM assets WHERE root_domain=$1`, root)
		_, _ = d.Exec(`DELETE FROM companies WHERE id=$1`, companyID)
		_ = d.DeleteTask(task.ID)
	})
	rootID := seedTreeAsset(t, d, "root_domain", map[string]any{
		"domain": root, "root_domain": root, "company_id": companyID, "company_source": "explicit",
	})
	if _, err := d.AddFinding(task.ID, 0, "XSS", tag+" 企业漏洞", "high", "s", "e", "w", []int64{rootID}); err != nil {
		t.Fatal(err)
	}

	tree, err := d.BuildFindingAssetTree(FindingFilter{TaskID: fmt.Sprint(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	companyNode := nodeByKey(tree, companyKey(companyID))
	if companyNode == nil || companyNode.Kind != "company" || companyNode.Label != tag+" 企业" || companyNode.CompanyID != companyID {
		t.Fatalf("company node = %+v", companyNode)
	}
	rootNode := nodeByKey(tree, assetKey(rootID))
	if rootNode == nil || rootNode.Parent != companyKey(companyID) {
		t.Fatalf("root parent = %+v", rootNode)
	}
	if companyNode.Total != 1 || companyNode.High != 1 {
		t.Fatalf("company totals = %+v", companyNode)
	}
}

// TestInterceptGateBlocksAssets covers the outbound asset gate: global block
// rules, task allow lists and the "no rules → allow everything" fast path.
func TestInterceptGateBlocksAssets(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tag := fmt.Sprintf("batch31-gate-%d", time.Now().UnixNano())
	blocked := tag + ".blocked.example"
	allowed := tag + ".allowed.example"
	other := tag + ".other.example"
	cleanup := func() {
		_, _ = d.Exec(`DELETE FROM assets WHERE root_domain=ANY($1::text[])`, []string{blocked, allowed, other})
	}
	cleanup()
	t.Cleanup(cleanup)

	blockedID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": blocked, "root_domain": blocked})
	allowedID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": allowed, "root_domain": allowed})
	otherID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": other, "root_domain": other})

	if hits, err := d.Assets().CheckAssetsIntercept(0, nil); err != nil || hits != nil {
		t.Fatalf("empty id set = %+v, %v", hits, err)
	}
	// 无规则:直接放行。
	if hits, err := d.Assets().CheckAssetsIntercept(0, []int64{blockedID}); err != nil || len(hits) != 0 {
		t.Fatalf("no-rule gate = %+v, %v", hits, err)
	}

	rule, err := d.CreateAssetInterceptRule("exact_domain", blocked, "batch31", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM asset_intercept_rules WHERE id=$1`, rule.ID) })
	hits, err := d.Assets().CheckAssetsIntercept(0, []int64{blockedID, allowedID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Asset.ID != blockedID || hits[0].Reason == "" {
		t.Fatalf("blocked gate = %+v", hits)
	}
	if desc := hits[0].Describe(); !strings.Contains(desc, blocked) {
		t.Fatalf("hit description = %q", desc)
	}

	// 任务白名单:不在名单内的资产一律拒绝,名单内放行。
	task, err := d.CreateTaskWithOptions(tag+" gate", "gate", TaskCreateOptions{Name: tag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	if _, err := d.Assets().CreateTaskInterceptRule(task.ID, "allow", "exact_domain", allowed, "", true); err != nil {
		t.Fatal(err)
	}
	hits, err = d.Assets().CheckAssetsIntercept(task.ID, []int64{blockedID, allowedID, otherID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("allow-list gate = %+v", hits)
	}
	byID := map[int64]string{}
	for _, hit := range hits {
		byID[hit.Asset.ID] = hit.Reason
	}
	if !strings.Contains(byID[otherID], "白名单") || !strings.Contains(byID[blockedID], "拦截规则") {
		t.Fatalf("gate reasons = %+v", byID)
	}
	// 未命中的禁用规则不参与判定。
	if _, err := d.Assets().CreateTaskInterceptRule(task.ID, "block", "exact_domain", blocked+"-off", "", false); err != nil {
		t.Fatal(err)
	}
	if hits, err = d.Assets().CheckAssetsIntercept(task.ID, []int64{allowedID}); err != nil || len(hits) != 0 {
		t.Fatalf("allowed asset = %+v, %v", hits, err)
	}
}

// TestInheritedActivityAndMainSessions covers the direct-source activity reads
// and the main-agent session segment listing.
func TestInheritedActivityAndMainSessions(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tag := fmt.Sprintf("batch31-src-%d", time.Now().UnixNano())
	source, err := d.CreateTaskWithOptions(tag+" source", "source", TaskCreateOptions{Name: tag})
	if err != nil {
		t.Fatal(err)
	}
	current, err := d.CreateTaskWithOptions(tag+" current", "current", TaskCreateOptions{SourceTaskIDs: []int64{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteTask(current.ID)
		_ = d.DeleteTask(source.ID)
	})

	srcStore := d.Exploration(source.ExplorationID)
	sourceIntent, err := srcStore.AddIntent(map[string]any{"summary": "source work"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	sourceFact, err := srcStore.AddNode(KindFact, map[string]any{"summary": "needle fact"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srcStore.Link(sourceIntent, RelYields, sourceFact); err != nil {
		t.Fatal(err)
	}
	if _, err := srcStore.AppendActivity(Activity{
		NodeID: &sourceIntent, Worker: "worker", Kind: "text", Summary: "inherited needle", Detail: "detail",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srcStore.SetIntentState(sourceIntent, "done"); err != nil {
		t.Fatal(err)
	}

	cur := d.Exploration(current.ExplorationID)
	localIntent, err := cur.AddIntent(map[string]any{"summary": "local work"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cur.AppendActivity(Activity{NodeID: &localIntent, Worker: "worker", Kind: "text", Summary: "local step"}); err != nil {
		t.Fatal(err)
	}

	inherited, cursor, err := cur.ActivityListWithSources(sourceIntent, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inherited) != 1 || !inherited[0].Inherited || inherited[0].SourceTaskID != source.ID || cursor == 0 {
		t.Fatalf("inherited activity = %+v (cursor %d)", inherited, cursor)
	}
	local, _, err := cur.ActivityListWithSources(localIntent, 0, 10)
	if err != nil || len(local) != 1 || local[0].Inherited {
		t.Fatalf("local activity = %+v, %v", local, err)
	}
	if none, _, err := cur.ActivityListWithSources(999999999, 0, 10); err != nil || len(none) != 0 {
		t.Fatalf("unknown node activity = %+v, %v", none, err)
	}

	hits, err := cur.ActivityTraceSearchWithSources(sourceIntent, "needle", 10)
	if err != nil || len(hits) != 1 || !hits[0].Inherited {
		t.Fatalf("inherited search = %+v, %v", hits, err)
	}
	if localHits, err := cur.ActivityTraceSearchWithSources(localIntent, "local", 10); err != nil || len(localHits) != 1 {
		t.Fatalf("local search = %+v, %v", localHits, err)
	}
	if missing, err := cur.ActivityTraceSearchWithSources(999999999, "needle", 10); err != nil || len(missing) != 0 {
		t.Fatalf("unknown node search = %+v, %v", missing, err)
	}

	// 继承的 fact 出现在带来源的分页里,且标记为 inherited。
	nodes, _, total, err := cur.ListByKindPageWithSources(KindFact, 0, 10, "needle")
	if err != nil {
		t.Fatal(err)
	}
	if total < 1 || len(nodes) != 1 || !nodes[0].Inherited || nodes[0].SourceTaskID != source.ID {
		t.Fatalf("source page = %+v (total %d), %v", nodes, total, err)
	}

	first, err := cur.NewMainSession()
	if err != nil || first.Seq != 1 {
		t.Fatalf("first main session = %+v, %v", first, err)
	}
	second, err := cur.NewMainSession()
	if err != nil || second.Seq != 2 {
		t.Fatalf("second main session = %+v, %v", second, err)
	}
	if seg, err := cur.CurrentMainSeg(); err != nil || seg != 2 {
		t.Fatalf("current main seg = %d, %v", seg, err)
	}
	sessions, err := cur.ListMainSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 || sessions[0].Seq != 2 || sessions[1].Seq != 1 || sessions[2].Seq != 0 {
		t.Fatalf("main sessions = %+v", sessions)
	}
	if !sessions[2].CreatedAt.IsZero() {
		t.Fatalf("implicit session 0 must have no timestamp: %+v", sessions[2])
	}
}
