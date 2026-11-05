package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
)

// ---------- mainagent.go / chat.go plumbing ----------

func TestMainAgentPlumbing(t *testing.T) {
	m := NewMainAgent(nil, "model-x", "/tmp/work", nil, 4096, 3)
	if m.compactionWindow() != 4096 {
		t.Fatalf("window default broken")
	}
	m.SetCompactionWindowResolver(func() int { return 256 })
	if m.compactionWindow() != 256 {
		t.Fatalf("window resolver broken")
	}
	if m.nonStreaming() || m.maxTokens() != 0 {
		t.Fatalf("unset resolvers must be streaming / no cap")
	}
	m.SetNonStreaming(func() bool { return true })
	m.SetMaxTokens(func() int { return 777 })
	if !m.nonStreaming() || m.maxTokens() != 777 {
		t.Fatalf("resolvers broken")
	}
	m.SetProxy("127.0.0.1:8888", "/tmp/ca.pem")
	m.SetWebSearch(WebSearchOpts{Enabled: true, Backend: "ddgs"})
	m.SetSteerWork(func(int64, string) error { return nil })
	m.SetFindingRecorder(nil)
	if m.proxyAddr == "" || m.proxyCACert == "" || !m.webSearch.Enabled || m.steerWork == nil {
		t.Fatalf("main agent setters broken")
	}
	sys := mainAgentSystem("拿到 flag", "/tmp/data", "/tmp/work/tasks/3")
	if !strings.Contains(sys, "拿到 flag") || !strings.Contains(sys, "主 agent") || !strings.Contains(sys, "中间产物") {
		t.Fatalf("main agent system prompt broken")
	}
}

func TestChatAgentPlumbing(t *testing.T) {
	c := NewChatAgent(nil, "m", "/tmp/work", nil, 2048)
	if c.nonStreaming() || c.maxTokens() != 0 {
		t.Fatalf("chat defaults broken")
	}
	c.SetNonStreaming(func() bool { return true })
	c.SetNoaEnabled(func() bool { return false })
	c.SetMaxTokens(func() int { return 99 })
	c.SetProxy("http://127.0.0.1:8888", "/ca.pem")
	c.SetWebSearch(WebSearchOpts{Enabled: true})
	c.SetGuard(nil)
	if !c.nonStreaming() || c.maxTokens() != 99 || c.proxyAddr != "http://127.0.0.1:8888" || !c.webSearch.Enabled {
		t.Fatalf("chat setters broken")
	}
	sys := chatSystem("chat-agent", "/data", "/work")
	if !strings.Contains(sys, "文件输出规约") {
		t.Fatalf("chat system prompt must carry the workdir spec")
	}
}

func TestFindingRecorderSetters(t *testing.T) {
	// Pure setter plumbing on every agent flavor — a nil recorder is a valid
	// "feature off" configuration.
	ts := NewToolSet(nil, "")
	ts.SetFindingRecorder(nil)
	if ts.findingRecorder != nil {
		t.Fatalf("toolset recorder setter broken")
	}
	w := NewWorker(nil, "m", "", nil, 0, 1, nil)
	w.SetFindingRecorder(nil)
	p := NewPlanner(nil, "m", "", nil, 0, 1)
	p.SetFindingRecorder(nil)
	m := NewMainAgent(nil, "m", "", nil, 0, 1)
	m.SetFindingRecorder(nil)
}

// ---------- tools.go: write counts / coverage filter / cross-task accessors ----------

func TestWriteCountsTotalAndString(t *testing.T) {
	w := WriteCounts{Facts: 1, Assets: 2, Findings: 3}
	if w.Total() != 6 {
		t.Fatalf("Total broken: %d", w.Total())
	}
	if got := w.String(); got != "事实1 资产2 漏洞3" {
		t.Fatalf("String broken: %q", got)
	}
}

func TestDropCoverageTools(t *testing.T) {
	ts := NewToolSet(nil, "")
	if ts.CoverageDisabled() {
		t.Fatalf("coverage defaults to enabled")
	}
	plannerTools := ts.PlannerTools()
	if got := ts.DropCoverageTools(plannerTools); len(got) != len(plannerTools) {
		t.Fatalf("enabled task must keep all tools")
	}
	ts.SetCoverageEnabled(false)
	if !ts.CoverageDisabled() {
		t.Fatalf("disable flag broken")
	}
	got := ts.DropCoverageTools(plannerTools)
	if len(got) >= len(plannerTools) {
		t.Fatalf("disabled coverage must drop tools: %d → %d", len(plannerTools), len(got))
	}
	for _, tool := range got {
		if tool.Name() == "list_untested_assets" {
			t.Fatalf("coverage-only tool must be dropped when the feature is off")
		}
	}
}

func TestCrossTaskAccessorTools(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("cross task accessors", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	store := d.Exploration(task.ExplorationID)
	intentID, err := store.AddNode(db.KindIntent, map[string]any{"summary": "probe"}, 0, "done", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := intentID
	if _, err := store.AppendActivity(db.Activity{NodeID: &nodeID, Worker: "worker", Kind: "text", Summary: "步骤", Detail: "执行细节"}); err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(store, "planner")
	trace, err := ts.GetWorkerTraceTool().Call(context.Background(), json.RawMessage(`{"intent_id":`+itoa(intentID)+`}`), nil)
	if err != nil {
		t.Fatalf("get_worker_trace accessor broken: %v", err)
	}
	if !strings.Contains(trace.Flatten(), `"summary":"步骤"`) {
		t.Fatalf("trace must include the intent's step list: %s", trace.Flatten())
	}
	overview, err := ts.GraphOverviewTool().Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("graph_overview accessor broken: %v", err)
	}
	if overview.Flatten() == "" {
		t.Fatalf("overview must render")
	}
	findings, err := ts.ListFindingsTool().Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("list_findings accessor broken: %v", err)
	}
	if findings.Flatten() == "" {
		t.Fatalf("findings list must render")
	}
	traces, err := ts.ListWorkerTracesTool().Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("list_worker_traces accessor broken: %v", err)
	}
	if !strings.Contains(traces.Flatten(), "probe") {
		t.Fatalf("worker traces must include the intent: %s", traces.Flatten())
	}
	if _, err := ts.SearchWorkerTracesTool().Call(context.Background(), json.RawMessage(`{"q":"细节"}`), nil); err != nil {
		t.Fatalf("search_worker_traces accessor broken: %v", err)
	}
}

// ---------- noa.go ----------

func TestEnableNoa(t *testing.T) {
	// Off paths: nil resolver and a false resolver must touch nothing.
	opts := agentcore.Options{}
	enableNoa(&opts, nil, t.TempDir(), "sess-a", nil)
	if opts.OnWarn != nil || opts.Compaction != nil {
		t.Fatalf("nil resolver must be a no-op")
	}
	enableNoa(&opts, func() bool { return false }, t.TempDir(), "sess-b", nil)
	if opts.OnWarn != nil {
		t.Fatalf("disabled resolver must be a no-op")
	}
	// On: noa takes over, the built-in compaction is cleared, and the warn sink
	// is installed.
	root := t.TempDir()
	opts.Compaction = compactionConfig(1000)
	var warns []string
	enableNoa(&opts, func() bool { return true }, root, "sess-c", func(msg string) { warns = append(warns, msg) })
	if opts.Compaction != nil {
		t.Fatalf("noa enabled must clear the built-in compaction")
	}
	if opts.OnWarn == nil {
		t.Fatalf("warn sink must be installed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("archive root must remain accessible: %v", err)
	}
}

func TestNoaWarn(t *testing.T) {
	sink := noaWarn("sess-x")
	if sink == nil {
		t.Fatalf("warn sink must be a callable func")
	}
	sink("diagnostic") // must not panic
}

// ---------- provider.go ----------

func TestConfigCompactionWindow(t *testing.T) {
	cases := []struct{ k, want int }{{0, 200000}, {16, 32000}, {2000, 1000000}, {128, 128000}}
	for _, tc := range cases {
		if got := (Config{ContextWindowK: tc.k}).CompactionWindow(); got != tc.want {
			t.Fatalf("ContextWindowK=%d → %d, want %d", tc.k, got, tc.want)
		}
	}
}

func TestConfigProviderName(t *testing.T) {
	if got := (Config{Format: llm.FormatAnthropic}).Provider(); got != "anthropic" {
		t.Fatalf("anthropic name broken: %q", got)
	}
	if got := (Config{Format: llm.FormatOpenAI}).Provider(); got != "openai" {
		t.Fatalf("openai name broken: %q", got)
	}
	if got := (Config{Format: llm.FormatOpenAIResponses}).Provider(); got != "openai-responses" {
		t.Fatalf("responses name broken: %q", got)
	}
}

func TestIsFalsy(t *testing.T) {
	for _, s := range []string{"0", "false", "off", "no", " FALSE ", "\toff\t"} {
		if !isFalsy(s) {
			t.Fatalf("%q must be falsy", s)
		}
	}
	for _, s := range []string{"", "1", "true", "on", "nope"} {
		if isFalsy(s) {
			t.Fatalf("%q must not be falsy", s)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	// Wipe the whole env surface first: empty strings read as unset here.
	for _, k := range []string{"ARTEX_LLM_PROVIDER", "ARTEX_LLM_MODEL", "ARTEX_LLM_BASE_URL", "ARTEX_LLM_PROXY", "ARTEX_LLM_STREAM", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	if _, ok := FromEnv(); ok {
		t.Fatalf("no provider hint → not ok")
	}

	t.Setenv("ANTHROPIC_API_KEY", "k-anthropic")
	c, ok := FromEnv()
	if !ok || c.Format != llm.FormatAnthropic || c.APIKey != "k-anthropic" || !c.Stream {
		t.Fatalf("key inference broken: %+v %v", c, ok)
	}

	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "k-openai")
	t.Setenv("ARTEX_LLM_MODEL", "custom-model")
	t.Setenv("ARTEX_LLM_PROXY", " http://127.0.0.1:8888 ")
	c, ok = FromEnv()
	if !ok || c.Format != llm.FormatOpenAI || c.Model != "custom-model" || c.Proxy != "http://127.0.0.1:8888" {
		t.Fatalf("openai env broken: %+v %v", c, ok)
	}

	t.Setenv("ARTEX_LLM_PROVIDER", "openai-responses")
	t.Setenv("ARTEX_LLM_MODEL", "")
	c, _ = FromEnv()
	if c.Format != llm.FormatOpenAIResponses || c.Model != "gpt-5" {
		t.Fatalf("responses default model broken: %+v", c)
	}

	t.Setenv("ARTEX_LLM_STREAM", "off")
	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	c, _ = FromEnv()
	if c.Stream {
		t.Fatalf("ARTEX_LLM_STREAM=off must disable streaming")
	}
}

func TestQuotaAwareHTTPClient(t *testing.T) {
	if _, err := quotaAwareHTTPClient("", ""); err != nil {
		t.Fatalf("empty proxy must be direct: %v", err)
	}
	if _, err := quotaAwareHTTPClient("  ", ""); err != nil {
		t.Fatalf("blank proxy must be direct: %v", err)
	}
	if _, err := quotaAwareHTTPClient("127.0.0.1", ""); err == nil || !strings.Contains(err.Error(), "missing scheme") {
		t.Fatalf("scheme-less proxy must be rejected, got %v", err)
	}
	if _, err := quotaAwareHTTPClient("127.0.0.1:8888", ""); err == nil {
		t.Fatalf("colon-only proxy must be rejected, got nil")
	}
	if _, err := quotaAwareHTTPClient("ftp://127.0.0.1", ""); err == nil || !strings.Contains(err.Error(), "unsupported proxy scheme") {
		t.Fatalf("unsupported scheme must be rejected, got %v", err)
	}
	if _, err := quotaAwareHTTPClient("http://127.0.0.1:8888", "sess-key"); err != nil {
		t.Fatalf("http proxy must be accepted: %v", err)
	}
	if _, err := quotaAwareHTTPClient("socks5://127.0.0.1:1080", ""); err != nil {
		t.Fatalf("socks5 proxy must be accepted: %v", err)
	}
}

func TestClipBody(t *testing.T) {
	if got := clipBody("   "); got != "(空)" {
		t.Fatalf("blank body → placeholder, got %q", got)
	}
	if got := clipBody("abc"); got != "abc" {
		t.Fatalf("short body passes through, got %q", got)
	}
	long := strings.Repeat("x", 5000)
	got := clipBody(long)
	if !strings.HasPrefix(got, strings.Repeat("x", 4096)) || !strings.Contains(got, "5000") {
		t.Fatalf("long body must clip at 4096 and report total bytes, tail=%q", got[len(got)-30:])
	}
}

func TestLogTestConnectionWithoutAttempts(t *testing.T) {
	ctx, capt := llmrec.NewCapture(context.Background())
	logTestConnection(Config{Model: "m", BaseURL: "http://x"}, capt)
	_ = ctx // capture lives on the ctx; nothing to assert beyond no-panic
}

// ---------- TestConnection over a scripted SSE endpoint ----------

const testSSEOK = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestTestConnectionHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, testSSEOK)
	}))
	defer srv.Close()
	c := Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "claude-test", Stream: true}
	latency, reply, err := TestConnection(context.Background(), c)
	if err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	if reply != "OK" || latency <= 0 {
		t.Fatalf("reply/latency broken: %q %v", reply, latency)
	}
}

func TestTestConnectionEmptyReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1"}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`+"\n\n"+
			"event: message_stop\n"+
			`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()
	c := Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "claude-test", Stream: true}
	_, _, err := TestConnection(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "无回复内容") {
		t.Fatalf("silent model must be a failure, got %v", err)
	}
}

func TestTestConnectionUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // free the port so nothing answers
	c := Config{Format: llm.FormatAnthropic, BaseURL: base, APIKey: "k", Model: "m", Stream: true}
	if _, _, err := TestConnection(context.Background(), c); err == nil {
		t.Fatalf("dead endpoint must fail")
	}
}

// ---------- capture.go pure helpers ----------

func TestActivityWithUsage(t *testing.T) {
	a := db.Activity{Summary: "s"}
	if got := activityWithUsage(a, nil); got.InputTokens != nil || got.OutputTokens != nil {
		t.Fatalf("nil usage must leave activity untouched")
	}
	u := llm.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, CacheWriteTokens: 2}
	got := activityWithUsage(a, &u)
	if *got.InputTokens != 11 || *got.OutputTokens != 7 || *got.CacheReadTokens != 3 || *got.CacheWriteTokens != 2 {
		t.Fatalf("usage not attached: %+v", got)
	}
	if got.Summary != "s" {
		t.Fatalf("other fields must survive: %+v", got)
	}
}

func TestApprovalHistory(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: []llm.ContentBlock{
			{Type: llm.BlockText, Text: "我来试试"},
			{Type: llm.BlockToolUse, ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"id"}`)},
		}},
		{Role: "user", Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolUseID: "t1", IsError: false, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "uid=0"}}},
			{Type: llm.BlockThinking, Thinking: "hidden"},
		}},
	}
	entries := approvalHistory(msgs)
	if len(entries) != 3 {
		t.Fatalf("thinking blocks must be skipped, got %d entries", len(entries))
	}
	if entries[0].Kind != "assistant" || entries[0].Text != "我来试试" {
		t.Fatalf("text entry broken: %+v", entries[0])
	}
	if entries[1].Kind != "tool_use" || entries[1].Tool != "bash" || entries[1].ToolUseID != "t1" || entries[1].Text != `{"command":"id"}` {
		t.Fatalf("tool_use entry broken: %+v", entries[1])
	}
	if entries[2].Kind != "tool_result" || entries[2].ToolUseID != "t1" || entries[2].Text != "uid=0" || entries[2].IsError {
		t.Fatalf("tool_result entry broken: %+v", entries[2])
	}
}

// ---------- main agent Chat smoke ----------

func TestMainAgentChatSmoke(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("chat smoke", "拿到 flag", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	ts := d.Exploration(task.ExplorationID)

	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SETextDelta, Text: "当前还没有任何进展。"},
			{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		} {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	m := NewMainAgent(provider, "test-model", t.TempDir(), nil, 4096, 1)
	m.SetNonStreaming(func() bool { return true })
	var acts []db.Activity
	reply, err := m.Chat(context.Background(), task.ID, 0, nil, ts, "拿到 flag", "现在进展如何？", func(a db.Activity) {
		acts = append(acts, a)
	}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if !strings.Contains(reply, "当前还没有任何进展") {
		t.Fatalf("reply broken: %q", reply)
	}
	found := false
	for _, a := range acts {
		if a.Worker == "mainagent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("chat activities must be tagged mainagent")
	}
}
