package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// TestStageFindingsExport 覆盖导出前的冻结步骤：绑定/版本/报告正文都要在响应
// 开始前钉住，否则下载过程中用户改一条 statement 就会出现"报告与附件对不上"。
func TestStageFindingsExport(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	seedExchange(t, s, "export", []byte("exported body"), false)
	r, err := s.Record(ctx, in, []db.TrafficRef{{TrafficID: "export", Role: "proof", Note: "n"}})
	if err != nil {
		t.Fatal(err)
	}
	// 报告正文是后续单独写入的（Record 只产出骨架），先把正文放进去。
	if _, err := s.DB.SetFindingReportByNodeID(r.NodeID, "export report"); err != nil {
		t.Fatal(err)
	}
	f, err := s.DB.GetFinding(r.FindingID)
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := s.StageFindingsExport(ctx, []*db.DBFinding{f}, dest, true); err != nil {
		t.Fatalf("StageFindingsExport: %v", err)
	}
	if len(f.TrafficBindings) != 1 || f.TrafficCount != 1 {
		t.Fatalf("绑定未冻结: count=%d bindings=%d", f.TrafficCount, len(f.TrafficBindings))
	}
	if f.EvidenceVersion != r.Traffic.Version {
		t.Errorf("证据版本未冻结: %d want %d", f.EvidenceVersion, r.Traffic.Version)
	}
	if f.Report != "export report" {
		t.Errorf("报告正文未冻结: %q", f.Report)
	}
	// 私有副本必须真实落盘到 dest（导出包不能链接回可丢弃的流量库）。
	for _, b := range f.TrafficBindings {
		for _, hash := range []string{b.Snapshot.ReqHash, b.Snapshot.RespHash} {
			path, err := hashPath(dest, hash)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("正文副本缺失 %s: %v", hash, err)
			}
		}
	}

	// copyBodies=false：只冻结元数据，用于只要报告不要附件的导出。
	metaOnly := t.TempDir()
	f2, err := s.DB.GetFinding(r.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StageFindingsExport(ctx, []*db.DBFinding{f2}, metaOnly, false); err != nil {
		t.Fatalf("StageFindingsExport(meta only): %v", err)
	}
	entries, err := os.ReadDir(metaOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("copyBodies=false 不应复制正文，实际 %d 项", len(entries))
	}
	if f2.TrafficCount != 1 {
		t.Errorf("元数据仍应被冻结: %+v", f2)
	}
}

func TestStageFindingsExportFailsOnUnknownFinding(t *testing.T) {
	s, _, _ := evidenceFixture(t)
	f := &db.DBFinding{}
	f.ID = 999999999
	// 找不到 findings 行时必须中止整个导出，而不是产出一份半残的包。
	if err := s.StageFindingsExport(context.Background(), []*db.DBFinding{f}, t.TempDir(), false); err == nil {
		t.Fatal("未知 finding 应报错")
	}
}

func TestBindingLookupAndErrors(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	seedExchange(t, s, "bind", []byte("binding body"), false)
	r, err := s.Record(ctx, in, []db.TrafficRef{{TrafficID: "bind"}})
	if err != nil {
		t.Fatal(err)
	}
	bindingID := r.Traffic.Bindings[0].ID

	b, err := s.Binding(ctx, r.FindingID, bindingID)
	if err != nil {
		t.Fatalf("Binding: %v", err)
	}
	if b.ID != bindingID || b.Snapshot.SourceTrafficID != "bind" {
		t.Errorf("binding 元数据不对: %+v", b)
	}

	if _, err := s.Binding(ctx, r.FindingID, bindingID+12345); !errors.Is(err, db.ErrEvidenceNotFound) {
		t.Fatalf("不存在的绑定应返回 ErrEvidenceNotFound，得到 %v", err)
	}
}

func TestOpenBodyRejectsBadSideAndBadHash(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	seedExchange(t, s, "open", []byte("body"), false)
	r, err := s.Record(ctx, in, []db.TrafficRef{{TrafficID: "open"}})
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Traffic.Bindings[0].Snapshot

	if _, _, err := s.OpenBody(snap, "sideways"); err == nil || !strings.Contains(err.Error(), "side") {
		t.Fatalf("非法 side 应报错，得到 %v", err)
	}

	// 元数据被改过（哈希对不上磁盘）时不能把错的字节当成证据交出去。
	tampered := snap
	tampered.RespHash = strings.Repeat("f", 64)
	if _, _, err := s.OpenBody(tampered, "response"); err == nil {
		t.Fatal("哈希不存在时应报错")
	}

	// 长度不符同样要拒。
	short := snap
	short.ReqLen = snap.ReqLen + 99
	if _, _, err := s.OpenBody(short, "request"); err == nil {
		t.Fatal("长度不符时应报错")
	}

	// 正常请求侧可读。
	f, length, err := s.OpenBody(snap, "request")
	if err != nil {
		t.Fatalf("OpenBody(request): %v", err)
	}
	defer f.Close()
	if length != snap.ReqLen {
		t.Errorf("返回长度 %d want %d", length, snap.ReqLen)
	}
}

func TestHashPathValidation(t *testing.T) {
	dir := t.TempDir()
	good := strings.Repeat("ab", 32)
	path, err := hashPath(dir, good)
	if err != nil {
		t.Fatalf("合法哈希被拒: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join("blobs", "ab", good+".bin")) {
		t.Errorf("路径布局不对: %s", path)
	}
	for _, bad := range []string{
		"",
		"deadbeef",              // 长度不够
		strings.Repeat("z", 64), // 非十六进制
		strings.ToUpper(good),   // 大写会被当成另一个路径
		good + "00",             // 超长
	} {
		if _, err := hashPath(dir, bad); err == nil {
			t.Errorf("hashPath(%q) 应拒绝", bad)
		}
	}
}

func TestWriteBodyRejectsShortAndMismatched(t *testing.T) {
	s, _, dir := evidenceFixture(t)
	if _, err := s.writeBody(strings.NewReader("abc"), 5, ""); err == nil || !strings.Contains(err.Error(), "正文不完整") {
		t.Fatalf("字节数不足应报错，得到 %v", err)
	}
	sum := sha256.Sum256([]byte("abc"))
	if _, err := s.writeBody(strings.NewReader("abc"), 3, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "哈希不匹配") {
		t.Fatalf("哈希不符应报错，得到 %v", err)
	}

	// 同一份正文写两次是幂等的（内容寻址），第二次只刷新宽限期。
	hash := hex.EncodeToString(sum[:])
	if _, err := s.writeBody(strings.NewReader("abc"), 3, hash); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	path, _ := hashPath(s.Dir, hash)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeBody(strings.NewReader("abc"), 3, hash); err != nil {
		t.Fatalf("重复写入应成功: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Error("重复写入应刷新宽限期（可能被 GC 提前回收）")
	}
	// 暂存目录不能留垃圾。
	staged, err := os.ReadDir(filepath.Join(dir, "evidence", ".staging"))
	if err == nil && len(staged) != 0 {
		t.Errorf("暂存目录残留 %d 个文件", len(staged))
	}
}

// TestInstallSnapshotsFromArchive 覆盖从导出包导回：源目录里的正文必须重新落到
// 本实例的 blobs 下，之后才能被 OpenBody 读到。
func TestInstallSnapshotsFromArchive(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	seedExchange(t, s, "install", []byte("installable body"), false)
	r, err := s.Record(ctx, in, []db.TrafficRef{{TrafficID: "install"}})
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Traffic.Bindings[0].Snapshot
	archive := t.TempDir()
	if err := s.CopySnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, archive); err != nil {
		t.Fatal(err)
	}

	// 先删掉本地正文，模拟"只拿到了归档包"的新实例。
	path, err := hashPath(s.Dir, snap.RespHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenBody(snap, "response"); err == nil {
		t.Fatal("正文已删，OpenBody 不该成功")
	}

	called := false
	if err := s.WithInstalledSnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, archive, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithInstalledSnapshots: %v", err)
	}
	if !called {
		t.Error("恢复回调必须被调用")
	}
	f, _, err := s.OpenBody(snap, "response")
	if err != nil {
		t.Fatalf("导回后正文应可读: %v", err)
	}
	f.Close()

	// 归档目录里缺文件时必须整体失败（不能只导回一半）。
	if err := s.InstallSnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, t.TempDir()); err == nil {
		t.Fatal("源目录缺正文应报错")
	}
}

func TestRunGCStopsOnContextCancel(t *testing.T) {
	s, _, _ := evidenceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.RunGC(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 GC 循环必须退出，否则进程关不掉")
	}
}

func TestCopySnapshotsIsIdempotent(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	seedExchange(t, s, "copy", []byte("copy body"), false)
	r, err := s.Record(ctx, in, []db.TrafficRef{{TrafficID: "copy"}})
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Traffic.Bindings[0].Snapshot
	dest := t.TempDir()
	for i := 0; i < 2; i++ {
		// 第二次走 O_EXCL 已存在的分支：只校验、不重写。
		if err := s.CopySnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, dest); err != nil {
			t.Fatalf("第 %d 次复制: %v", i+1, err)
		}
	}
}

func TestCopySnapshotsFailsWhenBodyMissing(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	ctx := context.Background()
	seedExchange(t, s, "copy-miss", []byte("body"), false)
	r, err := s.Record(ctx, in, []db.TrafficRef{{TrafficID: "copy-miss"}})
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Traffic.Bindings[0].Snapshot
	path, err := hashPath(s.Dir, snap.ReqHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.CopySnapshots(ctx, []db.TrafficEvidenceSnapshot{snap}, t.TempDir()); err == nil {
		t.Fatal("正文缺失时导出包必须整体失败，不能产出残缺附件")
	}
}
