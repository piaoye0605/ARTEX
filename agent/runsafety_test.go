package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// ---------- cancelcause.go ----------

func TestAbortCausefFormatting(t *testing.T) {
	c := Causef("killed", "用户杀掉了它", "work #%d 因 %s 被杀", 42, "测试")
	if c.Code != "killed" || c.Short != "用户杀掉了它" || c.Error() != "work #42 因 测试 被杀" {
		t.Fatalf("Causef formatting broken: %+v", c)
	}
}

func TestAbortReasonBranches(t *testing.T) {
	// No cancellation at all → not ok.
	if _, _, _, ok := AbortReason(context.Background()); ok {
		t.Fatalf("live context must report no abort reason")
	}

	// Named cause → exact code/short/text round-trip.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(AbortKilledByPlanner)
	code, short, text, ok := AbortReason(ctx)
	if !ok || code != "killed_by_planner" || short != AbortKilledByPlanner.Short || text != AbortKilledByPlanner.Text {
		t.Fatalf("named cause round-trip broken: %q/%q/%q/%v", code, short, text, ok)
	}

	// Plain cancel without a cause → the diagnostic branch that asks callers to
	// register the cancellation site.
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	cancel2(nil)
	code2, _, text2, ok2 := AbortReason(ctx2)
	if !ok2 || code2 != "canceled_no_cause" || !strings.Contains(text2, "WithCancelCause") {
		t.Fatalf("causeless cancel branch broken: %q/%q", code2, text2)
	}

	// Deadline exceeded with a named cause → that cause.
	ctx3, cancel3 := context.WithTimeoutCause(context.Background(), time.Millisecond, AbortRunHardTimeout)
	defer cancel3()
	time.Sleep(10 * time.Millisecond)
	if code3, _, _, ok3 := AbortReason(ctx3); !ok3 || code3 != "run_hard_timeout" {
		t.Fatalf("deadline with named cause broken: %q/%v", code3, ok3)
	}

	// Deadline exceeded WITHOUT a cause → the deadline_exceeded branch.
	ctx4, cancel4 := context.WithTimeoutCause(context.Background(), time.Millisecond, nil)
	defer cancel4()
	time.Sleep(10 * time.Millisecond)
	code4, _, _, ok4 := AbortReason(ctx4)
	if !ok4 || code4 != "deadline_exceeded" {
		t.Fatalf("causeless deadline branch broken: %q/%v", code4, ok4)
	}

	// Foreign error cause → "other"; short is the first-line preview, text is full.
	ctx5, cancel5 := context.WithCancelCause(context.Background())
	cancel5(errors.New("boom line one\nboom line two"))
	code5, short5, text5, ok5 := AbortReason(ctx5)
	if !ok5 || code5 != "other" {
		t.Fatalf("foreign-cause code broken: %q/%v", code5, ok5)
	}
	if short5 != "boom line one" {
		t.Fatalf("foreign-cause short must be the first line, got %q", short5)
	}
	if text5 != "boom line one\nboom line two" {
		t.Fatalf("foreign-cause text must keep the full error, got %q", text5)
	}
}

// ---------- taskclock.go ----------

func TestClampMaxDuration(t *testing.T) {
	// No deadline → own budget untouched, nothing clamped.
	if eff, clamped := clampMaxDuration(0, 5*time.Minute); eff != 5*time.Minute || clamped {
		t.Fatalf("no deadline must pass through: %v/%v", eff, clamped)
	}
	if eff, clamped := clampMaxDuration(0, 0); eff != 0 || clamped {
		t.Fatalf("no deadline + no budget → unlimited: %v/%v", eff, clamped)
	}
	// Far deadline: own budget binds (and is not "clamped by the task").
	soon := time.Now().Add(time.Hour).Unix()
	if eff, clamped := clampMaxDuration(soon, 30*time.Minute); eff != 30*time.Minute || clamped {
		t.Fatalf("own budget must bind when deadline is far: %v/%v", eff, clamped)
	}
	if _, clamped := clampMaxDuration(soon, 0); !clamped {
		t.Fatalf("unlimited own budget with a deadline → deadline binds")
	}
	// Near deadline: task deadline binds.
	near := time.Now().Add(10 * time.Minute).Unix()
	if eff, clamped := clampMaxDuration(near, 30*time.Minute); eff <= 9*time.Minute || eff >= 11*time.Minute || !clamped {
		t.Fatalf("near deadline must clamp the budget: %v/%v", eff, clamped)
	}
	// Past deadline: floored at 1s so harness never reads 0 as "unlimited".
	past := time.Now().Add(-time.Hour).Unix()
	if eff, clamped := clampMaxDuration(past, 30*time.Minute); eff != time.Second || !clamped {
		t.Fatalf("past deadline must floor at 1s: %v/%v", eff, clamped)
	}
}

func TestTaskClockContextRoundTrip(t *testing.T) {
	if tc := taskClockFrom(context.Background()); tc.DeadlineUnix != 0 || tc.Final {
		t.Fatalf("missing clock must read as zero value")
	}
	want := TaskClock{DeadlineUnix: 12345, Final: true}
	if tc := taskClockFrom(WithTaskClock(context.Background(), want)); tc != want {
		t.Fatalf("clock round-trip broken: %+v", tc)
	}
}

// ---------- wrapup.go ----------

func TestWrapupDefaults(t *testing.T) {
	for _, key := range []string{"worker", "planner", "mainagent"} {
		if strings.TrimSpace(WrapupDefault(key)) == "" {
			t.Fatalf("%s must have a built-in wrap-up prompt", key)
		}
	}
	if got := WrapupDefault("custom-agent"); got != genericWrapUpDefault {
		t.Fatalf("unknown key must fall back to the generic prompt")
	}
	if WrapupTurnsDefault("worker") != 10 || WrapupTurnsDefault("nobody") != genericWrapupTurns {
		t.Fatalf("turn defaults broken")
	}
	if TaskTimeoutWrapupDefault("planner") == "" || TaskTimeoutWrapupDefault("worker") == "" {
		t.Fatalf("worker/planner must have task-timeout defaults")
	}
	if TaskTimeoutWrapupDefault("mainagent") != "" {
		t.Fatalf("mainagent has no task-timeout word by design")
	}
}

// withWrapupOverrides swaps the DB-override hooks for the test and restores them
// afterwards; these are package globals so tests must run sequentially (they do —
// no t.Parallel in this package).
func withWrapupOverrides(t *testing.T) {
	t.Helper()
	oldPrompt, oldTurns := WrapupOverride, WrapupMaxTurnsOverride
	oldTT, oldTTTurns := WrapupTaskTimeoutOverride, WrapupTaskTimeoutTurnsOverride
	t.Cleanup(func() {
		WrapupOverride, WrapupMaxTurnsOverride = oldPrompt, oldTurns
		WrapupTaskTimeoutOverride, WrapupTaskTimeoutTurnsOverride = oldTT, oldTTTurns
	})
}

func TestResolveWrapupOverride(t *testing.T) {
	withWrapupOverrides(t)
	def := WrapupDefault("planner")
	if resolveWrapup("planner") != def {
		t.Fatalf("no hook → built-in default")
	}
	WrapupOverride = func(string) (string, bool) { return "  ", true }
	if resolveWrapup("planner") != def {
		t.Fatalf("blank override must be ignored (default kept)")
	}
	WrapupOverride = func(string) (string, bool) { return "custom prompt", true }
	if resolveWrapup("planner") != "custom prompt" {
		t.Fatalf("non-empty override must win")
	}
	WrapupOverride = func(string) (string, bool) { return "custom prompt", false }
	if resolveWrapup("planner") != def {
		t.Fatalf("ok=false must fall back to the default")
	}
}

func TestResolveWrapupTurnsOverride(t *testing.T) {
	withWrapupOverrides(t)
	if resolveWrapupTurns("worker") != 10 {
		t.Fatalf("no hook → built-in default 10")
	}
	WrapupMaxTurnsOverride = func(string) (int, bool) { return 0, true }
	if resolveWrapupTurns("worker") != 10 {
		t.Fatalf("non-positive override must be ignored")
	}
	WrapupMaxTurnsOverride = func(string) (int, bool) { return 3, true }
	if resolveWrapupTurns("worker") != 3 {
		t.Fatalf("positive override must win")
	}
}

func TestWrapupSettlementForTask(t *testing.T) {
	withWrapupOverrides(t)
	st := wrapupSettlement("planner", []string{"bash"})
	if st.Prompt != WrapupDefault("planner") || st.MaxTurns != 10 || len(st.DisabledTools) != 1 || st.PromptByReason != nil {
		t.Fatalf("plain settlement broken: %+v", st)
	}
	// clamped=false → pure per-run behavior.
	st2 := wrapupSettlementForTask("planner", nil, false)
	if st2.PromptByReason != nil || st2.MaxTurns != 10 {
		t.Fatalf("unclamped task settlement must degenerate to per-run: %+v", st2)
	}
	// clamped=true → timeout reason picks the task-timeout word.
	st3 := wrapupSettlementForTask("planner", nil, true)
	if st3.PromptByReason == nil {
		t.Fatalf("clamped settlement must pick by reason")
	}
	if st3.PromptByReason[harness.ReasonTimeout] != TaskTimeoutWrapupDefault("planner") {
		t.Fatalf("timeout reason must use the task-timeout word")
	}
	if st3.PromptByReason[harness.ReasonMaxTurns] != WrapupDefault("planner") {
		t.Fatalf("max-turns reason must keep the per-run word")
	}
	WrapupTaskTimeoutTurnsOverride = func(string) (int, bool) { return 4, true }
	st4 := wrapupSettlementForTask("planner", nil, true)
	if st4.MaxTurns != 4 {
		t.Fatalf("task-timeout turn override must apply when clamped: %d", st4.MaxTurns)
	}
	// clamped with a task-timeout word available nowhere (custom agent) → no
	// PromptByReason, since resolveTaskTimeoutWrapup is empty.
	st5 := wrapupSettlementForTask("mainagent", nil, true)
	if st5.PromptByReason != nil {
		t.Fatalf("agents without a task-timeout word must not build PromptByReason")
	}
	WrapupTaskTimeoutOverride = func(string) (string, bool) { return "custom-timeout", true }
	st6 := wrapupSettlementForTask("mainagent", nil, true)
	if st6.PromptByReason == nil || st6.PromptByReason[harness.ReasonTimeout] != "custom-timeout" {
		t.Fatalf("DB task-timeout override must enable PromptByReason")
	}
}

// ---------- constraints.go ----------

func TestConstraintBlock(t *testing.T) {
	if got := constraintBlock(nil); got != "" {
		t.Fatalf("nil store → empty block, got %q", got)
	}
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("constraints block", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)
	if got := constraintBlock(ts); got != "" {
		t.Fatalf("no constraints → empty block, got %q", got)
	}
	if _, err := ts.AddConstraint("deny", "禁止触碰 6379 端口", "human"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AddConstraint("allow", "仅限被动侦察 10.0.0.0/8", "human"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AddConstraint("deny", "   ", "human"); err != nil {
		t.Fatal(err)
	}
	got := constraintBlock(ts)
	for _, want := range []string{
		"【操作约束", "允许的操作：", "- 仅限被动侦察 10.0.0.0/8",
		"禁止的操作：", "- 禁止触碰 6379 端口", "out-of-scope",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("constraint block missing %q:\n%s", want, got)
		}
	}
}

// ---------- goals.go ----------

func TestDecomposeGoalsNilProvider(t *testing.T) {
	if out := DecomposeGoals(context.Background(), nil, "dir", "goal", "", nil, nil, 0, nil); out != nil {
		t.Fatalf("nil provider must yield nil, got %v", out)
	}
}

func TestDecomposeGoalsTextOnlyProvider(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("goals text only", "g", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SETextDelta, Text: "我觉得可以拆两个。"},
			{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		} {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	// The provider never calls set_goals → read-back finds nothing → nil, and the
	// caller falls back to the rule-based split.
	if out := DecomposeGoalsWithProvider(context.Background(), provider, t.TempDir(), "拿到 flag", "", nil, ts, task.ID, true, 0, nil); len(out) != 0 {
		t.Fatalf("text-only provider must produce no goals, got %v", out)
	}
}

func TestDecomposeGoalsWritesViaSetGoals(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("goals via set_goals", "g", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)

	turn := 0
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		turn++
		if turn == 1 {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEToolUseStart, ToolID: "call-1", ToolName: "set_goals"},
				{Type: llm.SEToolInputJSON, Text: `{"goals":[{"text":"拿到 flag","vulnclass":"WEB"},{"text":"拿到内网权限"}]}`},
				{Type: llm.SEMessageDelta, StopReason: "tool_use"},
			} {
				if !yield(ev, nil) {
					return
				}
			}
			return
		}
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SETextDelta, Text: "已提交目标。"},
			{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		} {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	out := DecomposeGoalsWithProvider(context.Background(), provider, t.TempDir(), "拿到 flag 并拿到内网权限", "", nil, ts, task.ID, true, 0, nil)
	// ListByKind is newest-first; match by content, not order.
	if len(out) != 2 {
		t.Fatalf("set_goals write-back must surface 2 GoalSpecs, got %+v", out)
	}
	byText := map[string]GoalSpec{}
	for _, g := range out {
		byText[g.Text] = g
	}
	g1, ok1 := byText["拿到 flag"]
	g2, ok2 := byText["拿到内网权限"]
	if !ok1 || !ok2 || g1.VulnClass != "WEB" || g2.VulnClass != "" {
		t.Fatalf("goal specs mismatch: %+v", out)
	}
	goals, err := ts.ListByKind(db.KindGoal, 100)
	if err != nil || len(goals) != 2 {
		t.Fatalf("goal nodes persisted in store: %d %v", len(goals), err)
	}
}
