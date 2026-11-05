package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestRenderPlannerTodos(t *testing.T) {
	if got := renderPlannerTodos(nil); got != "" {
		t.Fatalf("no todos must render empty, got %q", got)
	}
	items := []actool.Todo{
		{Content: "先拿 shell", Status: actool.TodoCompleted},
		{Content: "提权到 root", Status: actool.TodoInProgress},
		{Content: "翻数据库", Status: actool.TodoPending},
		{Content: "未知状态", Status: "weird"},
	}
	got := renderPlannerTodos(items)
	for _, want := range []string{"✔ 先拿 shell", "▶ 提权到 root", "☐ 翻数据库", "☐ 未知状态", "【你的规划待办", "据此推进"} {
		if !strings.Contains(got, want) {
			t.Fatalf("todo render missing %q:\n%s", want, got)
		}
	}
}

func TestTruncOutput(t *testing.T) {
	if got := truncOutput("short", 800); got != "short" {
		t.Fatalf("short text must pass through, got %q", got)
	}
	long := strings.Repeat("长", 900)
	got := truncOutput(long, 800)
	if !strings.HasPrefix(got, strings.Repeat("长", 800)) {
		t.Fatalf("long text must keep the first 800 runes, got head=%q", []rune(got)[:820])
	}
	if !strings.HasSuffix(got, " …（已截断，完整见 get_worker_output）") {
		t.Fatalf("truncation hint missing, tail=%q", string([]rune(got)[780:]))
	}
}

func TestRenderGraphOverview(t *testing.T) {
	got := renderGraphOverview(map[string]any{"task": "demo", "facts": 3})
	if !strings.Contains(got, "【本轮态势") || !strings.Contains(got, `"facts":3`) {
		t.Fatalf("overview render broken: %q", got)
	}
	// Unmarshalable data (channel) must fall back to "" so the model calls the
	// tool itself instead of crashing the round.
	if got := renderGraphOverview(map[string]any{"bad": make(chan int)}); got != "" {
		t.Fatalf("unmarshalable overview must render empty, got %q", got)
	}
}

func TestPlannerResolverPlumbing(t *testing.T) {
	p := NewPlanner(nil, "model-x", "/tmp/work", nil, 4096, 7)
	if p.compactionWindow() != 4096 {
		t.Fatalf("window default must fall back to static value")
	}
	p.SetCompactionWindowResolver(func() int { return 1234 })
	if p.compactionWindow() != 1234 {
		t.Fatalf("windowFn must take precedence")
	}
	if p.nonStreaming() {
		t.Fatalf("unset resolver must default to streaming")
	}
	p.SetNonStreaming(func() bool { return true })
	if !p.nonStreaming() {
		t.Fatalf("resolver must be read per run")
	}
	if p.maxTokens() != 0 {
		t.Fatalf("unset maxTokens must be 0")
	}
	p.SetMaxTokens(func() int { return 512 })
	if p.maxTokens() != 512 {
		t.Fatalf("maxTokens resolver broken")
	}
	if !p.wantConstraints() {
		t.Fatalf("constraint injection defaults to on")
	}
	p.SetConstraintInject(func() bool { return false })
	if p.wantConstraints() {
		t.Fatalf("constraint resolver must be honored")
	}
	p.SetProxy("127.0.0.1:8888", "/tmp/ca.pem")
	if p.proxyAddr != "127.0.0.1:8888" || p.proxyCACert != "/tmp/ca.pem" {
		t.Fatalf("proxy wiring broken")
	}
	p.SetWebSearch(WebSearchOpts{Enabled: true, Backend: "ddgs"})
	if !p.webSearch.Enabled || p.webSearch.Backend != "ddgs" {
		t.Fatalf("web search wiring broken")
	}
	called := 0
	p.SetKillWork(func(int64) error { called++; return nil })
	p.SetSteerWork(func(int64, string) error { called++; return nil })
	if err := p.killWork(5); err != nil || called != 1 {
		t.Fatalf("kill work callback broken: %v %d", err, called)
	}
	if err := p.steerWork(5, "go deeper"); err != nil || called != 2 {
		t.Fatalf("steer work callback broken: %v %d", err, called)
	}
	// todoFor must return the SAME store for the same exploration id (the
	// cross-wake-up plan scratchpad) and a fresh one for another task.
	s1 := p.todoFor(11)
	if s1 != p.todoFor(11) {
		t.Fatalf("todoFor must be stable per exploration")
	}
	if s1 == p.todoFor(22) {
		t.Fatalf("different explorations must not share a todo store")
	}
}

func TestPlannerSettersAndSystemPrompt(t *testing.T) {
	sys := plannerSystem("拿到 flag", "/tmp/data", "/tmp/work/tasks/9")
	if !strings.Contains(sys, "拿到 flag") {
		t.Fatalf("planner system prompt must embed the goal")
	}
	if !strings.Contains(sys, "规划者") || !strings.Contains(sys, "中间产物") {
		t.Fatalf("planner system prompt must carry the planner body and artifact spec tail")
	}
}

func TestRenderTriggersAllKinds(t *testing.T) {
	if got := renderTriggers(nil, nil); got != "" {
		t.Fatalf("no events → empty, got %q", got)
	}
	// Store-dependent kinds need a real store; goal/hint kinds are pure.
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("trigger render", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)
	intentID, err := ts.AddNode(db.KindIntent, map[string]any{"summary": "探测管理后台"}, 0, "done", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := intentID
	if _, err := ts.AppendActivity(db.Activity{NodeID: &nodeID, Worker: "worker", Kind: "result", Summary: "结论", Detail: strings.Repeat("证", 900)}); err != nil {
		t.Fatal(err)
	}
	f1, err := ts.AddNode(db.KindFact, map[string]any{"summary": "f1"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := ts.AddNode(db.KindFact, map[string]any{"summary": "f2"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []int64{f1, f2} {
		if err := ts.Link(intentID, db.RelYields, f); err != nil {
			t.Fatal(err)
		}
	}

	got := renderTriggers(ts, []TriggerEvent{
		{Kind: "goal", Goals: []string{"只测这一个目标"}},
		{Kind: "goal", Goals: []string{"g1", "g2"}},
		{Kind: "hint", Hints: []string{"优先内网"}},
		{Kind: "hint", Hints: []string{"h1", "h2"}},
		{Kind: "goal_deleted", Detail: "旧目标"},
		{Kind: "goal_edited", OldGoal: "旧", NewGoal: "新"},
		{Kind: "finding", IntentID: intentID, Detail: "发现越权"},
		{Kind: "cancelled", IntentID: 999999, Summary: "被删的意图", Detail: "方向重复"},
		{Kind: "done", IntentID: intentID},
	})
	for _, want := range []string{
		"新增了一个目标：只测这一个目标",
		"新增了 2 个目标：g1；g2",
		"新增了一条战略提示：优先内网",
		"新增了 2 条战略提示：h1；h2",
		"人删除了该目标：旧目标",
		"由「旧」变为「新」",
		"#999999", "被删的意图", "方向重复",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trigger render missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "探测管理后台") {
		t.Fatalf("done trigger must carry the intent summary:\n%s", got)
	}
	if !strings.Contains(got, "发现越权") {
		t.Fatalf("finding trigger must carry the detail")
	}
	// Done trigger embeds the worker output (truncated) and yielded fact ids.
	if !strings.Contains(got, "#"+itoa(f1)+"、#"+itoa(f2)) {
		t.Fatalf("done trigger must list yielded fact ids:\n%s", got)
	}
	if !strings.Contains(got, strings.Repeat("证", 800)) || strings.Contains(got, strings.Repeat("证", 900)) {
		t.Fatalf("worker output must be truncated to 800 runes")
	}

	// Empty-string intents render as "?" via intentSummary's best-effort miss.
	if s := intentSummary(ts, 424242); s != "?" {
		t.Fatalf("missing node summary must be \"?\", got %q", s)
	}
	if w := workerOutput(ts, 424242); w != "(该 work 尚无输出记录)" {
		t.Fatalf("missing worker output placeholder, got %q", w)
	}
	if got := renderTriggers(ts, nil); got != "" {
		t.Fatalf("empty event slice → empty")
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func TestIntentSummaryAndWorkerOutput(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("summary output", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)
	intentID, err := ts.AddNode(db.KindIntent, map[string]any{"summary": "一句话意图"}, 0, "done", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := intentSummary(ts, intentID); s != "一句话意图" {
		t.Fatalf("intent summary read broken: %q", s)
	}
	// Intent without a summary field → "?".
	bare, err := ts.AddNode(db.KindIntent, map[string]any{"state": "done"}, 0, "done", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := intentSummary(ts, bare); s != "?" {
		t.Fatalf("summary-less intent must render ?, got %q", s)
	}
	// No activities → placeholder; a result activity → its detail; when detail is
	// empty the summary is used instead.
	if w := workerOutput(ts, intentID); w != "(该 work 尚无输出记录)" {
		t.Fatalf("no-activity placeholder broken: %q", w)
	}
	nid := intentID
	if _, err := ts.AppendActivity(db.Activity{NodeID: &nid, Worker: "worker", Kind: "text", Summary: "文本行", Detail: "文本细节"}); err != nil {
		t.Fatal(err)
	}
	if w := workerOutput(ts, intentID); w != "文本细节" {
		t.Fatalf("text fallback (no result yet) broken: %q", w)
	}
	if _, err := ts.AppendActivity(db.Activity{NodeID: &nid, Worker: "worker", Kind: "result", Summary: "结果摘要", Detail: "完整结果"}); err != nil {
		t.Fatal(err)
	}
	if w := workerOutput(ts, intentID); w != "完整结果" {
		t.Fatalf("result activity must win over text: %q", w)
	}
}

func TestFactIDsYielded(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("fact ids", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)
	intentID, err := ts.AddNode(db.KindIntent, map[string]any{"summary": "i"}, 0, "done", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := factIDsYielded(ts, intentID); got != "" {
		t.Fatalf("no yields → empty, got %q", got)
	}
	f1, err := ts.AddNode(db.KindFact, map[string]any{"summary": "a"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := ts.AddNode(db.KindFact, map[string]any{"summary": "b"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ts.Link(intentID, db.RelYields, f1)
	_ = ts.Link(intentID, db.RelYields, f2)
	got := factIDsYielded(ts, intentID)
	if !strings.HasPrefix(got, "#") || !strings.Contains(got, "、") {
		t.Fatalf("yielded ids must render as #a、#b, got %q", got)
	}
	if strings.Count(got, "#") != 2 {
		t.Fatalf("both facts must be listed: %q", got)
	}
}

// TestPlannerPlanSmoke runs one full planning round over a real exploration with
// a scripted provider: one plain-text turn, no tool calls. The round must
// complete without error, emit exactly one planner-tagged activity, and leave
// the goal unmet.
func TestPlannerPlanSmoke(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("planner smoke", "拿到 flag", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)

	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SETextDelta, Text: "目标尚未达成，暂无未覆盖新方向。"},
			{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		} {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	p := NewPlanner(provider, "test-model", t.TempDir(), nil, 4096, 1)
	p.SetNonStreaming(func() bool { return true })
	p.SetConstraintInject(func() bool { return false })

	var acts []db.Activity
	met, reason, err := p.Plan(context.Background(), task.ID, nil, ts, "拿到 flag", nil, func(a db.Activity) {
		acts = append(acts, a)
	})
	if err != nil {
		t.Fatalf("plan round failed: %v", err)
	}
	if met {
		t.Fatalf("goal must be unmet after a no-tool round")
	}
	if reason != "" {
		t.Fatalf("no prove_goal → empty reason, got %q", reason)
	}
	if len(acts) == 0 {
		t.Fatalf("planner round must emit activity")
	}
	found := false
	for _, a := range acts {
		if a.Worker == "planner" {
			found = true
		}
	}
	if !found {
		t.Fatalf("emitted activities must be tagged planner")
	}
}
