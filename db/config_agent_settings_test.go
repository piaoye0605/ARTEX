package db

import (
	"testing"
)

// config.go 的 agent 设置面：自定义 agent 生命周期、十几枚 setter（含负数钳制、
// 枚举白名单回落）、prompt 版本化、MCP 服务器与工具缓存。

func agentFixture(t *testing.T, d *DB) *Agent {
	t.Helper()
	a, err := d.CreateAgent("test_setter_agent", "测试 Agent", "描述文本")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteAgent("test_setter_agent") })
	return a
}

func reloadAgent(t *testing.T, d *DB, key string) *Agent {
	t.Helper()
	list, err := d.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.Key == key {
			return a
		}
	}
	t.Fatalf("agent %s missing from ListAgents", key)
	return nil
}

func TestAgentSettersRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	a := agentFixture(t, d)

	if a.Role != "assistant" || a.Builtin || !a.Enabled {
		t.Fatalf("create defaults broken: %+v", a)
	}

	if err := d.SetAgentMaxTurns("test_setter_agent", 25); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentMaxTurns("test_setter_agent", -5); err != nil { // 负数钳到 0
		t.Fatal(err)
	}
	if err := d.SetAgentRunSeconds("test_setter_agent", 3600); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentRunSeconds("test_setter_agent", -1); err != nil { // 钳 0
		t.Fatal(err)
	}
	if err := d.SetAgentWebSearch("test_setter_agent", true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentInteractiveShell("test_setter_agent", true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentWrapupPrompt("test_setter_agent", "自定义收尾词"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentWrapupMaxTurns("test_setter_agent", 4); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentTaskTimeoutWrapup("test_setter_agent", "超时收尾词", 2); err != nil {
		t.Fatal(err)
	}
	// 非法枚举回落默认；maxParallel 负数钳 0
	if err := d.SetAgentTriggerBehavior("test_setter_agent", "bogus", "bogus", -3); err != nil {
		t.Fatal(err)
	}

	got := reloadAgent(t, d, "test_setter_agent")
	if got.MaxTurns != 0 || got.RunSecs != 0 {
		t.Fatalf("negative clamp broken: maxTurns=%d runSecs=%d", got.MaxTurns, got.RunSecs)
	}
	if !got.WebSearch || !got.InteractiveShell {
		t.Fatalf("bool setters broken: %+v", got)
	}
	if got.WrapupPrompt != "自定义收尾词" || got.WrapupMaxTurns != 4 {
		t.Fatalf("wrapup setters broken: %+v", got)
	}
	if got.TaskTimeoutWrapupPrompt != "超时收尾词" || got.TaskTimeoutWrapupMaxTurns != 2 {
		t.Fatalf("task-timeout wrapup broken: %+v", got)
	}
	if got.TriggerRunMode != "serial" || got.TriggerMergeMode != "by_task" || got.TriggerMaxParallel != 0 {
		t.Fatalf("trigger behavior fallback broken: %+v", got)
	}

	// 合法枚举透传
	if err := d.SetAgentTriggerBehavior("test_setter_agent", "parallel", "none", 3); err != nil {
		t.Fatal(err)
	}
	got = reloadAgent(t, d, "test_setter_agent")
	if got.TriggerRunMode != "parallel" || got.TriggerMergeMode != "none" || got.TriggerMaxParallel != 3 {
		t.Fatalf("trigger behavior passthrough broken: %+v", got)
	}

	// 元数据更新
	if err := d.UpdateAgentMeta("test_setter_agent", "新名字", "新描述"); err != nil {
		t.Fatal(err)
	}
	got = reloadAgent(t, d, "test_setter_agent")
	if got.Name != "新名字" || got.Description != "新描述" {
		t.Fatalf("UpdateAgentMeta broken: %+v", got)
	}
	// 内置 agent 受 builtin=false 守卫：UPDATE 0 行、内置名不被改动
	if err := d.UpdateAgentMeta("planner", "改名内置", ""); err != nil {
		t.Fatal(err)
	}
	planners, _ := d.ListAgents()
	for _, ag := range planners {
		if ag.Key == "planner" && ag.Name == "改名内置" {
			t.Fatalf("builtin agent must not be renamable via UpdateAgentMeta")
		}
	}

	// LLM profile 绑定：未知 key no-op、有效绑定、nil 清绑、无效 id 报错
	pid, err := d.SaveProfile(&LLMProfile{Name: "t-setter-profile", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, pid) })
	if err := d.SetAgentLLMProfile("no-such-key", &pid); err != nil {
		t.Fatalf("unknown key must be a no-op, got %v", err)
	}
	if err := d.SetAgentLLMProfile("test_setter_agent", &pid); err != nil {
		t.Fatal(err)
	}
	if got = reloadAgent(t, d, "test_setter_agent"); got.LLMProfileID == nil || *got.LLMProfileID != pid {
		t.Fatalf("profile binding broken: %+v", got)
	}
	bad := int64(99999999)
	if err := d.SetAgentLLMProfile("test_setter_agent", &bad); err != ErrLLMProfileNotFound {
		t.Fatalf("missing profile must return ErrLLMProfileNotFound, got %v", err)
	}
	if err := d.SetAgentLLMProfile("test_setter_agent", nil); err != nil {
		t.Fatal(err)
	}
	if got = reloadAgent(t, d, "test_setter_agent"); got.LLMProfileID != nil {
		t.Fatalf("nil must clear binding, got %v", *got.LLMProfileID)
	}

	if err := d.DeleteAgent("test_setter_agent"); err != nil {
		t.Fatal(err)
	}
	list, _ := d.ListAgents()
	for _, ag := range list {
		if ag.Key == "test_setter_agent" {
			t.Fatalf("DeleteAgent left the row")
		}
	}
}

func TestAgentPromptVersioning(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	a := agentFixture(t, d)

	// SeedPromptIfEmpty：首播写入 v1，二次调用 no-op
	if err := d.SeedPromptIfEmpty(a.ID, "模板 v1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SeedPromptIfEmpty(a.ID, "模板 v1 重复"); err != nil {
		t.Fatal(err)
	}
	prompt, err := d.CurrentPrompt(a.ID)
	if err != nil || prompt != "模板 v1" {
		t.Fatalf("seed prompt broken: %q %v", prompt, err)
	}

	// ResetPromptToDefault 追加新版本并指向它
	ver, err := d.ResetPromptToDefault(a.ID, "模板 v2")
	if err != nil || ver != 2 {
		t.Fatalf("reset prompt: ver=%d err=%v", ver, err)
	}
	prompt, _ = d.CurrentPrompt(a.ID)
	if prompt != "模板 v2" {
		t.Fatalf("reset must point current at the new version: %q", prompt)
	}

	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1`, a.ID)
		_, _ = d.Exec(`UPDATE agents SET current_prompt_id=NULL WHERE id=$1`, a.ID)
	})
}

func TestMCPServerAndToolCache(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveMCP(&MCPServer{
		Name: "test-mcp-srv", Transport: "stdio", Command: "python",
		Args: []byte(`["-m","srv"]`), Env: []byte(`{}`), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteMCP(id) })

	if err := d.SaveMCPTools(id, []MCPTool{
		{Name: "scan", Description: "扫描工具"},
		{Name: "report", Description: "报告工具"},
	}); err != nil {
		t.Fatal(err)
	}

	names, err := d.MCPToolNames(id)
	if err != nil || len(names) != 2 {
		t.Fatalf("MCPToolNames: %v %v", names, err)
	}
	detailed, err := d.MCPToolsDetailed(id)
	if err != nil || len(detailed) != 2 || detailed[0].Name != "report" || detailed[0].Description != "报告工具" {
		t.Fatalf("MCPToolsDetailed: %+v %v", detailed, err)
	}

	servers, err := d.ListMCP()
	var mine *MCPServer
	for i := range servers {
		if servers[i].ID == id {
			mine = servers[i]
		}
	}
	if mine == nil || len(mine.Tools) != 2 || mine.Command != "python" {
		t.Fatalf("ListMCP with cached tools: %+v", mine)
	}

	// SaveMCPTools 幂等覆盖（先清旧缓存再写）
	if err := d.SaveMCPTools(id, []MCPTool{{Name: "solo", Description: ""}}); err != nil {
		t.Fatal(err)
	}
	names, _ = d.MCPToolNames(id)
	if len(names) != 1 || names[0] != "solo" {
		t.Fatalf("SaveMCPTools overwrite broken: %v", names)
	}

	if err := d.DeleteMCP(id); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.MCPToolNames(id); len(got) != 0 {
		t.Fatalf("DeleteMCP must cascade tool cache: %v", got)
	}
}
