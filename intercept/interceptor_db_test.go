package intercept

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain takes the shared advisory lock so this package's DB mutations
// serialize against the db/agent/evidence/server/guard suites during parallel
// `go test ./...` runs.
func TestMain(m *testing.M) {
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}

// --- context helpers (pure) --------------------------------------------------

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	if got := ConvIDFromContext(ctx); got != 0 {
		t.Fatalf("empty ctx conv=%d", got)
	}
	if got := ConvIDFromContext(WithConvID(ctx, 42)); got != 42 {
		t.Fatalf("conv=%d want 42", got)
	}

	var emitted []db.Activity
	taskCtx := WithTaskContext(ctx, "task-9", "worker-1", func(a db.Activity) { emitted = append(emitted, a) })
	taskID, agent := taskInfoFromCtx(taskCtx)
	if taskID != "task-9" || agent != "worker-1" {
		t.Fatalf("task info=%q/%q", taskID, agent)
	}
	if id, agent := taskInfoFromCtx(ctx); id != "" || agent != "" {
		t.Fatalf("plain ctx info=%q/%q", id, agent)
	}
	if emit := taskEmitFromCtx(ctx); emit != nil {
		t.Fatal("plain ctx must have no emitter")
	}
	emit := taskEmitFromCtx(taskCtx)
	emit(db.Activity{Kind: "x"})
	if len(emitted) != 1 || emitted[0].Kind != "x" {
		t.Fatalf("emit not bound: %+v", emitted)
	}
}

func TestPendingManager(t *testing.T) {
	p := newPendingManager()
	ch := p.add(7)

	// Resolve before receive: the buffered channel keeps the verdict.
	p.resolve(7, true)
	if got := <-ch; !got {
		t.Fatal("resolve lost the verdict")
	}

	// Resolving an unknown/removed id is a no-op, not a panic.
	p.resolve(8, true)
	p.add(9)
	p.remove(9)
	p.resolve(9, true)

	// A second add for the same id replaces the channel: the verdict goes to
	// the live registration, the stale one is never woken.
	p.add(10)
	ch2 := p.add(10)
	p.resolve(10, false)
	select {
	case got := <-ch2:
		if got {
			t.Fatal("wrong verdict delivered")
		}
	case <-time.After(time.Second):
		t.Fatal("verdict never delivered to the live channel")
	}
}

// --- DB-backed engine tests --------------------------------------------------

func icFixture(t *testing.T) (*db.DB, *Interceptor) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("ARTEX_PG_DSN not configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	return d, New(d)
}

// uniqueTool returns a tool name unique to this run so shared rules can never
// interfere with (or be interfered by) these tests.
func uniqueTool() string { return fmt.Sprintf("ic_probe_%d", time.Now().UnixNano()) }

type savedSetting struct {
	key, val string
	existed  bool
}

// snapshotSettings registers a cleanup restoring the given keys. An absent key
// is restored as empty (every reader treats "" as unset).
func snapshotSettings(t *testing.T, d *db.DB, keys ...string) {
	t.Helper()
	var snap []savedSetting
	for _, key := range keys {
		v, ok, _ := d.GetSetting(key)
		snap = append(snap, savedSetting{key, v, ok})
	}
	t.Cleanup(func() {
		for _, s := range snap {
			if s.existed {
				_ = d.SetSetting(s.key, s.val)
			} else {
				_ = d.SetSetting(s.key, "")
			}
		}
	})
}

func TestEnabledToolsLifecycle(t *testing.T) {
	d, ic := icFixture(t)
	snapshotSettings(t, d, "intercept_enabled_tools")

	// No setting saved → the hard-coded default set applies.
	if !ic.IsToolEnabled("Bash") || !ic.IsToolEnabled("web_search") {
		t.Fatal("defaults lost: Bash/web_search must be enabled")
	}
	if ic.IsToolEnabled("CustomNotARealTool") {
		t.Fatal("unknown tool must not be enabled by default")
	}
	def, err := ic.GetEnabledTools()
	if err != nil || len(def) != len(defaultEnabledTools) {
		t.Fatalf("defaults list=%v err=%v", def, err)
	}

	// Saving the setting replaces the whole set.
	if err := ic.SetEnabledTools([]string{"Bash", "ic_only"}); err != nil {
		t.Fatalf("SetEnabledTools: %v", err)
	}
	ic.Invalidate()
	if !ic.IsToolEnabled("ic_only") || ic.IsToolEnabled("web_search") {
		t.Fatal("saved set not applied")
	}
	got, _ := ic.GetEnabledTools()
	if len(got) != 2 {
		t.Fatalf("GetEnabledTools=%v", got)
	}

	// A corrupt setting disables everything rather than crashing.
	_ = d.SetSetting("intercept_enabled_tools", "{not-json")
	ic.Invalidate()
	if ic.IsToolEnabled("Bash") {
		t.Fatal("corrupt setting must disable all tools")
	}
}

func TestMatchRuleEvaluation(t *testing.T) {
	d, ic := icFixture(t)
	stamp := time.Now().UnixNano()
	marker := fmt.Sprintf("ic-marker-%d", stamp)

	// Priority ordering: of the two rules matching the same call, the
	// higher-priority one wins even though the lower one was created later.
	// (All builtin seed rules target tool_input, so tool-name rules with an
	// empty input never collide with them.)
	hi, err := d.CreateInterceptRule(fmt.Sprintf("hi-%d", stamp), "tool_name", "string", "Bash", "deny", "高优先", 90, true, false, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lo, err := d.CreateInterceptRule(fmt.Sprintf("lo-%d", stamp), "tool_name", "string", "Bash", "allow", "", 10, true, false, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Disabled rules never match even at a higher priority.
	off, err := d.CreateInterceptRule(fmt.Sprintf("off-%d", stamp), "tool_name", "string", "Bash", "deny", "停用", 99, false, false, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A rule with an invalid regex is skipped, not fatal.
	bad, err := d.CreateInterceptRule(fmt.Sprintf("bad-%d", stamp), "tool_input", "regex", "(unclosed", "deny", "", 95, true, false, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Input-targeted rule with a unique marker: nothing else can match it.
	mk, err := d.CreateInterceptRule(fmt.Sprintf("mk-%d", stamp), "tool_input", "string", marker, "allow", "", 10, true, false, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, id := range []int64{hi.ID, lo.ID, off.ID, bad.ID, mk.ID} {
		id := id
		t.Cleanup(func() { _ = d.DeleteInterceptRule(id) })
	}
	ic.Invalidate()

	// Both tool-name rules match; priority decides. Custom message passes
	// through; decision metadata is filled in.
	dec, matched := ic.Match("Bash", []byte(`{}`))
	if !matched || dec.Action != "deny" || dec.RuleID != hi.ID {
		t.Fatalf("match=%+v matched=%v want rule %d", dec, matched, hi.ID)
	}
	if dec.Message != "高优先" {
		t.Fatalf("custom message lost: %q", dec.Message)
	}
	if dec.RuleName != hi.Name || dec.ConfigDigest == "" {
		t.Fatalf("decision metadata incomplete: %+v", dec)
	}

	// The input-targeted rule fires on its marker, and an allow rule keeps an
	// empty default message.
	dec, matched = ic.Match("Write", []byte(fmt.Sprintf(`{"body":"%s"}`, marker)))
	if !matched || dec.Action != "allow" || dec.RuleID != mk.ID {
		t.Fatalf("input match=%+v matched=%v", dec, matched)
	}
	if dec.Message != "" {
		t.Fatalf("allow messages stay empty, got %q", dec.Message)
	}

	// No match at all.
	if _, matched := ic.Match("Unknown", []byte(`{"quiet":true}`)); matched {
		t.Fatal("no rule should match")
	}

	// Cache invalidation: deleting the winning rule after it was cached changes
	// nothing until Invalidate runs; the next-priority rule surfaces after.
	_ = d.DeleteInterceptRule(hi.ID)
	if dec, _ := ic.Match("Bash", []byte(`{}`)); dec.RuleID != hi.ID {
		t.Fatalf("stale cache should still return hi rule, got %+v", dec)
	}
	ic.Invalidate()
	if dec, matched := ic.Match("Bash", []byte(`{}`)); !matched || dec.RuleID != lo.ID || dec.Action != "allow" {
		t.Fatalf("post-invalidation match=%+v matched=%v want lo", dec, matched)
	}
}

func TestJudgeVerdicts(t *testing.T) {
	d, ic := icFixture(t)
	snapshotSettings(t, d, "llm_judge_enabled", "llm_judge_profile_id", "llm_judge_prompt",
		"llm_judge_timeout_seconds", "llm_judge_fail_action", "llm_judge_ask_timeout_seconds", "llm_judge_ask_timeout_action")

	input := json.RawMessage(`{"command":"echo probe"}`)
	ctx := context.Background()

	// Disabled (fresh DB default): reviewer wired but not consulted.
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, in ReviewInput) (Decision, error) {
		return Decision{Action: "deny", Message: "不应出现"}, nil
	})
	if dec, judged := ic.Judge(ctx, "Bash", input); judged {
		t.Fatalf("disabled judge produced %+v", dec)
	}

	cfg := JudgeConfig{Enabled: true, FailAction: "allow", AskTimeoutSeconds: 2, AskTimeoutAction: "deny", TimeoutSeconds: 5}
	if err := ic.SetJudgeConfig(cfg); err != nil {
		t.Fatalf("SetJudgeConfig: %v", err)
	}

	// The stored config round-trips; an empty prompt falls back to the template.
	got := ic.GetJudgeConfig()
	if !got.Enabled || got.FailAction != "allow" || got.AskTimeoutSeconds != 2 {
		t.Fatalf("config=%+v", got)
	}
	if got.Prompt != DefaultJudgePrompt {
		t.Fatalf("default prompt lost")
	}

	// A deny verdict is prefixed with [模型] and never claims a rule.
	dec, judged := ic.Judge(ctx, "Bash", input)
	if !judged || dec.Action != "deny" || dec.RuleID != 0 {
		t.Fatalf("deny verdict=%+v judged=%v", dec, judged)
	}
	if !strings.HasPrefix(dec.Message, "[模型] ") || !strings.Contains(dec.Message, "不应出现") {
		t.Fatalf("message framing lost: %q", dec.Message)
	}

	// An ask verdict inherits the ask-timeout pair so HandleAsk can consume it.
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, in ReviewInput) (Decision, error) {
		return Decision{Action: "ask"}, nil
	})
	dec, _ = ic.Judge(ctx, "Bash", input)
	if !dec.TimeoutEnabled || dec.TimeoutSeconds != 2 || dec.TimeoutAction != "deny" {
		t.Fatalf("ask timeout=%+v", dec)
	}

	// Reviewer error → configured fail action with an explanatory message.
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, in ReviewInput) (Decision, error) {
		return Decision{}, errors.New("model down")
	})
	dec, judged = ic.Judge(ctx, "Bash", input)
	if !judged || dec.Action != "allow" || !strings.Contains(dec.Message, "模型审批失败") {
		t.Fatalf("fail-action allow=%+v judged=%v", dec, judged)
	}

	// Unparseable action (empty) → fail action too.
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, in ReviewInput) (Decision, error) {
		return Decision{Action: "maybe"}, nil
	})
	if dec, _ := ic.Judge(ctx, "Bash", input); dec.Action != "allow" {
		t.Fatalf("unparseable verdict handling: %+v", dec)
	}

	// Flip the failure strategy to deny; both failure modes now deny.
	cfg.FailAction = "deny"
	if err := ic.SetJudgeConfig(cfg); err != nil {
		t.Fatalf("SetJudgeConfig: %v", err)
	}
	if dec, _ := ic.Judge(ctx, "Bash", input); dec.Action != "deny" {
		t.Fatalf("fail-action deny: %+v", dec)
	}

	// Invalid JSON arguments cannot be reviewed faithfully → forced ask.
	dec, judged = ic.Judge(ctx, "Bash", json.RawMessage(`{bad`))
	if !judged || dec.Action != "ask" || !strings.Contains(dec.Message, "人工确认") {
		t.Fatalf("invalid input verdict=%+v judged=%v", dec, judged)
	}
}

// HandleAsk + Decide is the human-approval loop: the worker blocks, the pending
// row appears, Decide unblocks it with the chosen outcome.
func TestHandleAskDecideFlow(t *testing.T) {
	d, ic := icFixture(t)

	for _, tc := range []struct {
		name    string
		allowed bool
	}{
		{"human approves", true},
		{"human denies", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := uniqueTool()
			rule, err := d.CreateInterceptRule("guard-ask-"+tool, "tool_name", "string", "^"+tool+"$",
				"ask", "需要审批", 100, true, false, 0, "")
			if err != nil {
				t.Fatalf("create rule: %v", err)
			}
			t.Cleanup(func() { _ = d.DeleteInterceptRule(rule.ID) })

			var emitted []db.Activity
			var mu sync.Mutex
			ctx := WithTaskContext(WithCall(context.Background(), tool, []byte(`{"x":1}`)),
				"task-ic", "worker", func(a db.Activity) { mu.Lock(); emitted = append(emitted, a); mu.Unlock() })

			type outcome struct {
				allowed bool
			}
			done := make(chan outcome, 1)
			go func() {
				dec := Decision{Action: "ask", RuleID: rule.ID, RuleName: rule.Name, Message: "请求审批"}
				allowed := ic.HandleAsk(ctx, 0, dec, tool, []byte(`{"x":1}`))
				done <- outcome{allowed}
			}()

			// Wait for the pending row, then decide from "the UI".
			var pendingID int64
			deadline := time.Now().Add(5 * time.Second)
			for {
				rows, err := d.ListPendingIntercepts()
				if err != nil {
					t.Fatalf("list pending: %v", err)
				}
				for _, r := range rows {
					if r.ToolName == tool {
						pendingID = r.ID
					}
				}
				if pendingID != 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if pendingID == 0 {
				t.Fatal("pending row never appeared")
			}
			if err := ic.Decide(pendingID, tc.allowed); err != nil {
				t.Fatalf("Decide: %v", err)
			}

			select {
			case out := <-done:
				if out.allowed != tc.allowed {
					t.Fatalf("HandleAsk=%v want %v", out.allowed, tc.allowed)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("HandleAsk never returned")
			}

			// The task emitter got exactly one inline card.
			mu.Lock()
			n := len(emitted)
			kind := ""
			if n > 0 {
				kind = emitted[0].Kind
			}
			mu.Unlock()
			if n != 1 || kind != "intercept_request" {
				t.Fatalf("emit=%d kind=%q", n, kind)
			}

			// Deciding the same row twice reports it as already handled.
			if err := ic.Decide(pendingID, tc.allowed); !errors.Is(err, ErrAlreadyDecided) {
				t.Fatalf("second Decide=%v want ErrAlreadyDecided", err)
			}
		})
	}
}

// HandleAsk with a per-rule timeout never blocks forever: the rule's timeout
// action is applied when nobody decides in time.
func TestHandleAskTimeoutAction(t *testing.T) {
	_, ic := icFixture(t)
	for _, tc := range []struct {
		name         string
		timeoutAllow bool
	}{
		{"timeout deny", false},
		{"timeout allow", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := Decision{Action: "ask", TimeoutEnabled: true, TimeoutSeconds: 1}
			if tc.timeoutAllow {
				dec.TimeoutAction = "allow"
			} else {
				dec.TimeoutAction = "deny"
			}
			start := time.Now()
			got := ic.HandleAsk(context.Background(), 0, dec, uniqueTool(), []byte(`{}`))
			if got != tc.timeoutAllow {
				t.Fatalf("HandleAsk=%v want %v", got, tc.timeoutAllow)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("timeout path took %v", elapsed)
			}
		})
	}
}

// Log records an already-decided row for observability; it must not create a
// pending entry nor block. convID 0 (no active conversation) stores NULL.
func TestLogWritesDecidedRow(t *testing.T) {
	d, ic := icFixture(t)
	tool := uniqueTool()
	rule, err := d.CreateInterceptRule("guard-log-"+tool, "tool_name", "string", "^"+tool+"$",
		"deny", "禁止", 100, true, false, 0, "")
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(rule.ID) })
	ctx := WithTaskContext(WithConvID(WithCall(context.Background(), tool, []byte(`{"a":1}`)), 77), "task-log", "worker", nil)
	dec := Decision{Action: "deny", RuleID: rule.ID, RuleName: rule.Name, Message: "禁止", ConfigDigest: "d"}

	ic.Log(ctx, 0, dec, tool, []byte(`{"a":1}`), "denied")

	rows, err := d.ListAllIntercepts(50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ToolName == tool {
			found = true
			if r.Status != "denied" || r.ConversationID != nil {
				t.Fatalf("row=%+v", r)
			}
		}
	}
	if !found {
		t.Fatal("decided row not recorded")
	}
}
