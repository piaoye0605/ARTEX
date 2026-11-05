package guard

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// --- pure, no-DB tests -------------------------------------------------------

// The audit log must capture the shell-command surface of the gated call:
// Bash/shell_open carry "command", shell_send carries "text", everything else
// records an empty command. This surface is what the history page shows.
func TestCommandExtractionInAudit(t *testing.T) {
	g := New()
	h := g.Hooks()
	ctx := context.Background()

	mustJSON := func(m map[string]string) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	_, _, _ = h.PreToolUse(ctx, "Bash", mustJSON(map[string]string{"command": "nmap -sV 10.0.0.1"}))
	_, _, _ = h.PreToolUse(ctx, "shell_open", mustJSON(map[string]string{"command": "psql -h db"}))
	_, _, _ = h.PreToolUse(ctx, "shell_send", mustJSON(map[string]string{"text": "select 1"}))
	_, _, _ = h.PreToolUse(ctx, "WebFetch", mustJSON(map[string]string{"url": "https://x"}))
	// Malformed input must not panic; the command surfaces as empty.
	_, _, _ = h.PreToolUse(ctx, "Bash", []byte("{not-json"))

	audit := g.Audit()
	if len(audit) != 5 {
		t.Fatalf("audit len=%d want 5", len(audit))
	}
	wantCmd := []string{"nmap -sV 10.0.0.1", "psql -h db", "select 1", "", ""}
	for i, w := range wantCmd {
		if audit[i].Command != w {
			t.Errorf("entry %d command=%q want %q", i, audit[i].Command, w)
		}
		if audit[i].Action != "allow" {
			t.Errorf("entry %d action=%q want allow", i, audit[i].Action)
		}
	}
	for i, w := range []string{"Bash", "shell_open", "shell_send", "WebFetch", "Bash"} {
		if audit[i].Tool != w {
			t.Errorf("entry %d tool=%q want %q", i, audit[i].Tool, w)
		}
	}
}

// Observer (G5) failure attribution: only Bash results are classified, and the
// blocked regex wins over IsError.
func TestPostToolUseAttributionClasses(t *testing.T) {
	g := New()
	h := g.Hooks()
	ctx := context.Background()

	h.PostToolUse(ctx, "Bash", nil, []byte("curl: (7) HTTP 403 Forbidden by WAF"), false)
	h.PostToolUse(ctx, "Bash", nil, []byte("rate limit exceeded, captcha required"), false)
	h.PostToolUse(ctx, "Bash", nil, []byte("connection refused"), true)
	h.PostToolUse(ctx, "Bash", nil, []byte("22/tcp open ssh"), false)
	h.PostToolUse(ctx, "WebFetch", nil, []byte("HTTP 403"), false)
	h.PostToolUse(ctx, "Bash", nil, []byte("429 too many requests"), true) // blocked wins over error

	got := g.Attributions()
	if got["blocked"] != 3 || got["error"] != 1 || got["ok"] != 1 {
		t.Fatalf("attributions=%v want blocked=3 error=1 ok=1", got)
	}

	// Attributions returns a snapshot: mutating the returned map must not
	// affect the guard's own counters.
	got["blocked"] = 999
	if g.Attributions()["blocked"] != 3 {
		t.Fatal("Attributions leaked internal state")
	}
}

// The audit log is a ring capped at 2000 entries, keeping the newest.
func TestAuditRingCap(t *testing.T) {
	g := New()
	for i := 0; i < 2100; i++ {
		g.record("Bash", "allow", "", fmt.Sprintf("cmd-%d", i))
	}
	audit := g.Audit()
	if len(audit) != 2000 {
		t.Fatalf("audit len=%d want 2000", len(audit))
	}
	if audit[0].Command != "cmd-100" || audit[1999].Command != "cmd-2099" {
		t.Fatalf("ring kept wrong slice: first=%q last=%q", audit[0].Command, audit[1999].Command)
	}
}

func TestSystemBlockMessageFraming(t *testing.T) {
	msg := systemBlockMessage("命中规则 D4")
	for _, want := range []string{"平台管控", "非目标防御", "命中规则 D4", "此操作被禁止"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q: %s", want, msg)
		}
	}
}

// --- DB-backed tests ---------------------------------------------------------

// TestMain takes the shared advisory lock so guard's DB mutations serialize
// against the db/agent/evidence/server suites during parallel `go test ./...`.
func TestMain(m *testing.M) {
	dsn, _, err := db.DSN()
	if err != nil {
		// No DB configured — DB-backed tests skip themselves.
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if conn.Ping() != nil {
		os.Exit(m.Run())
	}
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}

// dbFixture opens the shared DB and skips the test when no DSN is configured
// or postgres is unreachable.
func dbFixture(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("ARTEX_PG_DSN not configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	return d
}

// probeTool returns a tool name unique to this run. Every rule created by the
// test matches only this name, so concurrent/shared rules can never interfere.
func probeTool() string {
	return fmt.Sprintf("guard_probe_%d", time.Now().UnixNano())
}

// withToolsOnly swaps the global intercept_enabled_tools setting to exactly
// [tool] and restores the previous effective list afterwards.
func withToolsOnly(t *testing.T, ic *intercept.Interceptor, tool string) {
	t.Helper()
	prev, err := ic.GetEnabledTools()
	if err != nil {
		t.Fatalf("read enabled tools: %v", err)
	}
	if err := ic.SetEnabledTools([]string{tool}); err != nil {
		t.Fatalf("set enabled tools: %v", err)
	}
	ic.Invalidate()
	t.Cleanup(func() {
		_ = ic.SetEnabledTools(prev)
		ic.Invalidate()
	})
}

// denyRuleFor creates a deny rule matching the probe tool by name.
func denyRuleFor(t *testing.T, d *db.DB, tool, message string) {
	t.Helper()
	r, err := d.CreateInterceptRule("guard-deny-"+tool, "tool_name", "regex", "^"+tool+"$", "deny", message, 100, true, false, 0, "")
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(r.ID) })
}

// saveSettings snapshots judge-related settings and restores them (an absent
// key is restored as empty, which every reader treats as unset).
func saveSettings(t *testing.T, d *db.DB, keys ...string) {
	t.Helper()
	type saved struct {
		key, val string
		existed  bool
	}
	var snap []saved
	for _, key := range keys {
		v, ok, _ := d.GetSetting(key)
		snap = append(snap, saved{key, v, ok})
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

// A deny rule turns the hook into a hard block whose model-facing message is
// framed as platform governance (not target defense), and the block is
// recorded in the audit log after the allow entry.
func TestGuardDenyRuleBlocks(t *testing.T) {
	d := dbFixture(t)
	tool := probeTool()
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)
	withToolsOnly(t, ic, tool)
	denyRuleFor(t, d, tool, "渗透平台禁止此操作")
	ic.Invalidate()

	input, _ := json.Marshal(map[string]string{"command": "echo hi"})
	blocked, msg, _ := g.Hooks().PreToolUse(context.Background(), tool, input)
	if !blocked {
		t.Fatal("deny rule did not block")
	}
	if !strings.Contains(msg, "平台管控") || !strings.Contains(msg, "渗透平台禁止此操作") {
		t.Fatalf("block message not framed as platform governance: %q", msg)
	}
	entries := g.Audit()
	if len(entries) != 2 || entries[0].Action != "allow" || entries[1].Action != "block" {
		t.Fatalf("audit=%+v want allow then block", entries)
	}
}

// An allow rule keeps the call going and records no block.
func TestGuardAllowRulePasses(t *testing.T) {
	d := dbFixture(t)
	tool := probeTool()
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)
	withToolsOnly(t, ic, tool)
	r, err := d.CreateInterceptRule("guard-allow-"+tool, "tool_name", "regex", "^"+tool+"$", "allow", "放行探针", 100, true, false, 0, "")
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(r.ID) })
	ic.Invalidate()

	input, _ := json.Marshal(map[string]string{"command": "echo hi"})
	if blocked, msg, _ := g.Hooks().PreToolUse(context.Background(), tool, input); blocked || msg != "" {
		t.Fatalf("allow rule blocked: %q", msg)
	}
	for _, e := range g.Audit() {
		if e.Action == "block" {
			t.Fatalf("allow rule recorded a block: %+v", e)
		}
	}
}

// A tool outside intercept_enabled_tools bypasses rule evaluation entirely,
// even when a matching deny rule exists.
func TestGuardDisabledToolPassesThrough(t *testing.T) {
	d := dbFixture(t)
	tool := probeTool()
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)
	// Keep the probe tool OUT of the enabled set on purpose.
	withToolsOnly(t, ic, "not-a-real-tool")
	denyRuleFor(t, d, tool, "不应生效")
	ic.Invalidate()

	input, _ := json.Marshal(map[string]string{"command": "echo hi"})
	if blocked, _, _ := g.Hooks().PreToolUse(context.Background(), tool, input); blocked {
		t.Fatal("disabled tool must bypass rules")
	}
}

// ask + per-rule timeout: when nobody decides within the window the rule's
// timeout action is applied (deny → block, allow → pass).
func TestGuardAskTimeout(t *testing.T) {
	d := dbFixture(t)
	for _, tc := range []struct {
		name      string
		action    string // rule timeout_action
		wantBlock bool
		wantMsg   string
	}{
		{"timeout deny", "deny", true, "人工审批"},
		{"timeout allow", "allow", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := probeTool()
			ic := intercept.New(d)
			g := NewWithInterceptor(ic)
			withToolsOnly(t, ic, tool)
			r, err := d.CreateInterceptRule("guard-ask-"+tool, "tool_name", "regex", "^"+tool+"$",
				"ask", "需要审批", 100, true, true, 1, tc.action)
			if err != nil {
				t.Fatalf("create rule: %v", err)
			}
			t.Cleanup(func() { _ = d.DeleteInterceptRule(r.ID) })
			ic.Invalidate()

			input, _ := json.Marshal(map[string]string{"command": "echo hi"})
			blocked, msg, _ := g.Hooks().PreToolUse(context.Background(), tool, input)
			if blocked != tc.wantBlock {
				t.Fatalf("blocked=%v msg=%q", blocked, msg)
			}
			if tc.wantBlock && !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("message missing %q: %s", tc.wantMsg, msg)
			}
		})
	}
}

// ask while the worker context is already cancelled must block immediately —
// no pending record, no waiting for a decision that can never come.
func TestGuardAskCancelledContext(t *testing.T) {
	d := dbFixture(t)
	tool := probeTool()
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)
	withToolsOnly(t, ic, tool)
	r, err := d.CreateInterceptRule("guard-ask-"+tool, "tool_name", "regex", "^"+tool+"$",
		"ask", "需要审批", 100, true, false, 0, "")
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(r.ID) })
	ic.Invalidate()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	blocked, msg, _ := g.Hooks().PreToolUse(ctx, tool, []byte(`{"command":"echo hi"}`))
	if !blocked {
		t.Fatal("cancelled context must block the ask")
	}
	if !strings.Contains(msg, "工作已取消") {
		t.Fatalf("message missing cancellation reason: %s", msg)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancelled ask took %v — it must not wait for approval", elapsed)
	}
}

// The LLM fallback judge: with judging off the reviewer is never consulted;
// a deny verdict blocks; a reviewer error falls back to the configured fail
// action (allow by default, deny when the strategy says so).
func TestGuardReviewerVerdicts(t *testing.T) {
	d := dbFixture(t)
	saveSettings(t, d, "llm_judge_enabled", "llm_judge_fail_action")

	tool := probeTool()
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)
	withToolsOnly(t, ic, tool)
	ic.Invalidate()
	input, _ := json.Marshal(map[string]string{"command": "echo hi"})
	call := func() (bool, string) {
		b, m, _ := g.Hooks().PreToolUse(context.Background(), tool, input)
		return b, m
	}

	// Judge disabled (default): the reviewer is wired but never consulted.
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, in intercept.ReviewInput) (intercept.Decision, error) {
		return intercept.Decision{Action: "deny", Message: "模型拒绝"}, nil
	})
	if blocked, msg := call(); blocked {
		t.Fatalf("disabled judge must not block: %s", msg)
	}

	// Judge enabled + reviewer denies → block carrying the model's message.
	if err := d.SetSetting("llm_judge_enabled", "true"); err != nil {
		t.Fatalf("enable judge: %v", err)
	}
	blocked, msg := call()
	if !blocked || !strings.Contains(msg, "模型拒绝") {
		t.Fatalf("reviewer deny not applied: blocked=%v msg=%q", blocked, msg)
	}

	// Reviewer error → configured fail action. Default is allow.
	ic.SetReviewer(func(ctx context.Context, profileID int64, prompt string, in intercept.ReviewInput) (intercept.Decision, error) {
		return intercept.Decision{}, fmt.Errorf("model down")
	})
	if blocked, msg := call(); blocked {
		t.Fatalf("fail-action allow must pass: %s", msg)
	}

	// Flip the failure strategy to deny and the same error now blocks.
	if err := d.SetSetting("llm_judge_fail_action", "deny"); err != nil {
		t.Fatalf("set fail action: %v", err)
	}
	if blocked, msg := call(); !blocked || !strings.Contains(msg, "模型审批失败") {
		t.Fatalf("fail-action deny not applied")
	}
}
