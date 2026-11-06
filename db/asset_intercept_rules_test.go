package db

import "testing"

// asset_intercept.go / task_intercept.go：全局资产黑名单 + 任务级 block/allow
// 规则。Action 归一化（空→block）与 task_id scope 是安全契约，写错会放行不该
// 测的目标。

func TestGlobalAssetInterceptRulesCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	created, err := d.CreateAssetInterceptRule("fuzzy_domain", "corp-target.example", "测试黑名单", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteAssetInterceptRule(created.ID) }()
	if created.ID <= 0 || created.Builtin || !created.Enabled || created.Action != "" {
		t.Fatalf("create broken: %+v", created)
	}

	// 更新 + 读取回环
	upd, err := d.UpdateAssetInterceptRule(created.ID, "exact_ip", "10.66.0.0/24", "改过的备注", false)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Kind != "exact_ip" || upd.Pattern != "10.66.0.0/24" || upd.Enabled {
		t.Fatalf("update broken: %+v", upd)
	}

	list, err := d.ListAssetInterceptRules()
	if err != nil {
		t.Fatal(err)
	}
	var mine *AssetInterceptRule
	for i := range list {
		if list[i].ID == created.ID {
			mine = &list[i]
		}
	}
	if mine == nil {
		t.Fatalf("rule missing from list")
	}

	// 开关
	if err := d.ToggleAssetInterceptRule(created.ID, true); err != nil {
		t.Fatal(err)
	}
	list, _ = d.ListAssetInterceptRules()
	for _, r := range list {
		if r.ID == created.ID && !r.Enabled {
			t.Fatalf("toggle to enabled failed")
		}
	}

	if err := d.DeleteAssetInterceptRule(created.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = d.ListAssetInterceptRules()
	for _, r := range list {
		if r.ID == created.ID {
			t.Fatalf("delete failed")
		}
	}
}

func TestTaskInterceptRulesCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTask("task intercept rules", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()
	s := d.Assets()

	// taskID<=0 快速返回空
	if rules, err := s.ListTaskInterceptRules(0); err != nil || rules != nil {
		t.Fatalf("taskID=0 must return nil,nil, got %v %v", rules, err)
	}
	if _, _, err := s.TaskInterceptRulesSplit(0); err != nil {
		t.Fatalf("split with taskID=0 must not error: %v", err)
	}

	// 创建：action 空 → 归一化为 block
	b1, err := s.CreateTaskInterceptRule(task.ID, "", "fuzzy_domain", "blocked.example", "默认拦截", true)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := s.CreateTaskInterceptRule(task.ID, "allow", "exact_domain", "allowed.example", "白名单", true)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Action != "block" {
		t.Fatalf("empty action must normalize to block, got %q", b1.Action)
	}

	// split：block 在前 allow 在后
	block, allow, err := s.TaskInterceptRulesSplit(task.ID)
	if err != nil || len(block) != 1 || len(allow) != 1 {
		t.Fatalf("split: %v %v %v", block, allow, err)
	}
	if block[0].ID != b1.ID || allow[0].ID != a1.ID {
		t.Fatalf("split membership broken")
	}

	// 更新：跨任务的 task_id scope 必须拒绝（0 行 → ErrNoRows）
	if _, err := s.UpdateTaskInterceptRule(task.ID+100000, b1.ID, "allow", "x", "y", "z", true); err == nil {
		t.Fatalf("cross-task update must fail")
	}
	upd, err := s.UpdateTaskInterceptRule(task.ID, b1.ID, "block", "cidr", "10.0.0.0/8", "改过", false)
	if err != nil || upd.Kind != "cidr" || upd.Pattern != "10.0.0.0/8" || upd.Enabled || upd.Action != "block" {
		t.Fatalf("update broken: %+v %v", upd, err)
	}

	// 开关
	if err := s.ToggleTaskInterceptRule(task.ID, b1.ID, true); err != nil {
		t.Fatal(err)
	}
	rules, _ := s.ListTaskInterceptRules(task.ID)
	for _, r := range rules {
		if r.ID == b1.ID && !r.Enabled {
			t.Fatalf("toggle failed")
		}
	}

	// 删除：false 分支（不存在）+ 真删
	if ok, err := s.DeleteTaskInterceptRule(task.ID, b1.ID+100000); err != nil || ok {
		t.Fatalf("missing delete must return false,nil, got %v %v", ok, err)
	}
	if ok, err := s.DeleteTaskInterceptRule(task.ID, b1.ID); err != nil || !ok {
		t.Fatalf("delete failed: %v %v", ok, err)
	}
	if rules, _ = s.ListTaskInterceptRules(task.ID); len(rules) != 1 {
		t.Fatalf("post-delete count=%d", len(rules))
	}
}

func TestInsertTaskInterceptRulesInTx(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTask("tx intercept rules", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteTask(task.ID) }()

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	inputs := []TaskInterceptRuleInput{
		{Enabled: true, Action: "", Kind: "fuzzy_url", Pattern: "http://tx-*.example", Note: "n1"},
		{Enabled: false, Action: "allow", Kind: "exact_ip", Pattern: "192.168.1.1", Note: ""},
	}
	if err := insertTaskInterceptRules(tx, task.ID, inputs); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rules, err := d.Assets().ListTaskInterceptRules(task.ID)
	if err != nil || len(rules) != 2 {
		t.Fatalf("insert via tx: %v %v", rules, err)
	}
	// ListTaskInterceptRules 按 (action, id) 排序：allow 字母序在 block 前
	if rules[0].Action != "allow" || rules[1].Action != "block" || rules[1].Note != "n1" {
		t.Fatalf("normalize in tx broken: %+v", rules)
	}
}
