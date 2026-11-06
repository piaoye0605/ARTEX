package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// callToolResult invokes a domain tool and reports both the flattened text and
// the harness error flag (actool.Errorf sets IsError instead of returning err).
func callToolResult(t *testing.T, tool actool.CoreTool, payload any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", tool.Name(), err)
	}
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("%s call: %v", tool.Name(), err)
	}
	return res.Flatten(), res.IsError
}

func decodeToolMap(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode tool result %q: %v", text, err)
	}
	return out
}

// toolIDs decodes the {"ids":[...]} shape shared by every batch write tool.
func toolIDs(t *testing.T, out map[string]any) []int64 {
	t.Helper()
	raw, ok := out["ids"].([]any)
	if !ok {
		t.Fatalf("batch result missing ids: %#v", out)
	}
	ids := make([]int64, len(raw))
	for i, v := range raw {
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("id %d not numeric: %#v", i, v)
		}
		ids[i] = int64(f)
	}
	return ids
}

func toolErrors(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	errs, ok := out["errors"].(map[string]any)
	if !ok {
		t.Fatalf("batch result missing errors: %#v", out)
	}
	return errs
}

// =====================================================================
// 写工具：单条/批量、回调、绑定开关与各类拒绝分支
// =====================================================================
func TestWriteToolsBatchPathsAndGuards(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tag := fmt.Sprintf("batch32-%d", time.Now().UnixNano())
	task, err := d.CreateTask(tag, "write tool paths", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()

	ts := NewToolSet(d.Exploration(task.ExplorationID), "fixture")
	ts.taskID = task.ID
	ts.SetAssetStore(d.Assets(), d.Companies())

	var hintTexts, goalTexts, bareWakes, resumes int
	ts.SetNotifyHint(func(texts []string) { hintTexts += len(texts) })
	ts.SetNotifyGoal(func(texts []string) { goalTexts += len(texts) })
	ts.SetNotify(func() { bareWakes++ })
	ts.SetResumeTask(func() { resumes++ })

	oldBinding := FindingTrafficBindingEnabled
	t.Cleanup(func() { FindingTrafficBindingEnabled = oldBinding })

	// --- add_hint：单条 ---
	text, isErr := callToolResult(t, ts.addHint(), map[string]any{"text": "focus authenticated APIs"})
	if isErr || !strings.HasPrefix(text, "hint added: ") {
		t.Fatalf("single hint = %q (err=%v)", text, isErr)
	}
	// --- add_hint：批量（一条空文本被拒，其余落库） ---
	text, isErr = callToolResult(t, ts.addHint(), map[string]any{"hints": []any{
		map[string]any{"text": "probe idor", "asset_ids": []any{0}},
		map[string]any{"text": "   "},
	}})
	if isErr {
		t.Fatalf("batch hint reported harness error: %q", text)
	}
	out := decodeToolMap(t, text)
	ids := toolIDs(t, out)
	errs := toolErrors(t, out)
	if len(ids) != 2 || ids[0] <= 0 || ids[1] != 0 || errs["1"] == nil {
		t.Fatalf("batch hint ids/errors = %#v %#v", ids, errs)
	}
	if hintTexts != 2 {
		t.Fatalf("hint notifications = %d, want 2 (1 single + 1 successful batch item)", hintTexts)
	}

	// --- add_hint：绑定关闭时携带 traffic_refs 被拒 ---
	FindingTrafficBindingEnabled = func() bool { return false }
	text, isErr = callToolResult(t, ts.addHint(), map[string]any{
		"text":         "hand over traffic",
		"traffic_refs": []any{map[string]any{"traffic_id": "t-1", "role": "proof"}},
	})
	if !isErr || !strings.Contains(text, "自动绑定流量已关闭") {
		t.Fatalf("disabled binding hint = %q (err=%v)", text, isErr)
	}

	// --- add_hint：绑定开启后合法 refs 落库，非法 refs 进 errors ---
	FindingTrafficBindingEnabled = func() bool { return true }
	text, isErr = callToolResult(t, ts.addHint(), map[string]any{"hints": []any{
		map[string]any{"text": "with evidence", "traffic_refs": []any{
			map[string]any{"traffic_id": "t-1", "role": "proof", "note": "repro"},
		}},
		map[string]any{"text": "bad evidence", "traffic_refs": []any{map[string]any{"traffic_id": "  "}}},
	}})
	if isErr {
		t.Fatalf("enabled binding batch = %q", text)
	}
	out = decodeToolMap(t, text)
	ids = toolIDs(t, out)
	if ids[0] <= 0 || ids[1] != 0 || !strings.Contains(fmt.Sprint(toolErrors(t, out)["1"]), "traffic_id 不能为空") {
		t.Fatalf("enabled binding batch = %#v", out)
	}
	// graph_overview 只在开关打开时折叠 traffic_refs
	overview := ts.graphOverviewData()
	hints, _ := overview["hints"].([]map[string]any)
	var withRefs int
	for _, h := range hints {
		if h["traffic_refs"] != nil {
			withRefs++
		}
	}
	if withRefs != 1 {
		t.Fatalf("overview hints carrying traffic_refs = %d (%#v)", withRefs, hints)
	}
	FindingTrafficBindingEnabled = oldBinding

	// --- set_goals：单条 + 批量（空目标被拒） ---
	text, isErr = callToolResult(t, ts.setGoals(), map[string]any{"text": "own the admin panel"})
	if isErr || !strings.HasPrefix(text, "goal added: ") {
		t.Fatalf("single goal = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.setGoals(), map[string]any{"goals": []any{
		map[string]any{"text": "dump user table", "vulnclass": "SQLi"},
		map[string]any{"text": ""},
	}})
	if isErr {
		t.Fatalf("batch goals = %q", text)
	}
	out = decodeToolMap(t, text)
	ids = toolIDs(t, out)
	if ids[0] <= 0 || ids[1] != 0 || toolErrors(t, out)["1"] == nil {
		t.Fatalf("batch goals = %#v", out)
	}
	if goalTexts != 2 || resumes != 2 {
		t.Fatalf("goal notifications = %d resumes = %d, want 2/2", goalTexts, resumes)
	}

	// --- set_constraints：单条默认 deny、非法 type 被拒、批量 ---
	text, isErr = callToolResult(t, ts.setConstraints(), map[string]any{"text": "only passive recon"})
	if isErr || !strings.HasPrefix(text, "constraint added: ") {
		t.Fatalf("single constraint = %q (err=%v)", text, isErr)
	}
	list, err := ts.ts.ListConstraints()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[len(list)-1].Kind != "deny" {
		t.Fatalf("default constraint kind must be deny: %#v", list)
	}
	text, isErr = callToolResult(t, ts.setConstraints(), map[string]any{"text": "x", "type": "maybe"})
	if !isErr || !strings.Contains(text, "type 必须是 allow 或 deny") {
		t.Fatalf("invalid constraint type = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.setConstraints(), map[string]any{"constraints": []any{
		map[string]any{"text": "may read public pages", "type": "ALLOW"},
		map[string]any{"text": " "},
	}})
	if isErr {
		t.Fatalf("batch constraints = %q", text)
	}
	out = decodeToolMap(t, text)
	ids = toolIDs(t, out)
	if ids[0] <= 0 || ids[1] != 0 || toolErrors(t, out)["1"] == nil {
		t.Fatalf("batch constraints = %#v", out)
	}

	// --- record_fact：单条带全部可选字段 + 意图校验 + 批量混合 ---
	intent, err := ts.ts.AddIntent(map[string]any{"summary": "probe admin panel"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	text, isErr = callToolResult(t, ts.recordFact(), map[string]any{
		"summary": "/admin reachable", "detail": "GET /admin → 200", "evidence": "curl -s -o /dev/null -w %{http_code} → 200",
		"confidence": "observed", "intent_id": intent, "asset_ids": []any{0, "0"},
	})
	if isErr || !strings.HasPrefix(text, "fact recorded: ") {
		t.Fatalf("single fact = %q (err=%v)", text, isErr)
	}
	if ts.writes.Facts != 1 {
		t.Fatalf("fact write counter = %d", ts.writes.Facts)
	}
	goalID, err := ts.ts.AddNode(db.KindGoal, map[string]any{"text": "proof goal"}, 0, "open", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	text, isErr = callToolResult(t, ts.recordFact(), map[string]any{"facts": []any{
		map[string]any{"summary": "second fact without intent"},
		map[string]any{"summary": "fact anchored to a goal", "intent_id": goalID},
		map[string]any{"summary": "  "},
	}})
	if isErr {
		t.Fatalf("batch fact = %q", text)
	}
	out = decodeToolMap(t, text)
	ids = toolIDs(t, out)
	errs = toolErrors(t, out)
	if ids[0] <= 0 || ids[1] != 0 || ids[2] != 0 || errs["1"] == nil || errs["2"] == nil {
		t.Fatalf("batch fact = %#v", out)
	}
	if !strings.Contains(fmt.Sprint(errs["1"]), "intent_id 必须是本任务的意图") {
		t.Fatalf("wrong intent rejection: %v", errs["1"])
	}
	text, isErr = callToolResult(t, ts.recordFact(), map[string]any{"summary": " "})
	if !isErr || !strings.Contains(text, "summary 不能为空") {
		t.Fatalf("empty fact summary = %q (err=%v)", text, isErr)
	}

	// --- add_intent：空 summary、合法锚点、非法锚点、不存在锚点、批量 ---
	text, isErr = callToolResult(t, ts.addIntent(), map[string]any{"summary": " "})
	if !isErr || !strings.Contains(text, "summary 不能为空") {
		t.Fatalf("empty intent summary = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.addIntent(), map[string]any{"summary": "top level direction"})
	if isErr || !strings.HasPrefix(text, "intent created: ") {
		t.Fatalf("single intent = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.addIntent(), map[string]any{"summary": "anchored", "parent_ids": []any{goalID}})
	if !isErr || !strings.Contains(text, "不能作为意图锚点") {
		t.Fatalf("goal as intent anchor = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.addIntent(), map[string]any{"summary": "dangling", "parent_ids": []any{987654321}})
	if !isErr || !strings.Contains(text, "不存在于本任务") {
		t.Fatalf("missing intent anchor = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.addIntent(), map[string]any{"intents": []any{
		map[string]any{"summary": "batched direction", "priority": 9},
		map[string]any{"summary": ""},
	}})
	if isErr {
		t.Fatalf("batch intents = %q", text)
	}
	out = decodeToolMap(t, text)
	ids = toolIDs(t, out)
	if ids[0] <= 0 || ids[1] != 0 || toolErrors(t, out)["1"] == nil {
		t.Fatalf("batch intents = %#v", out)
	}
	if resumes != 4 {
		t.Fatalf("resume hook fired %d times, want 4 (2 goals + 2 intents)", resumes)
	}

	// --- add_intent：资产拦截命中即拒绝下发 ---
	blocked := tag + ".blocked.example"
	assetID, err := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: blocked, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := d.CreateAssetInterceptRule("exact_domain", blocked, "batch32", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = d.DeleteAssetInterceptRule(rule.ID)
		_, _ = d.Exec(`DELETE FROM assets WHERE domain=$1`, blocked)
	}()
	text, isErr = callToolResult(t, ts.addIntent(), map[string]any{"summary": "hit blocked asset", "asset_ids": []any{assetID}})
	if !isErr || !strings.Contains(text, "未通过测试范围校验") {
		t.Fatalf("intercepted intent = %q (err=%v)", text, isErr)
	}

	// --- prove_goal：必填、错类型、成功 ---
	text, isErr = callToolResult(t, ts.proveGoal(), map[string]any{"goal_id": 0, "evidence_id": 0})
	if !isErr || !strings.Contains(text, "必填") {
		t.Fatalf("empty prove_goal = %q (err=%v)", text, isErr)
	}
	factID, err := ts.ts.AddNode(db.KindFact, map[string]any{"summary": "evidence fact"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	text, isErr = callToolResult(t, ts.proveGoal(), map[string]any{"goal_id": factID, "evidence_id": factID})
	if !isErr || !strings.Contains(text, "goal_id 必须是本任务的目标节点") {
		t.Fatalf("goal_id not a goal = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.proveGoal(), map[string]any{"goal_id": goalID, "evidence_id": intent})
	if !isErr || !strings.Contains(text, "evidence_id 必须是本任务或直接关联任务的事实/漏洞节点") {
		t.Fatalf("evidence_id not a fact = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.proveGoal(), map[string]any{
		"goal_id": goalID, "evidence_id": factID, "reason": "fact proves goal",
	})
	if isErr || !strings.Contains(text, fmt.Sprintf("goal %d marked met", goalID)) {
		t.Fatalf("prove_goal = %q (err=%v)", text, isErr)
	}

	// --- report_finding：坏 JSON、非法 evidence_hint_id、非 hint 锚点、仅 notify 回调 ---
	if text, isErr = rawToolCall(t, ts.addFinding(), `{`); !isErr || !strings.Contains(text, "unexpected end of JSON") {
		t.Fatalf("broken finding json = %q (err=%v)", text, isErr)
	}
	// 只有绑定开关打开时才会解析 evidence_hint_id（关闭时入参被整体忽略）
	FindingTrafficBindingEnabled = func() bool { return true }
	defer func() { FindingTrafficBindingEnabled = oldBinding }()
	text, isErr = callToolResult(t, ts.addFinding(), map[string]any{
		"vulnclass": "TEST", "severity": "low", "summary": "bad hint id", "evidence_hint_id": "abc",
	})
	if !isErr || !strings.Contains(text, "evidence_hint_id 必须为有效的提示节点 ID") {
		t.Fatalf("invalid hint id = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.addFinding(), map[string]any{
		"vulnclass": "TEST", "severity": "low", "summary": "fact is not a hint", "evidence_hint_id": factID,
	})
	if !isErr || !strings.Contains(text, "必须是本任务的提示节点") {
		t.Fatalf("non-hint evidence anchor = %q (err=%v)", text, isErr)
	}
	FindingTrafficBindingEnabled = oldBinding
	ts.SetNotifyFinding(nil)
	ts.SetNotify(func() { bareWakes++ })
	before := bareWakes
	text, isErr = callToolResult(t, ts.addFinding(), map[string]any{
		"vulnclass": "TEST", "severity": "low", "summary": "notify fallback", "evidence": "observed in fixture",
	})
	if isErr || !strings.HasPrefix(text, "finding recorded: ") {
		t.Fatalf("finding via notify fallback = %q (err=%v)", text, isErr)
	}
	if bareWakes != before+1 || ts.writes.Findings != 1 {
		t.Fatalf("notify fallback = %d (want %d), findings=%d", bareWakes, before+1, ts.writes.Findings)
	}
}

// rawToolCall sends an already-encoded body (used for malformed JSON paths).
func rawToolCall(t *testing.T, tool actool.CoreTool, body string) (string, bool) {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(body), nil)
	if err != nil {
		t.Fatalf("%s call: %v", tool.Name(), err)
	}
	return res.Flatten(), res.IsError
}

// =====================================================================
// 资产工具：范围、未测清单、查询、企业、插入与闸门
// =====================================================================
func TestInsertToolScopeCompanyAndQueryPaths(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tag := fmt.Sprintf("batch32-ins-%d", time.Now().UnixNano())
	task, err := d.CreateTask(tag, "asset tool paths", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()

	ts := NewToolSet(d.Exploration(task.ExplorationID), "fixture")
	ts.taskID = task.ID
	ts.SetAssetStore(d.Assets(), d.Companies())

	// 未接 store / 无任务上下文 的两类拒绝
	if text, isErr := callToolResult(t, NewToolSet(nil, "").addTaskScope(), map[string]any{"kind": "ip", "value": "10.0.0.1"}); !isErr || !strings.Contains(text, "未启用") {
		t.Fatalf("nil store add_task_scope = %q (err=%v)", text, isErr)
	}
	noTask := NewToolSet(nil, "")
	noTask.SetAssetStore(d.Assets(), d.Companies())
	if text, isErr := callToolResult(t, noTask.addTaskScope(), map[string]any{"kind": "ip", "value": "10.0.0.1"}); !isErr || !strings.Contains(text, "需要任务上下文") {
		t.Fatalf("taskless add_task_scope = %q (err=%v)", text, isErr)
	}

	// add_task_scope：批量（合法 + 未知 kind）与单条 keyword
	text, isErr := callToolResult(t, ts.addTaskScope(), map[string]any{
		"entries": []any{
			map[string]any{"kind": "ip", "value": "10.32.7.9"},
			map[string]any{"kind": "nonsense", "value": "x"},
		},
		"reason": "batch32 scope",
	})
	if isErr {
		t.Fatalf("batch add_task_scope = %q", text)
	}
	out := decodeToolMap(t, text)
	added, _ := out["added"].([]any)
	if len(added) != 1 || toolErrors(t, out)["1"] == nil {
		t.Fatalf("batch add_task_scope = %#v", out)
	}
	text, isErr = callToolResult(t, ts.addTaskScope(), map[string]any{"kind": "keyword", "value": tag + "-corp", "reason": "audit"})
	if isErr || !strings.Contains(text, `"kind":"keyword"`) {
		t.Fatalf("single add_task_scope = %q (err=%v)", text, isErr)
	}

	// list_untested_assets：默认分页 + 无任务上下文
	text, isErr = callToolResult(t, ts.listUntestedAssets(), map[string]any{})
	if isErr {
		t.Fatalf("list_untested_assets = %q", text)
	}
	out = decodeToolMap(t, text)
	if out["page"].(float64) != 1 || out["page_size"].(float64) != 10 {
		t.Fatalf("untested defaults = %#v", out)
	}
	if text, isErr = callToolResult(t, noTask.listUntestedAssets(), map[string]any{}); !isErr || !strings.Contains(text, "需要任务上下文") {
		t.Fatalf("taskless list_untested_assets = %q (err=%v)", text, isErr)
	}

	// insert_assets：坏 JSON、未知类型、app、ownerNode 锚点、闸门拦截
	if text, isErr = rawToolCall(t, ts.insertAssets(), `{"assets":`); !isErr || !strings.Contains(text, "invalid input") {
		t.Fatalf("broken insert_assets json = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.insertAssets(), map[string]any{"assets": []any{
		map[string]any{"type": "weird", "domain": tag + ".weird.example"},
	}})
	if isErr {
		t.Fatalf("unknown asset type = %q", text)
	}
	out = decodeToolMap(t, text)
	if errs, _ := out["errors"].([]any); len(errs) != 1 || !strings.Contains(fmt.Sprint(errs[0]), "unknown type") {
		t.Fatalf("unknown asset type = %#v", out)
	}

	appName := tag + "-app"
	defer func() {
		_, _ = d.Exec(`DELETE FROM assets WHERE app_name=$1 OR domain=$2`, appName, tag+".owner.example")
	}()
	text, isErr = callToolResult(t, ts.insertAssets(), map[string]any{"assets": []any{
		map[string]any{"type": "app", "app_name": appName, "bundle_id": "com.example." + tag},
		map[string]any{"type": "root_domain", "domain": tag + ".owner.example"},
	}})
	if isErr {
		t.Fatalf("app insert = %q", text)
	}
	out = decodeToolMap(t, text)
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("app/root insert results = %#v", out)
	}
	var rootID int64 = -1
	for _, r := range results {
		m := r.(map[string]any)
		if m["type"] == "root_domain" {
			rootID = int64(m["id"].(float64))
		}
	}
	if rootID <= 0 {
		t.Fatalf("root insert id = %#v", results)
	}
	// ownerNode>0：worker 意图归属的 summary 分支
	ownerNode, err := ts.ts.AddIntent(map[string]any{"summary": "owner for asset inserts"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	ts.SetOwnerNode(ownerNode)
	text, isErr = callToolResult(t, ts.insertAssets(), map[string]any{"assets": []any{
		map[string]any{"type": "subdomain", "domain": tag + ".owner.example"},
	}})
	if isErr {
		t.Fatalf("owner anchored insert = %q", text)
	}
	ts.SetOwnerNode(0)

	// 闸门：全局精确域名拦截
	blockedDomain := tag + ".blocked.example"
	rule, err := d.CreateAssetInterceptRule("exact_domain", blockedDomain, "batch32 insert gate", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteAssetInterceptRule(rule.ID) }()
	text, isErr = callToolResult(t, ts.insertAssets(), map[string]any{"assets": []any{
		map[string]any{"type": "subdomain", "domain": blockedDomain},
	}})
	if isErr {
		t.Fatalf("gated insert = %q", text)
	}
	out = decodeToolMap(t, text)
	if errs, _ := out["errors"].([]any); len(errs) != 1 ||
		!strings.Contains(fmt.Sprint(errs[0]), "已禁止插入") ||
		!strings.Contains(fmt.Sprint(errs[0]), "[subdomain] "+blockedDomain) {
		t.Fatalf("gated insert errors = %#v", out)
	}
	if res, _ := out["results"].([]any); len(res) != 0 {
		t.Fatalf("gated insert must not create assets: %#v", out)
	}

	// list_assets：无参拒绝、id、ids、DSL、DSL 报错
	if text, isErr = callToolResult(t, ts.listAssets(), map[string]any{}); !isErr || !strings.Contains(text, "dsl 不能为空") {
		t.Fatalf("unfiltered list_assets = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.listAssets(), map[string]any{"id": rootID})
	if isErr || !strings.Contains(text, `"count":1`) {
		t.Fatalf("list_assets by id = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.listAssets(), map[string]any{"ids": []any{rootID, 0}})
	if isErr || !strings.Contains(text, `"count":1`) {
		t.Fatalf("list_assets by ids = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.listAssets(), map[string]any{"dsl": "domain=" + tag + ".owner.example", "type": "root_domain"})
	if isErr || !strings.Contains(text, tag+".owner.example") {
		t.Fatalf("list_assets by dsl = %q (err=%v)", text, isErr)
	}
	if text, isErr = callToolResult(t, ts.listAssets(), map[string]any{"dsl": "(domain==x"}); !isErr || !strings.Contains(text, "DSL 错误") {
		t.Fatalf("bad dsl = %q (err=%v)", text, isErr)
	}

	// list_companies：全量、大小写不敏感过滤、无命中
	company := tag + "-Corp"
	companyID, _, err := d.Companies().UpsertCompany(company, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM companies WHERE id=$1`, companyID) }()
	if added, _, _, msgs := d.Companies().AddScope(companyID, []string{tag + "-corp.example"}, "batch32"); added != 1 || len(msgs) != 0 {
		t.Fatalf("company scope add = added:%d errors:%v", added, msgs)
	}
	text, isErr = callToolResult(t, ts.listCompanies(), map[string]any{"search": strings.ToLower(company)})
	if isErr {
		t.Fatalf("list_companies = %q", text)
	}
	out = decodeToolMap(t, text)
	if out["count"].(float64) != 1 || !strings.Contains(text, tag+"-corp.example") {
		t.Fatalf("list_companies filtered = %#v", out)
	}
	text, isErr = callToolResult(t, ts.listCompanies(), map[string]any{"search": "no-such-company-batch32"})
	if isErr || !strings.Contains(text, `"count":0`) {
		t.Fatalf("list_companies miss = %q (err=%v)", text, isErr)
	}

	// add_company_scope：空公司拒绝；范围含非法网段 → invalid + errors
	if text, isErr = callToolResult(t, ts.addCompanyScope(), map[string]any{"company": "  "}); !isErr || !strings.Contains(text, "company 不能为空") {
		t.Fatalf("empty company = %q (err=%v)", text, isErr)
	}
	text, isErr = callToolResult(t, ts.addCompanyScope(), map[string]any{
		"company": tag + "-scope-corp",
		"scope":   tag + "-scope.example\n0.0.0.0/0",
		"reason":  "whois",
	})
	if isErr {
		t.Fatalf("add_company_scope = %q", text)
	}
	out = decodeToolMap(t, text)
	if out["company_id"] == nil || out["added"] == nil || out["invalid"] == nil {
		t.Fatalf("add_company_scope = %#v", out)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM companies WHERE name=$1`, tag+"-scope-corp") }()
	if out["errors"] == nil {
		t.Logf("wide cidr accepted as %#v", out)
	}
}

// =====================================================================
// 资产候选/标签的纯函数分支
// =====================================================================
func TestAssetCandidateLabels(t *testing.T) {
	domains, ips, urls := assetInterceptCandidates(assetInputItem{
		Domain:       " a.example ",
		BoundDomains: []string{" b.example ", "  "},
		IP:           "10.0.0.1",
		ServiceIP:    "10.0.0.2",
		URL:          "https://c.example:8443/x",
	})
	if strings.Join(domains, ",") != "a.example,b.example,c.example" {
		t.Fatalf("domains = %v", domains)
	}
	if strings.Join(ips, ",") != "10.0.0.1,10.0.0.2" {
		t.Fatalf("ips = %v", ips)
	}
	if len(urls) != 1 || urls[0] != "https://c.example:8443/x" {
		t.Fatalf("urls = %v", urls)
	}
	_, ipHost, _ := assetInterceptCandidates(assetInputItem{URL: "http://10.1.2.3:8080/p"})
	if len(ipHost) != 1 || ipHost[0] != "10.1.2.3" {
		t.Fatalf("url ip host = %v", ipHost)
	}

	cases := []struct {
		item assetInputItem
		want string
	}{
		{assetInputItem{Type: "root_domain", Domain: " d.example "}, "[root_domain] d.example"},
		{assetInputItem{Type: "service", URL: "https://e.example/"}, "[service] https://e.example/"},
		{assetInputItem{Type: "ip", IP: "10.0.0.3"}, "[ip] 10.0.0.3"},
		{assetInputItem{Type: "service", ServiceIP: "10.0.0.4"}, "[service] 10.0.0.4"},
		{assetInputItem{Type: "endpoint"}, "[endpoint] (未知)"},
		{assetInputItem{Domain: "f.example"}, "f.example"},
	}
	for _, c := range cases {
		if got := assetInputLabel(c.item); got != c.want {
			t.Errorf("label(%+v) = %q, want %q", c.item, got, c.want)
		}
	}
}

// =====================================================================
// graph_overview：覆盖度未锚定、禁用开关与最近漏洞/事实窗口
// =====================================================================
func TestGraphOverviewCoverageAndWindows(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tag := fmt.Sprintf("batch32-ov-%d", time.Now().UnixNano())
	task, err := d.CreateTask(tag, "overview windows", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()

	ts := NewToolSet(d.Exploration(task.ExplorationID), "fixture")
	ts.taskID = task.ID
	ts.SetAssetStore(d.Assets(), d.Companies())

	// 无 scope → denominator 为 0 → pct 为 nil + status
	overview := ts.graphOverviewData()
	cov, _ := overview["coverage"].(map[string]any)
	if cov == nil || cov["denominator"].(int) != 0 || cov["pct"] != nil || cov["status"] != "范围未锚定" {
		t.Fatalf("unanchored coverage = %#v", cov)
	}
	// 关闭覆盖度 → 只保留 host_count，不再带 denominator
	ts.SetCoverageEnabled(false)
	overview = ts.graphOverviewData()
	cov, _ = overview["coverage"].(map[string]any)
	if _, has := cov["denominator"]; has {
		t.Fatalf("coverage-disabled overview leaked denominator: %#v", cov)
	}
	ts.SetCoverageEnabled(true)

	// running 意图 + 由意图产生的 finding/fact（from_intent/confidence）
	intent, err := ts.ts.AddIntent(map[string]any{"summary": "running work"}, 7, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ts.SetNodeState(intent, "running"); err != nil {
		t.Fatal(err)
	}
	fact, err := ts.ts.AddNode(db.KindFact, map[string]any{"summary": "observed thing", "confidence": "inferred"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ts.Link(intent, db.RelYields, fact); err != nil {
		t.Fatal(err)
	}
	overview = ts.graphOverviewData()
	running, _ := overview["running_intents"].([]map[string]any)
	if len(running) == 0 {
		t.Fatalf("running intent missing: %#v", overview["running_intents"])
	}
	recent, _ := overview["recent_facts"].([]map[string]any)
	var sawFromIntent, sawConfidence bool
	for _, m := range recent {
		if from, ok := m["from_intent"].(int64); ok && from == intent {
			sawFromIntent = true
		}
		if m["confidence"] == "inferred" {
			sawConfidence = true
		}
	}
	if !sawFromIntent || !sawConfidence {
		t.Fatalf("recent_facts annotations = %#v", recent)
	}
}
