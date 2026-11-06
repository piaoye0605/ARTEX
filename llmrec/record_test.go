package llmrec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

// requirePG 打开测试库；未配置 DSN 时跳过，与其它依赖数据库的包一致。
func requirePG(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

// scriptedProvider 按脚本吐事件，用来精确控制 Stream 的走法。
type scriptedProvider struct {
	events []llm.StreamEvent
	err    error // 事件吐完后追加的错误
}

func (p scriptedProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range p.events {
			if !yield(ev, nil) {
				return
			}
		}
		if p.err != nil {
			yield(llm.StreamEvent{}, p.err)
		}
	}
}

func (p scriptedProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "step-1"},
		{Type: llm.BlockThinking, Thinking: ""}, // 空 thinking 块不计入
		llm.TextBlock("final answer"),
	}}, "end_turn", llm.Usage{InputTokens: 11, OutputTokens: 4, CacheReadTokens: 2, CacheWriteTokens: 1}, nil
}

func drain(t *testing.T, seq iter.Seq2[llm.StreamEvent, error]) error {
	t.Helper()
	var last error
	for ev, err := range seq {
		if err != nil {
			last = err
		}
		_ = ev
	}
	return last
}

func TestWithWorkerOverridesSessionLane(t *testing.T) {
	ctx := WithWorker(context.Background(), " judge ")
	if got := workerFrom(ctx); got != "judge" {
		t.Fatalf("workerFrom()=%q want judge", got)
	}
	if got := workerFrom(WithWorker(ctx, "  ")); got != "judge" {
		t.Fatalf("空白 lane 不应覆盖父上下文，得到 %q", got)
	}
	if got := workerFrom(nil); got != "" {
		t.Fatalf("nil ctx 返回 %q", got)
	}
	if got := workerFrom(context.Background()); got != "" {
		t.Fatalf("无 lane 时返回 %q", got)
	}
}

// TestStreamRecordsFullExchange 覆盖"记录正文"的主路径：请求体序列化、响应文本/
// 思考累积、token 折叠、状态与错误落库。
func TestStreamRecordsFullExchange(t *testing.T) {
	pg := requirePG(t)
	profile := "llmrec-body-" + uuid.NewString()

	temp := 0.2
	provider := scriptedProvider{events: []llm.StreamEvent{
		{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 40, CacheReadTokens: 8}},
		{Type: llm.SEThinkingDelta, Text: "why "},
		{Type: llm.SEThinkingDelta, Text: "not"},
		{Type: llm.SETextDelta, Text: "hello "},
		{Type: llm.SETextDelta, Text: "world"},
		{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 12}, StopReason: "end_turn"},
		{Type: llm.SEMessageStop},
	}}
	rec := Wrap(provider, pg, "claude-x", profile, "enabled", "high", func() bool { return true })

	ctx := transcript.WithSessionID(context.Background(), "exp7-worker-i2")
	ctx = WithTaskID(ctx, "42")
	if err := drain(t, rec.Stream(ctx, llm.CompletionRequest{
		System: []string{"sys prompt"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}},
		},
		MaxTokens:   1024,
		Tools:       []llm.ToolSchema{{Name: "bash"}, {Name: "http"}},
		Temperature: &temp,
		Stop:        []string{"\n\n"},
		Thinking:    "disabled",
	})); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var (
		session, task, worker, model, status, reqBody, respBody, errMsg string
		in, out, cacheRead, cacheWrite                                  int
	)
	err := pg.QueryRow(`SELECT coalesce(session_id,''),coalesce(task_id,''),coalesce(worker,''),model,
coalesce(status,''),coalesce(request_body,''),coalesce(response_body,''),coalesce(error,''),
input_tokens,output_tokens,cache_read,cache_write
FROM llm_records WHERE profile_name=$1`, profile).
		Scan(&session, &task, &worker, &model, &status, &reqBody, &respBody, &errMsg,
			&in, &out, &cacheRead, &cacheWrite)
	_, _ = pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
	if err != nil {
		t.Fatalf("查询录制行: %v", err)
	}

	if session != "exp7-worker-i2" || task != "42" || worker != "worker" {
		t.Errorf("会话/任务/车道 = %q/%q/%q", session, task, worker)
	}
	if model != "claude-x" || status != "ok" || errMsg != "" {
		t.Errorf("model=%q status=%q err=%q", model, status, errMsg)
	}
	// Anthropic 把 input 放在 message_start、output 放在 message_delta，必须折叠相加。
	if in != 40 || out != 12 || cacheRead != 8 || cacheWrite != 0 {
		t.Errorf("token 折叠错误: in=%d out=%d cr=%d cw=%d", in, out, cacheRead, cacheWrite)
	}

	var req struct {
		System      []string          `json:"system"`
		MaxTokens   int               `json:"max_tokens"`
		Tools       []string          `json:"tools"`
		ToolsCount  int               `json:"tools_count"`
		Temperature float64           `json:"temperature"`
		Stop        []string          `json:"stop"`
		Thinking    map[string]string `json:"thinking"`
	}
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, reqBody)
	}
	if len(req.System) != 1 || req.System[0] != "sys prompt" || req.MaxTokens != 1024 {
		t.Errorf("请求体缺字段: %+v", req)
	}
	if len(req.Tools) != 2 || req.Tools[0] != "bash" || req.ToolsCount != 2 {
		t.Errorf("工具名应只存名字: %+v", req)
	}
	if req.Temperature != 0.2 || len(req.Stop) != 1 {
		t.Errorf("温度/停止词未记录: %+v", req)
	}
	// 每请求的 Thinking 覆盖配置级 thinkingType，与 norma buildBody 的判定一致。
	if req.Thinking["type"] != "disabled" || req.Thinking["effort"] != "high" {
		t.Errorf("思考参数应记录有效值: %+v", req.Thinking)
	}

	var resp struct {
		Text       string         `json:"text"`
		Thinking   string         `json:"thinking"`
		StopReason string         `json:"stop_reason"`
		Usage      map[string]int `json:"usage"`
	}
	if err := json.Unmarshal([]byte(respBody), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v (%s)", err, respBody)
	}
	if resp.Text != "hello world" || resp.Thinking != "why not" {
		t.Errorf("响应累积错误: text=%q thinking=%q", resp.Text, resp.Thinking)
	}
	if resp.StopReason != "end_turn" || resp.Usage["input_tokens"] != 40 || resp.Usage["output_tokens"] != 12 {
		t.Errorf("响应元数据错误: %+v", resp)
	}
}

func TestStreamRecordsErrorStatus(t *testing.T) {
	pg := requirePG(t)
	profile := "llmrec-err-" + uuid.NewString()
	provider := scriptedProvider{
		events: []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 5}}},
		err:    errors.New("upstream exploded"),
	}
	rec := Wrap(provider, pg, "m", profile, "", "", func() bool { return true })

	gotErr := drain(t, rec.Stream(transcript.WithSessionID(context.Background(), "exp3-planner"), llm.CompletionRequest{}))
	if gotErr == nil {
		t.Fatal("错误应透传给调用方")
	}

	var status, errMsg string
	err := pg.QueryRow(`SELECT coalesce(status,''),coalesce(error,'') FROM llm_records WHERE profile_name=$1`, profile).
		Scan(&status, &errMsg)
	_, _ = pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
	if err != nil {
		t.Fatalf("查询录制行: %v", err)
	}
	if status != "error" || !strings.Contains(errMsg, "upstream exploded") {
		t.Errorf("失败调用必须留下状态与原因: status=%q err=%q", status, errMsg)
	}
}

// TestConsumerStopStillRecords 覆盖消费方提前 break：defer 里的收尾必须把已累积的
// 用量落库，否则中断的调用会在台账上凭空消失。
func TestConsumerStopStillRecords(t *testing.T) {
	pg := requirePG(t)
	profile := "llmrec-break-" + uuid.NewString()
	provider := scriptedProvider{events: []llm.StreamEvent{
		{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 9}},
		{Type: llm.SETextDelta, Text: "partial"},
		{Type: llm.SETextDelta, Text: "never seen"},
	}}
	rec := Wrap(provider, pg, "m", profile, "", "", func() bool { return true })

	for ev := range rec.Stream(transcript.WithSessionID(context.Background(), "exp1-worker"), llm.CompletionRequest{}) {
		if ev.Type == llm.SETextDelta {
			break
		}
	}

	var status string
	var body string
	err := pg.QueryRow(`SELECT coalesce(status,''),coalesce(response_body,'') FROM llm_records WHERE profile_name=$1`, profile).
		Scan(&status, &body)
	_, _ = pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
	if err != nil {
		t.Fatalf("提前退出也必须留下录制行: %v", err)
	}
	// 消费者退出时 ctx 未取消，收尾按正常完成记（流没报错）。
	if status != "ok" {
		t.Errorf("status=%q", status)
	}
	if !strings.Contains(body, "partial") || strings.Contains(body, "never seen") {
		t.Errorf("只应记录已消费的部分: %s", body)
	}
}

func TestCompleteRecordsThinkingAndBody(t *testing.T) {
	pg := requirePG(t)
	profile := "llmrec-complete-" + uuid.NewString()
	rec := Wrap(scriptedProvider{}, pg, "m", profile, "enabled", "", func() bool { return true })

	ctx := WithWorker(transcript.WithSessionID(context.Background(), "exp9-judge"), "judge")
	msg, stop, usage, err := rec.Complete(ctx, llm.CompletionRequest{MaxTokens: 16})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if msg.Text() != "final answer" || stop != "end_turn" || usage.InputTokens != 11 {
		t.Fatalf("返回值不应被记录逻辑改动: %q %q %+v", msg.Text(), stop, usage)
	}

	var worker, respBody string
	err = pg.QueryRow(`SELECT coalesce(worker,''),coalesce(response_body,'') FROM llm_records WHERE profile_name=$1`, profile).
		Scan(&worker, &respBody)
	_, _ = pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
	if err != nil {
		t.Fatalf("查询录制行: %v", err)
	}
	if worker != "judge" {
		t.Errorf("WithWorker 的 lane 应覆盖会话解析结果，得到 %q", worker)
	}
	var resp struct {
		Thinking string `json:"thinking"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal([]byte(respBody), &resp); err != nil {
		t.Fatalf("响应体非法: %v", err)
	}
	if resp.Thinking != "step-1" || resp.Text != "final answer" {
		t.Errorf("thinking/text 记录错误: %+v", resp)
	}
}

func TestCompleteErrorPath(t *testing.T) {
	pg := requirePG(t)
	profile := "llmrec-complete-err-" + uuid.NewString()
	errProvider := completeErrorProvider{}
	rec := Wrap(errProvider, pg, "m", profile, "", "", func() bool { return true })

	if _, _, _, err := rec.Complete(context.Background(), llm.CompletionRequest{}); err == nil {
		t.Fatal("错误应透传")
	}
	var status string
	err := pg.QueryRow(`SELECT coalesce(status,'') FROM llm_records WHERE profile_name=$1`, profile).Scan(&status)
	_, _ = pg.Exec(`DELETE FROM llm_records WHERE profile_name=$1`, profile)
	if err != nil {
		t.Fatal(err)
	}
	if status != "error" {
		t.Errorf("status=%q want error", status)
	}
}

type completeErrorProvider struct{}

func (completeErrorProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (completeErrorProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "", llm.Usage{}, errors.New("boom")
}

// TestRecordingDisabledSkipsBodies 覆盖开关关闭：正文（贵）不写，但用量（便宜、
// 且是 token 统计的唯一来源）必须照写。
func TestRecordingDisabledSkipsBodies(t *testing.T) {
	pg := requirePG(t)
	profile := "llmrec-off-" + uuid.NewString()
	provider := scriptedProvider{events: []llm.StreamEvent{
		{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 3}},
		{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 1}},
	}}
	rec := Wrap(provider, pg, "m", profile, "", "", func() bool { return false })
	if err := drain(t, rec.Stream(transcript.WithSessionID(context.Background(), "exp2-worker"), llm.CompletionRequest{})); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var records int
	if err := pg.QueryRow(`SELECT count(*) FROM llm_records WHERE profile_name=$1`, profile).Scan(&records); err != nil {
		t.Fatal(err)
	}
	var usage int
	if err := pg.QueryRow(`SELECT count(*) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	if records != 0 {
		t.Errorf("开关关闭时不应写正文，实际 %d 行", records)
	}
	if usage != 1 {
		t.Errorf("用量计量必须始终运行，实际 %d 行", usage)
	}
}

func TestRecordUsageSkipsEmptyAndNilDB(t *testing.T) {
	// pg 为 nil：纯装饰器模式（测试里常见），不能 panic。
	Wrap(scriptedProvider{}, nil, "m", "", "", "", nil).recordUsage("1", 1, "worker", llm.Usage{}, 0, "ok")

	pg := requirePG(t)
	profile := "llmrec-empty-" + uuid.NewString()
	// 没有模型名、也没有任何 token 的调用不值得记账。
	Wrap(scriptedProvider{}, pg, "", profile, "", "", nil).
		recordUsage("1", 1, "worker", llm.Usage{}, 0, "ok")
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("空调用不应写用量行，实际 %d", n)
	}
	// 只要有 token 就必须记，哪怕模型名缺失。
	Wrap(scriptedProvider{}, pg, "", profile, "", "", nil).
		recordUsage("1", 1, "worker", llm.Usage{OutputTokens: 2}, 5, "ok")
	if err := pg.QueryRow(`SELECT count(*) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&n); err != nil {
		t.Fatal(err)
	}
	_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	if n != 1 {
		t.Errorf("有 token 的调用必须记账，实际 %d", n)
	}
}

func TestSerializeRequestOmitsEmptyOptionals(t *testing.T) {
	rec := &Recorder{model: "m"}
	body := rec.serializeRequest(llm.CompletionRequest{MaxTokens: 8})
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("非法 JSON: %v", err)
	}
	for _, absent := range []string{"thinking", "tools", "tools_count", "temperature", "stop"} {
		if _, ok := m[absent]; ok {
			t.Errorf("未设置的 %s 不应出现在请求体里", absent)
		}
	}
	if m["max_tokens"].(float64) != 8 {
		t.Errorf("max_tokens 丢失: %v", m)
	}

	// 配置级 thinkingType 在每请求未覆盖时生效。
	rec = &Recorder{model: "m", thinkingType: "enabled", reasoningEffort: "low"}
	var withThinking struct {
		Thinking map[string]string `json:"thinking"`
	}
	if err := json.Unmarshal([]byte(rec.serializeRequest(llm.CompletionRequest{})), &withThinking); err != nil {
		t.Fatal(err)
	}
	if withThinking.Thinking["type"] != "enabled" || withThinking.Thinking["effort"] != "low" {
		t.Errorf("配置级思考参数未写入: %+v", withThinking.Thinking)
	}
}

func TestParseSessionVariants(t *testing.T) {
	cases := []struct {
		in, task, worker string
	}{
		{"exp12-worker-i99", "12", "worker"},
		{"exp2-planner", "2", "planner"},
		{"exp3", "3", ""},
		{"worker-1", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		task, worker := parseSession(c.in)
		if task != c.task || worker != c.worker {
			t.Errorf("parseSession(%q)=(%q,%q)，期望 (%q,%q)", c.in, task, worker, c.task, c.worker)
		}
	}
}

func TestCaptureAttemptsAndRawBodies(t *testing.T) {
	ctx, capt := NewCapture(context.Background())
	if CaptureFrom(ctx) != capt {
		t.Fatal("NewCapture 应把 Capture 挂到 ctx 上")
	}
	if CaptureFrom(nil) != nil || CaptureFrom(context.Background()) != nil {
		t.Fatal("没有 capture 的 ctx 应返回 nil")
	}
	// nil 接收者必须全部安全（关闭录制时同一个 transport 仍会被复用）。
	var nilCapt *Capture
	nilCapt.SetRequest("x")
	if got := nilCapt.RawRequest(); got != "" {
		t.Errorf("nil capture RawRequest=%q", got)
	}
	if got := nilCapt.RawResponse(); got != "" {
		t.Errorf("nil capture RawResponse=%q", got)
	}
	if got := nilCapt.Attempts(); got != nil {
		t.Errorf("nil capture Attempts=%v", got)
	}
	if rc := nilCapt.TeeResponse(200, io.NopCloser(strings.NewReader("x"))); rc == nil {
		t.Error("nil capture 应原样返回响应体")
	}

	// 重试会重复发送同样的请求体，只保留第一份。
	capt.SetRequest("first")
	capt.SetRequest("second")
	if got := capt.RawRequest(); got != "first" {
		t.Errorf("RawRequest=%q want first", got)
	}
	capt.SetRequest("")

	// 第一次尝试没读完就失败（norma 会关掉非最终响应），第二次才成功。
	first := capt.TeeResponse(429, io.NopCloser(strings.NewReader("rate limited")))
	_, _ = io.ReadAll(first)
	_ = first.Close()
	second := capt.TeeResponse(200, io.NopCloser(strings.NewReader("data: ok\n\n")))
	_, _ = io.ReadAll(second)
	_ = second.Close()

	attempts := capt.Attempts()
	if len(attempts) != 2 || attempts[0].Status != 429 || attempts[1].Status != 200 {
		t.Fatalf("Attempts=%+v", attempts)
	}
	raw := capt.RawResponse()
	for _, want := range []string{"attempt 1/2", "HTTP 429", "rate limited", "attempt 2/2", "data: ok"} {
		if !strings.Contains(raw, want) {
			t.Errorf("重试拼接缺少 %q:\n%s", want, raw)
		}
	}
}

func TestCaptureSingleAttemptIsRaw(t *testing.T) {
	_, capt := NewCapture(context.Background())
	rc := capt.TeeResponse(200, io.NopCloser(strings.NewReader("payload")))
	_, _ = io.ReadAll(rc)
	// 单次尝试要原样返回，不能加头部——否则粘不回播放器。
	if got := capt.RawResponse(); got != "payload" {
		t.Errorf("RawResponse=%q want payload", got)
	}
}

func TestCaptureTeeResponseNilBody(t *testing.T) {
	_, capt := NewCapture(context.Background())
	if rc := capt.TeeResponse(500, nil); rc != nil {
		t.Error("nil 响应体应原样返回 nil")
	}
	if got := capt.RawResponse(); got != "" {
		t.Errorf("没有尝试时 RawResponse=%q", got)
	}
}
