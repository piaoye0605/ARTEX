package db

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCoverageScopeGraphAndBacklog covers the task-scope writer plus the three
// read models built on the same scope→assets predicate: TaskCoverage,
// BuildCoverageGraph and ListUntestedAssets.
func TestCoverageScopeGraphAndBacklog(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions("coverage scope graph", "build coverage graph", TaskCreateOptions{Name: "coverage"})
	if err != nil {
		t.Fatal(err)
	}
	root := fmt.Sprintf("covgraph-%d.example", task.ID)
	ip := fmt.Sprintf("10.42.%d.%d", (task.ID/250)%250, task.ID%250+1)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM task_scope WHERE task_id=$1`, task.ID)
		_, _ = d.Exec(`DELETE FROM assets WHERE domain LIKE $1 OR root_domain LIKE $1`, "%"+root)
		_, _ = d.Exec(`DELETE FROM assets WHERE ip=$1`, ip)
		_ = d.DeleteTask(task.ID)
	})

	rootID, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: root, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	subID, err := assets.UpsertSubdomain(UpsertSubdomainReq{
		Domain: "www." + root, RecordType: "A", RecordValue: []string{ip}, TaskID: task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ipID, err := assets.UpsertIP(UpsertIPReq{IP: ip, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}

	// root_domain 允许 host:port 形式，端口应被丢弃后入库。
	scope, err := assets.AddAgentScope(task.ID, "root_domain", root+":8443", "coverage", "")
	if err != nil {
		t.Fatal(err)
	}
	if scope.Domain != root || scope.Source != "agent" {
		t.Fatalf("root scope = %+v", scope)
	}
	ipScope, err := assets.AddAgentScope(task.ID, "ip", ip, "coverage", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if ipScope.Kind != "ip" || ipScope.Net != ip+"/32" || ipScope.Source != "manual" {
		t.Fatalf("ip scope = %+v", ipScope)
	}
	cidrScope, err := assets.AddAgentScope(task.ID, "cidr", ip+"/32", "coverage", "")
	if err != nil || cidrScope.Kind != "cidr" || cidrScope.Net != ip+"/32" {
		t.Fatalf("cidr scope = %+v, %v", cidrScope, err)
	}
	if _, err := assets.AddAgentScope(task.ID, "keyword", " 机房  ", "coverage", ""); err != nil {
		t.Fatalf("keyword scope: %v", err)
	}

	for _, tc := range []struct {
		name     string
		taskID   int64
		kind     string
		value    string
		wantHint string
	}{
		{"no task", 0, "root_domain", root, "需要 task_id"},
		{"empty value", task.ID, "root_domain", "   ", "value 不能为空"},
		{"invalid root", task.ID, "root_domain", ".", "无效根域"},
		{"invalid ip", task.ID, "ip", "not-an-ip", "无效 ip/cidr"},
		{"invalid cidr", task.ID, "cidr", "10.0.0.0/99", "无效 ip/cidr"},
		{"unknown kind", task.ID, "bogus", "x", "不支持的 kind"},
		{"missing company", task.ID, "company", "no-such-company-batch30", "company 不存在"},
	} {
		if _, err := assets.AddAgentScope(tc.taskID, tc.kind, tc.value, "coverage", ""); err == nil || !contains(err.Error(), tc.wantHint) {
			t.Fatalf("%s: err=%v, want hint %q", tc.name, err, tc.wantHint)
		}
	}

	store := d.Exploration(task.ExplorationID)
	if _, err := store.AddNode(KindFact, map[string]any{"summary": "coverage fact"}, 1, "confirmed", "worker", []int64{subID}); err != nil {
		t.Fatal(err)
	}

	cov, err := assets.TaskCoverage(task.ID, task.ExplorationID)
	if err != nil {
		t.Fatal(err)
	}
	if cov.ScopeRows < 4 || cov.Denominator < 3 || cov.Tested != 1 || cov.Pct == nil {
		t.Fatalf("coverage = %+v", cov)
	}
	if len(cov.ByType) == 0 {
		t.Fatal("coverage by type is empty")
	}

	graph, err := assets.BuildCoverageGraph(task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) < 3 || len(graph.Edges) == 0 {
		t.Fatalf("graph nodes=%d edges=%d", len(graph.Nodes), len(graph.Edges))
	}
	tested := false
	for _, n := range graph.Nodes {
		if n.AssetID == subID && n.Tested {
			tested = true
		}
	}
	if !tested {
		t.Fatal("anchored subdomain must be marked tested in the graph")
	}
	if empty, err := assets.BuildCoverageGraph(0, 0); err != nil || len(empty.Nodes) != 0 || len(empty.Edges) != 0 {
		t.Fatalf("empty graph = %+v, %v", empty, err)
	}

	backlog, total, err := assets.ListUntestedAssets(task.ID, task.ExplorationID, "", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 || len(backlog) != total || len(backlog) > 10 {
		t.Fatalf("backlog total=%d page=%d", total, len(backlog))
	}
	typed, typedTotal, err := assets.ListUntestedAssets(task.ID, task.ExplorationID, "ip", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if typedTotal != 1 || len(typed) != 1 || typed[0].Type != "ip" || typed[0].ID != ipID {
		t.Fatalf("typed backlog = %+v (total %d)", typed, typedTotal)
	}

	// 仅声明子域范围时，图中应补一个不在范围内的根域连接节点。
	connectorTask, err := d.CreateTaskWithOptions("coverage connector", "connector root", TaskCreateOptions{Name: "connector"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM task_scope WHERE task_id=$1`, connectorTask.ID)
		_ = d.DeleteTask(connectorTask.ID)
	})
	if _, err := assets.AddAgentScope(connectorTask.ID, "subdomain", "www."+root, "connector", ""); err != nil {
		t.Fatal(err)
	}
	connector, err := assets.BuildCoverageGraph(connectorTask.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundConnector := false
	for _, n := range connector.Nodes {
		if n.Kind == "root_domain" && !n.InScope {
			foundConnector = true
		}
	}
	if !foundConnector {
		t.Fatalf("connector root missing from graph: %+v", connector.Nodes)
	}
	if rootID == 0 || subID == 0 {
		t.Fatal("asset upserts returned zero ids")
	}
}

// TestArchiveListingDeleteQueueAndStub covers listing, the delete queue and the
// terminal stub removal that no other test exercises.
func TestArchiveListingDeleteQueueAndStub(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTaskWithOptions("archive listing", "list and delete", TaskCreateOptions{Name: "listing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	var records bytes.Buffer
	snapshot, err := d.SnapshotTaskArchiveWithLLMRecords(task.ID, &records)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/batch30.tar.zst", "batch30", 12, 6); err != nil {
		t.Fatal(err)
	}

	page, err := d.ListTaskArchives(fmt.Sprint(task.ID), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total < 1 || page.Page != 1 || page.Size != 20 || len(page.Items) == 0 {
		t.Fatalf("archive page = %+v", page)
	}
	byState, err := d.ListTaskArchives("", ArchiveReady, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if byState.Size != 20 || byState.Total < 1 {
		t.Fatalf("state-filtered page = %+v", byState)
	}
	stats, err := d.ArchivedAggregateStats()
	if err != nil || len(stats) == 0 {
		t.Fatalf("archived aggregate stats = %d, %v", len(stats), err)
	}
	// ready 状态的归档不能被当作“已还原”消费。
	if err := d.CompleteTaskArchiveRestore(archive.ID); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatalf("consume ready archive = %v", err)
	}
	if err := d.DeleteTaskArchiveStub(archive.ID); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatalf("delete stub in ready state = %v", err)
	}

	queued, err := d.QueueTaskArchiveDelete(archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != DeleteQueued {
		t.Fatalf("queued state = %s", queued.State)
	}
	if _, err := d.QueueTaskArchiveDelete(archive.ID); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatalf("re-queue delete = %v", err)
	}
	if _, err := d.QueueTaskArchiveDelete(-1); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatalf("queue delete for missing archive = %v", err)
	}

	claimed := false
	for attempt := 0; attempt < 20 && !claimed; attempt++ {
		item, err := d.ClaimTaskArchiveJob(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if item == nil {
			break
		}
		if item.ID == archive.ID {
			claimed = item.State == Deleting
		}
	}
	if !claimed {
		t.Fatal("delete job was never claimed")
	}
	if err := d.DeleteTaskArchiveStub(archive.ID); err != nil {
		t.Fatal(err)
	}
	if live, err := d.GetTask(task.ID); err != nil || live != nil {
		t.Fatalf("stub task survived deletion: %+v, %v", live, err)
	}
	if item, err := d.GetTaskArchive(archive.ID); err != nil || item != nil {
		t.Fatalf("archive metadata survived deletion: %+v, %v", item, err)
	}
	if err := d.DeleteTaskArchiveStub(archive.ID); err == nil {
		t.Fatal("deleting a consumed archive must fail")
	}
}

// TestUsageDailyJudgeAndTokenStats covers the always-on usage ledger readers.
func TestUsageDailyJudgeAndTokenStats(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tag := fmt.Sprintf("usage-batch30-%d", time.Now().UnixNano())
	profile := tag + "-profile"
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, tag) })
	if err := d.InsertLLMUsage(&LLMUsage{TaskID: tag, Worker: "judge", Model: "judge-model", ProfileName: profile, InputTokens: 40, OutputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertLLMUsage(&LLMUsage{TaskID: tag, Worker: "judge", Model: "judge-model", ProfileName: profile, InputTokens: 20, OutputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertLLMUsage(&LLMUsage{TaskID: tag, Worker: "worker", Model: "", ProfileName: "", InputTokens: 3, OutputTokens: 2, Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	daily, err := d.UsageDaily(0)
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, row := range daily {
		if row.ProfileName == profile {
			sum += row.InputTokens
		}
		if row.Date == "" {
			t.Fatalf("daily row without date: %+v", row)
		}
	}
	if sum != 60 {
		t.Fatalf("daily input tokens = %d, want 60", sum)
	}
	if filtered, err := d.UsageDaily(7); err != nil || len(filtered) == 0 {
		t.Fatalf("windowed daily = %d rows, %v", len(filtered), err)
	}

	byModel, err := d.TokenByModel(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 2 || byModel[0].Calls != 2 {
		t.Fatalf("token by model = %+v", byModel)
	}

	byProfile, err := d.UsageByProfile()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range byProfile {
		if row.ProfileName == profile {
			found = row.Calls == 2 && row.Tasks == 1
		}
	}
	if !found {
		t.Fatalf("profile usage missing %q: %+v", profile, byProfile)
	}

	judge, err := d.JudgeUsageStats(0)
	if err != nil {
		t.Fatal(err)
	}
	if judge.Calls < 2 || judge.InputTokens < 60 || len(judge.Daily) == 0 {
		t.Fatalf("judge usage = %+v", judge)
	}
	if judge7, err := d.JudgeUsageStats(7); err != nil || judge7.Daily == nil {
		t.Fatalf("windowed judge usage = %+v, %v", judge7, err)
	}
}

// TestFindingTrafficEditPopulateAndRecordValidation covers evidence binding
// editing (the user-facing reorder/retag/remove path) and the read-time
// enrichment that never guesses finding ids.
func TestFindingTrafficEditPopulateAndRecordValidation(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions("finding traffic edit", "edit bindings", TaskCreateOptions{Name: "traffic"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)

	finding, err := store.RecordFinding(t.Context(), RecordFindingInput{
		TaskID: task.ID, Worker: "worker", VulnClass: "SQL注入", Name: "batch30", Severity: "high", Summary: "summary",
	})
	if err != nil {
		t.Fatal(err)
	}

	rollback := func(fn func(*sql.Tx) error) error {
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		return fn(tx)
	}
	if err := rollback(func(tx *sql.Tx) error {
		_, err := RecordFindingTx(t.Context(), tx, RecordFindingInput{
			TaskID: task.ID, ExplorationID: task.ExplorationID + 9999,
		}, nil)
		return err
	}); err == nil || !contains(err.Error(), "不匹配") {
		t.Fatalf("mismatched exploration = %v", err)
	}
	if err := rollback(func(tx *sql.Tx) error {
		_, err := RecordFindingTx(t.Context(), tx, RecordFindingInput{
			TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: 999999999,
		}, nil)
		return err
	}); err == nil || !contains(err.Error(), "intent_id") {
		t.Fatalf("foreign intent = %v", err)
	}

	snapshot := TrafficEvidenceSnapshot{
		SourceTrafficID: "traffic-batch30", CapturedAt: time.Now().Unix(), URL: "https://batch30.example/api",
		Method: "GET", Status: 200, ContentType: "application/json", ReqHash: "req", RespHash: "resp", ReqLen: 10, RespLen: 20,
	}
	snapshot.ID = TrafficSnapshotID(snapshot)
	prepared := []PreparedTrafficEvidence{{Ref: TrafficRef{TrafficID: "traffic-batch30", Role: "proof", Note: "proof"}, Snapshot: snapshot}}
	if err := d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		return AddFindingTrafficTx(tx, finding.FindingID, prepared)
	}); err != nil {
		t.Fatal(err)
	}
	// 重复绑定同一快照不应再次 bump 版本。
	if err := d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		return AddFindingTrafficTx(tx, finding.FindingID, prepared)
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		return InsertEvidenceSnapshotTx(tx, TrafficEvidenceSnapshot{ID: "forged"})
	}); err == nil {
		t.Fatal("forged snapshot hash must be rejected")
	}

	current, err := d.GetFindingTraffic(t.Context(), finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Bindings) != 1 || current.Version != 1 {
		t.Fatalf("bindings = %+v", current)
	}
	bindingID := current.Bindings[0].ID

	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, bindingID, current.Version, strPtr("bogus"), nil, false, nil); err == nil {
		t.Fatal("invalid role must be rejected")
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, bindingID, current.Version+3, nil, nil, false, nil); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("stale version = %v", err)
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, 0, current.Version, nil, nil, false, []int64{bindingID, bindingID}); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("order length mismatch = %v", err)
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, 0, current.Version, nil, nil, false, []int64{99999999}); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("order with foreign id = %v", err)
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, bindingID, current.Version, strPtr("baseline"), strPtr("retagged"), false, nil); err != nil {
		t.Fatal(err)
	}
	retagged, err := d.GetFindingTraffic(t.Context(), finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if retagged.Bindings[0].Role != "baseline" || retagged.Bindings[0].Note != "retagged" {
		t.Fatalf("retag = %+v", retagged.Bindings[0])
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, 99999999, retagged.Version, nil, nil, false, nil); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("missing binding = %v", err)
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, 0, retagged.Version, nil, nil, false, []int64{bindingID}); err != nil {
		t.Fatal(err)
	}
	reordered, err := d.GetFindingTraffic(t.Context(), finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EditFindingTraffic(t.Context(), finding.FindingID, bindingID, reordered.Version, nil, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	emptied, err := d.GetFindingTraffic(t.Context(), finding.FindingID)
	if err != nil || len(emptied.Bindings) != 0 {
		t.Fatalf("after removal = %+v, %v", emptied, err)
	}

	node := &Node{ID: finding.NodeID, Kind: KindFinding}
	if err := store.PopulateFindingTrafficIDs([]*Node{nil, {ID: 1, Kind: KindFact}, node, node}); err != nil {
		t.Fatal(err)
	}
	if node.FindingID != finding.FindingID || node.FindingNodeID != finding.NodeID || node.TrafficCount != 0 {
		t.Fatalf("populated node = %+v", node)
	}
	if err := store.PopulateFindingTrafficIDs(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := FindingTrafficTx(mustTx(t, d), 99999999); !errors.Is(err, ErrFindingNotFound) {
		t.Fatalf("missing finding traffic = %v", err)
	}
}

// TestLogsGraphEdgesAndIntentDiscard covers the log readers, exploration edge
// readers and the admission-rollback deletion of an open intent.
func TestLogsGraphEdgesAndIntentDiscard(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	task, err := d.CreateTaskWithOptions("logs edges discard", "graph edges", TaskCreateOptions{Name: "edges"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)

	intentID, err := store.AddIntent(map[string]any{"summary": "intent"}, 1, nil, "worker")
	if err != nil {
		t.Fatal(err)
	}
	factID, err := store.AddNode(KindFact, map[string]any{"summary": "fact"}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(intentID, RelYields, factID); err != nil {
		t.Fatal(err)
	}
	edges, err := store.Edges(0)
	if err != nil || len(edges) != 1 {
		t.Fatalf("edges = %+v, %v", edges, err)
	}
	touching, err := store.EdgesTouching([]int64{intentID})
	if err != nil || len(touching) != 1 || touching[0].To != factID {
		t.Fatalf("touching = %+v, %v", touching, err)
	}
	if none, err := store.EdgesTouching(nil); err != nil || none != nil {
		t.Fatalf("empty touching = %+v, %v", none, err)
	}

	first, err := d.InsertLog("info", "batch30", "first line")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.InsertLog("warn", "batch30", "second line")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM server_logs WHERE id=ANY($1)`, []int64{first, second}) })
	recent, err := d.RecentLogs(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) < 2 || recent[0].ID > recent[len(recent)-1].ID {
		t.Fatalf("recent logs are not oldest-first: %+v", recent)
	}
	before, err := d.ListLogsBefore(second, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range before {
		if row.ID >= second {
			t.Fatalf("ListLogsBefore returned id %d >= %d", row.ID, second)
		}
	}
	foundFirst := false
	for _, row := range before {
		if row.ID == first {
			foundFirst = true
		}
	}
	if !foundFirst {
		t.Fatalf("first log missing from page: %+v", before)
	}

	if err := store.DiscardOpenIntent(intentID); err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardOpenIntent(intentID); err == nil {
		t.Fatal("discarding an already removed intent must fail")
	}
}

func strPtr(v string) *string { return &v }

func mustTx(t *testing.T, d *DB) *sql.Tx {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}
