package db

import "testing"

// digest.go 是冷压缩的持久层（round 计数、冷盖章、摘要节点与 covers 边），
// constraints.go 是任务约束的 CRUD。两者都是探索级隔离，必须钉死 scope。

func digestFixture(t *testing.T, d *DB) (*ExplorationStore, func()) {
	t.Helper()
	expID, err := d.CreateExploration("digest test", "goal")
	if err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(expID)
	cleanup := func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) }
	t.Cleanup(cleanup)
	return es, cleanup
}

func TestRoundCounterAndColdStamps(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })
	es, _ := digestFixture(t, d)

	r0, err := es.RoundNo()
	if err != nil {
		t.Fatal(err)
	}
	r1, err := es.BumpRound()
	if err != nil || r1 != r0+1 {
		t.Fatalf("BumpRound: %d -> %d err=%v", r0, r1, err)
	}
	if r2, _ := es.RoundNo(); r2 != r1 {
		t.Fatalf("RoundNo after bump: %d want %d", r2, r1)
	}

	i1, err := es.AddNode(KindIntent, map[string]any{"summary": "probe admin"}, 0, "done", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	i2, err := es.AddNode(KindIntent, map[string]any{"summary": "port scan"}, 0, "done", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 未盖章的节点 → nil
	stamps, err := es.ColdStamps()
	if err != nil {
		t.Fatal(err)
	}
	if stamps[i1] != nil || stamps[i2] != nil {
		t.Fatalf("fresh nodes must be unstamped: %v %v", stamps[i1], stamps[i2])
	}

	// 盖章 / 清除
	if err := es.ApplyStampOps([]StampOp{{ID: i1, Set: true, Round: r1}}); err != nil {
		t.Fatal(err)
	}
	stamps, _ = es.ColdStamps()
	if stamps[i1] == nil || *stamps[i1] != r1 {
		t.Fatalf("stamp not applied: %v", stamps[i1])
	}
	if err := es.ApplyStampOps([]StampOp{{ID: i1, Set: false}}); err != nil {
		t.Fatal(err)
	}
	stamps, _ = es.ColdStamps()
	if stamps[i1] != nil {
		t.Fatalf("clear not applied: %v", stamps[i1])
	}
	// 空批次是 no-op
	if err := es.ApplyStampOps(nil); err != nil {
		t.Fatal(err)
	}

	// 内容版本：新建即 0 或 1，不 panic 即可；键覆盖所有节点
	versions, err := es.ContentVersions()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := versions[i2]; !ok {
		t.Fatalf("ContentVersions missing node %d", i2)
	}
}

func TestDigestLifecycle(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })
	es, _ := digestFixture(t, d)

	m1, err := es.AddNode(KindFact, map[string]any{"summary": "admin open", "signature": "s1"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := es.AddNode(KindFact, map[string]any{"summary": "sqli found", "signature": "s2"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 初始无 active digest
	if digs, _ := es.ActiveDigests(); len(digs) != 0 {
		t.Fatalf("expected no digests initially, got %d", len(digs))
	}
	if cm, _ := es.CoveredMembers(); len(cm) != 0 {
		t.Fatalf("expected no coverage initially")
	}

	// 写摘要 + covers 边
	digestID, err := es.AddDigest(map[string]any{
		"summary": "folded 2 facts", "generation": 1, "signature": "sig-1",
		"member_ids": map[string]any{"facts": []int64{m1, m2}},
	}, []int64{m1, m2})
	if err != nil {
		t.Fatal(err)
	}
	if digestID <= 0 {
		t.Fatalf("digest id=%d", digestID)
	}

	digs, err := es.ActiveDigests()
	if err != nil || len(digs) != 1 || digs[0].ID != digestID {
		t.Fatalf("ActiveDigests: %v %v", digs, err)
	}
	members, err := es.DigestMembers(digestID)
	if err != nil || len(members) != 2 || members[0] != m1 || members[1] != m2 {
		t.Fatalf("DigestMembers: %v %v", members, err)
	}
	cm, err := es.CoveredMembers()
	if err != nil || cm[m1] != digestID || cm[m2] != digestID {
		t.Fatalf("CoveredMembers: %v %v", cm, err)
	}

	// 作废摘要：covers 边被删、节点转 superseded 但保留
	if err := es.SupersedeDigests([]int64{digestID}); err != nil {
		t.Fatal(err)
	}
	if cm, _ := es.CoveredMembers(); len(cm) != 0 {
		t.Fatalf("superseded digest must not cover: %v", cm)
	}
	if digs, _ := es.ActiveDigests(); len(digs) != 0 {
		t.Fatalf("superseded digest must leave active set")
	}
	// 空列表 no-op
	if err := es.SupersedeDigests(nil); err != nil {
		t.Fatal(err)
	}
}

func TestNodeAssetsAnchorMap(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })
	es, _ := digestFixture(t, d)

	// 造一个资产供锚定
	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: "digest-anchor-test.example", TaskID: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, assetID) })
	nid, err := es.AddNode(KindFact, map[string]any{"summary": "anchored"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Anchor(nid, assetID); err != nil {
		t.Fatal(err)
	}

	// 空 ids → 空 map 快速返回
	if m, err := es.NodeAssets(nil); err != nil || len(m) != 0 {
		t.Fatalf("empty ids must short-circuit: %v %v", m, err)
	}
	m, err := es.NodeAssets([]int64{nid, 999999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(m[nid]) != 1 || m[nid][0] != assetID {
		t.Fatalf("NodeAssets broken: %v", m)
	}
}

func TestConstraintCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Close 交给 t.Cleanup 且最先注册（LIFO 最后执行）：行删除 cleanup 必须在池仍打开时运行，
	// 否则静默失败留下残留行，下次运行撞唯一键。
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("constraints test", "goal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) }()
	otherID, err := d.CreateExploration("constraints other", "goal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, otherID) }()

	es := d.Exploration(expID)
	os := d.Exploration(otherID)

	// 非法 kind 拒绝
	if _, err := es.AddConstraint("maybe", "x", ""); err == nil {
		t.Fatalf("invalid kind must be rejected")
	}

	a1, err := es.AddConstraint("allow", "允许访问 /admin", "human")
	if err != nil {
		t.Fatal(err)
	}
	d1, err := es.AddConstraint("deny", "禁止删除数据", "") // origin 空 → system
	if err != nil {
		t.Fatal(err)
	}
	a2, err := es.AddConstraint("allow", "允许弱口令尝试", "goals")
	if err != nil {
		t.Fatal(err)
	}

	list, err := es.ListConstraints()
	if err != nil || len(list) != 3 {
		t.Fatalf("ListConstraints: %v %v", list, err)
	}
	// allow 在 deny 前，同 kind 内按 id 升序
	if list[0].ID != a1 || list[1].ID != a2 || list[2].ID != d1 {
		t.Fatalf("ordering broken: %v", list)
	}
	if list[2].Origin != "system" {
		t.Fatalf("empty origin must default to system, got %q", list[2].Origin)
	}
	// 另一个探索看不到这些约束
	if other, _ := os.ListConstraints(); len(other) != 0 {
		t.Fatalf("cross-exploration leak: %v", other)
	}

	// 更新
	if err := es.UpdateConstraint(a1, "deny", "改为禁止访问 /admin"); err != nil {
		t.Fatal(err)
	}
	if err := es.UpdateConstraint(a1, "maybe", "x"); err == nil {
		t.Fatalf("invalid kind on update must be rejected")
	}
	if err := es.UpdateConstraint(d1+100000, "allow", "x"); err == nil {
		t.Fatalf("updating missing constraint must error")
	}
	list, _ = es.ListConstraints()
	// a1 现在是 deny：deny 组 [a1, d1]，allow 组 [a2]
	if !(list[0].ID == a2 && list[1].ID == a1 && list[2].ID == d1) {
		t.Fatalf("post-update ordering broken: %+v", list)
	}

	// 删除（含不存在分支）
	if err := es.DeleteConstraint(a1); err != nil {
		t.Fatal(err)
	}
	if err := es.DeleteConstraint(a1); err == nil {
		t.Fatalf("deleting missing constraint must error")
	}
	if list, _ = es.ListConstraints(); len(list) != 2 {
		t.Fatalf("delete failed")
	}
}
