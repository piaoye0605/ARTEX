package db

import (
	"encoding/json"
	"testing"
)

// 杂项收尾：流量证据纯函数、拦截规则开关与裁决、工具绑定、任务范围登记。

func TestTrafficRefNormalizeAndSnapshotID(t *testing.T) {
	if !ValidTrafficRole("proof") || !ValidTrafficRole("baseline") ||
		!ValidTrafficRole("verification") || !ValidTrafficRole("supporting") {
		t.Fatalf("known roles must be valid")
	}
	if ValidTrafficRole("everything") {
		t.Fatalf("unknown role must be invalid")
	}

	// 去重 + 空白清理 + 默认 role + 非法 role 拒绝
	refs, err := NormalizeTrafficRefs([]TrafficRef{
		{TrafficID: " t1 ", Role: ""},
		{TrafficID: "t1", Role: "proof"}, // 重复 id 丢弃
		{TrafficID: "t2", Role: "bogus"},
	})
	if err == nil {
		t.Fatalf("invalid role must be rejected")
	}
	refs, err = NormalizeTrafficRefs([]TrafficRef{
		{TrafficID: " t1 ", Role: ""},
		{TrafficID: "t1", Role: "proof"},
		{TrafficID: "t2", Role: "verification"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].TrafficID != "t1" || refs[0].Role != "supporting" || refs[1].Role != "verification" {
		t.Fatalf("normalize broken: %+v", refs)
	}
	if _, err := NormalizeTrafficRefs([]TrafficRef{{TrafficID: "   "}}); err == nil {
		t.Fatalf("blank traffic_id must be rejected")
	}

	// 快照 ID：内容寻址、字段清洗影响哈希、CreatedAt/ID 不参与
	base := TrafficEvidenceSnapshot{SourceTrafficID: "abc", URL: "http://x/", Method: "GET", Status: 200, ReqHash: "r", RespHash: "p"}
	id1 := TrafficSnapshotID(base)
	if id2 := TrafficSnapshotID(base); id1 != id2 {
		t.Fatalf("snapshot id must be deterministic")
	}
	if len(id1) != 64 {
		t.Fatalf("sha256 hex length")
	}
	withBadHead := base
	withBadHead.ReqHead = "X: \x00bad" // NUL 会被 utf8Clean 清掉
	withBadHead2 := base
	withBadHead2.ReqHead = "X: bad" // NUL 被删除后等价
	if TrafficSnapshotID(withBadHead) != TrafficSnapshotID(withBadHead2) {
		t.Fatalf("Normalize must clean non-UTF8 before hashing")
	}
	withTime := base
	withTime.CreatedAt = withTime.CreatedAt.AddDate(1, 0, 0)
	withTime.ID = "whatever"
	if TrafficSnapshotID(withTime) != id1 {
		t.Fatalf("ID/CreatedAt must not participate in the hash")
	}
}

func TestInterceptToggleAndDecide(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })

	rule, err := d.CreateInterceptRule("t-toggle", "tool_name", "string", "rm", "deny", "危险命令", 100, true, false, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteInterceptRule(rule.ID) })

	if err := d.ToggleInterceptRule(rule.ID, false); err != nil {
		t.Fatal(err)
	}
	rules, _ := d.ListInterceptRules()
	for _, r := range rules {
		if r.ID == rule.ID && r.Enabled {
			t.Fatalf("toggle to disabled failed")
		}
	}

	// 待裁决记录：convID=0 / taskID="" 的 NULL 分支 + 裁决三态之一
	pid, err := d.CreateInterceptPending(rule.ID, 0, "", "test_agent", "bash", []byte(`{"command":"rm"}`), "命中黑名单")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, pid) })
	if err := d.DecideInterceptPending(pid, "denied"); err != nil {
		t.Fatal(err)
	}
	if err := d.DecideInterceptPending(pid+999999, "allowed"); err != nil {
		t.Fatalf("deciding a missing pending must be a silent no-op, got %v", err)
	}
}

func TestToolAgentBindingOps(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })

	// 用测试专属 tool 行，不碰种子数据
	key := "t_bind_tool"
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tools WHERE key=$1`, key) })
	if _, err := d.Exec(`INSERT INTO tools(key, system, description, schema, agents, enabled) VALUES ($1, false, '测试', '{}', '["existing_a"]'::jsonb, true)`, key); err != nil {
		t.Fatal(err)
	}

	readAgents := func() []string {
		var raw json.RawMessage
		if err := d.QueryRow(`SELECT agents FROM tools WHERE key=$1`, key).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil {
			t.Fatal(err)
		}
		return names
	}

	// 幂等追加
	if err := d.AddAgentToToolBinding("agent_x", []string{key}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAgentToToolBinding("agent_x", []string{key}); err != nil {
		t.Fatal(err)
	}
	names := readAgents()
	if len(names) != 2 || names[0] != "existing_a" || names[1] != "agent_x" {
		t.Fatalf("idempotent binding broken: %v", names)
	}

	// 单工具摘除 + 全局摘除
	if err := d.RemoveAgentFromTool("agent_x", key); err != nil {
		t.Fatal(err)
	}
	for _, a := range readAgents() {
		if a == "agent_x" {
			t.Fatalf("RemoveAgentFromTool broken: %v", readAgents())
		}
	}
	if err := d.AddAgentToToolBinding("agent_y", []string{key}); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveAgentFromToolBindings("agent_y"); err != nil {
		t.Fatal(err)
	}
	for _, a := range readAgents() {
		if a == "agent_y" {
			t.Fatalf("RemoveAgentFromToolBindings broken: %v", readAgents())
		}
	}
}

func TestAddAutoScopeAndAgentScope(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("auto scope probe", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	s := d.Assets()

	// taskID<=0 no-op
	if err := s.AddAutoScope(0, "root_domain", "x.example", "", ""); err != nil {
		t.Fatalf("taskID=0 must be no-op, got %v", err)
	}

	// root_domain / subdomain / ip（IP 字面量 host 回落 CIDR）
	if err := s.AddAutoScope(task.ID, "root_domain", "Auto.Example.", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAutoScope(task.ID, "subdomain", "www.Auto.Example", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAutoScope(task.ID, "ip", "", "", "10.20.30.40"); err != nil {
		t.Fatal(err)
	}
	scopes, err := s.ListTaskScope(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]TaskScope{}
	for _, sc := range scopes {
		kinds[sc.Kind] = sc
	}
	if _, ok := kinds["root_domain"]; !ok {
		t.Fatalf("root_domain scope missing: %+v", scopes)
	}
	if _, ok := kinds["subdomain"]; !ok {
		t.Fatalf("subdomain scope missing: %+v", scopes)
	}
	ipScope, ok := kinds["ip"]
	if !ok || ipScope.Net == "" {
		t.Fatalf("ip scope missing: %+v", scopes)
	}

	// AddAgentScope：校验分支
	if _, err := s.AddAgentScope(0, "ip", "1.2.3.4", "", ""); err == nil {
		t.Fatalf("taskID=0 must error")
	}
	if _, err := s.AddAgentScope(task.ID, "ip", "  ", "", ""); err == nil {
		t.Fatalf("blank value must error")
	}
	if _, err := s.AddAgentScope(task.ID, "company", "99999999", "", ""); err == nil {
		t.Fatalf("missing company must error")
	}
	ts, err := s.AddAgentScope(task.ID, "cidr", "192.168.5.0/24", "授权网段", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Kind != "cidr" || ts.Source != "manual" || ts.Reason != "授权网段" {
		t.Fatalf("AddAgentScope cidr broken: %+v", ts)
	}
	// 裸 IP 归一为 kind=ip 的 /32 网段
	ts2, err := s.AddAgentScope(task.ID, "cidr", "192.168.6.5", "", "")
	if err != nil || ts2.Kind != "ip" || ts2.Net != "192.168.6.5/32" {
		t.Fatalf("bare IP must normalize to ip scope /32: %+v %v", ts2, err)
	}
}
