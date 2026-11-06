package traffic

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDeleteAllClearsIndexTreesAndBlobs: 「清空全部」必须把索引、历史目录和
// 内容寻址 blob 一起清干净，并顺手压实索引把空间还给文件系统。
func TestDeleteAllClearsIndexTreesAndBlobs(t *testing.T) {
	tr, dir := openTraffic(t)
	big := bytes.Repeat([]byte("Z"), maxInlineBody+64)
	tr.record(newFlow("sweep.example", "GET", "/1", nil, []byte("small")))
	tr.record(newFlow("sweep.example", "GET", "/2", nil, big, withRespType("text/plain")))

	// 老版本留下的 host 目录：即使索引里没有 path 指向它，也要被清空动作带走。
	legacy := filepath.Join(dir, "sweep.example")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(legacy, "keep.txt"), "legacy")

	blobDir := filepath.Join(dir, "_blobs", "sha256")
	deleted, reclaimed, err := tr.DeleteAll()
	if err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("删除条数 = %d, want 2", deleted)
	}
	if reclaimed < 0 {
		t.Fatalf("回收字节数为负: %d", reclaimed)
	}
	for _, table := range []string{"exchanges", "exchange_bodies", "blob_refs"} {
		var n int
		if err := tr.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("统计 %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s 残留 %d 行", table, n)
		}
	}
	// 清理是后台跑的，Close 会等它结束。
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("历史 host 目录未清理: %v", err)
	}
	entries, err := os.ReadDir(blobDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				bucket := filepath.Join(blobDir, e.Name())
				if files, _ := os.ReadDir(bucket); len(files) > 0 {
					t.Fatalf("blob 未回收: %s", bucket)
				}
			}
		}
	}
}

func TestDeleteAllEmptyStore(t *testing.T) {
	tr, _ := openTraffic(t)
	deleted, _, err := tr.DeleteAll()
	if err != nil || deleted != 0 {
		t.Fatalf("空库清空 deleted=%d err=%v", deleted, err)
	}
}

// TestHostDeleteStageRollbackRestoresEverything: 回滚必须同时还原索引行与磁盘目录，
// 并且重复调用是幂等的（调用方可能在错误路径上重复收尾）。
func TestHostDeleteStageRollbackRestoresEverything(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("rb.example", "GET", "/x", nil, []byte("body")))
	legacy := filepath.Join(dir, "rb.example")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(legacy, "request.http"), "GET / HTTP/1.1\n\n")

	stage, err := tr.StageDeleteHostsExact([]string{"rb.example", "rb.example"})
	if err != nil {
		t.Fatal(err)
	}
	if stage.Deleted() != 1 {
		t.Fatalf("去重后应只删 1 条，得到 %d", stage.Deleted())
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("暂存阶段目录应已移走: %v", err)
	}
	if err := stage.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := stage.Rollback(); err != nil {
		t.Fatalf("重复 Rollback 应无操作: %v", err)
	}
	if err := stage.Commit(); err != nil {
		t.Fatalf("已收尾的 stage 再 Commit 应无操作: %v", err)
	}
	if n := countExchanges(t, tr); n != 1 {
		t.Fatalf("回滚后索引行 = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(legacy, "request.http")); err != nil {
		t.Fatalf("回滚后目录未还原: %v", err)
	}
}

func TestNilHostDeleteStageIsSafe(t *testing.T) {
	var stage *HostDeleteStage
	if stage.Deleted() != 0 {
		t.Fatal("nil stage 删除数应为 0")
	}
	if err := stage.Rollback(); err != nil {
		t.Fatalf("nil stage Rollback 应无操作: %v", err)
	}
	if err := stage.Commit(); err != nil {
		t.Fatalf("nil stage Commit 应无操作: %v", err)
	}
}

func TestStageDeleteHostsExactForArchiveRejectsBadIDs(t *testing.T) {
	tr, _ := openTraffic(t)
	for _, tc := range []struct {
		archive, task int64
	}{
		{0, 1}, {1, 0}, {-1, 5},
	} {
		if _, err := tr.StageDeleteHostsExactForArchive([]string{"x"}, tc.archive, tc.task); err == nil {
			t.Fatalf("archive=%d task=%d 应被拒绝", tc.archive, tc.task)
		}
	}
}

// TestRecoverHostDeleteStagesBranches 覆盖重启恢复的各种残局：无日志、坏日志、
// 版本不支持、越界路径、缺少归档解析器，以及「行已删干净」的完成分支。
func TestRecoverHostDeleteStagesBranches(t *testing.T) {
	// 没有 _delete_staging 目录是常态，应静默通过。
	empty, _ := openTraffic(t)
	if err := empty.RecoverHostDeleteStages(nil); err != nil {
		t.Fatalf("无暂存目录应通过: %v", err)
	}
	var nilTraffic *Traffic
	if err := nilTraffic.RecoverHostDeleteStages(nil); err != nil {
		t.Fatalf("空存储应通过: %v", err)
	}

	cases := []struct {
		name     string
		stage    func(t *testing.T, dir, stageDir string)
		resolver func(int64, int64) (bool, error)
		// nilResolver 让用例显式传 nil：恢复流程必须拒绝无解析器的归档残局。
		nilResolver bool
		wantErr     string
	}{
		{
			name:  "没有日志的老目录保留待人工检查",
			stage: func(t *testing.T, dir, stageDir string) { writeFile(t, filepath.Join(stageDir, "orphan.txt"), "x") },
		},
		{
			name: "日志损坏",
			stage: func(t *testing.T, dir, stageDir string) {
				writeFile(t, filepath.Join(stageDir, hostDeleteStageJournalName), "{not json")
			},
			wantErr: "读取流量暂存日志",
		},
		{
			name: "版本不支持",
			stage: func(t *testing.T, dir, stageDir string) {
				writeJournal(t, stageDir, hostDeleteStageJournal{Version: 2})
			},
			wantErr: "版本 2 不受支持",
		},
		{
			name: "越界路径",
			stage: func(t *testing.T, dir, stageDir string) {
				writeJournal(t, stageDir, hostDeleteStageJournal{Version: 1, Moves: []hostDeleteStageMove{
					{Source: filepath.Join(dir, "..", "escape"), Staged: filepath.Join(stageDir, "0-x")},
				}})
			},
			wantErr: "越界路径",
		},
		{
			name: "归档缺少状态解析器",
			stage: func(t *testing.T, dir, stageDir string) {
				writeJournal(t, stageDir, hostDeleteStageJournal{Version: 1, ArchiveID: 9, TaskID: 9})
			},
			nilResolver: true,
			wantErr:     "无状态解析器",
		},
		{
			name: "归档解析失败",
			stage: func(t *testing.T, dir, stageDir string) {
				writeJournal(t, stageDir, hostDeleteStageJournal{Version: 1, ArchiveID: 9, TaskID: 9})
			},
			wantErr: "归档库挂了",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, dir := openTraffic(t)
			stageDir := filepath.Join(dir, "_delete_staging", "hosts-x")
			if err := os.MkdirAll(stageDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.stage(t, dir, stageDir)
			resolver := tc.resolver
			if tc.nilResolver {
				resolver = nil
			} else if resolver == nil {
				resolver = func(int64, int64) (bool, error) { return false, errors.New("归档库挂了") }
			}
			err := tr.RecoverHostDeleteStages(resolver)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过，得到 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v, want 包含 %q", err, tc.wantErr)
			}
		})
	}
}

// TestRecoverCompletesHostOnlyDeletion: 没有归档信息时用「该 host 还有没有索引行」
// 判断 PostgreSQL 侧是否已提交；已删干净就补做收尾（含 blob 回收）。
func TestRecoverCompletesHostOnlyDeletion(t *testing.T) {
	tr, dir := openTraffic(t)
	const host = "done.example"
	tr.record(newFlow(host, "GET", "/x", nil, []byte("payload")))
	legacy := filepath.Join(dir, sanitize(host))
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	stage, err := tr.StageDeleteHostsExact([]string{host})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟进程退出：SQLite 事务丢失，但目录已移入暂存区，日志还在。
	simulateTrafficStageProcessExit(t, stage)
	// 模拟 PostgreSQL 侧已提交：恢复时该 host 已经没有索引行。
	if _, err := tr.DB().Exec(`DELETE FROM exchange_bodies WHERE id IN (SELECT id FROM exchanges WHERE host=?)`, host); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DB().Exec(`DELETE FROM exchanges WHERE host=?`, host); err != nil {
		t.Fatal(err)
	}

	if err := tr.RecoverHostDeleteStages(nil); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_delete_staging", "hosts-x")); err == nil {
		t.Fatal("暂存目录不该被创建")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "_delete_staging"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("暂存目录未清理: %v", entries)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("已提交删除的历史目录应被清掉: %v", err)
	}
}

// TestRecoverRestoresWhenHostStillHasRows: 反过来的局面——PostgreSQL 没提交，
// 索引行还在，就必须把目录搬回去而不是继续删。
func TestRecoverRestoresWhenHostStillHasRows(t *testing.T) {
	tr, dir := openTraffic(t)
	const host = "pending.example"
	tr.record(newFlow(host, "GET", "/x", nil, []byte("payload")))
	legacy := filepath.Join(dir, sanitize(host))
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(legacy, "request.http"), "GET / HTTP/1.1\n\n")

	stage, err := tr.StageDeleteHostsExact([]string{host})
	if err != nil {
		t.Fatal(err)
	}
	simulateTrafficStageProcessExit(t, stage)

	if err := tr.RecoverHostDeleteStages(nil); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "request.http")); err != nil {
		t.Fatalf("未提交的删除应把目录还原: %v", err)
	}
	if n := countExchanges(t, tr); n != 1 {
		t.Fatalf("未提交的删除不应丢索引行，实际 %d", n)
	}
}

func TestExportHostsLegacyTreeAndBlobs(t *testing.T) {
	tr, dir := openTraffic(t)
	const legacyHost = "legacy-export.example"
	const legacy = "legacy-export.example/0001"
	if err := os.MkdirAll(filepath.Join(dir, legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, legacy, "request.http"), "GET /old HTTP/1.1\n\nold-request")
	writeFile(t, filepath.Join(dir, legacy, "response.http"), "HTTP/1.1 200 OK\n\nold-response")
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('legacy-export',1,?,'GET','/old','http://legacy-export.example/old',200,'text/plain',11,12,?)`, legacyHost, legacy); err != nil {
		t.Fatal(err)
	}
	// 另一个 host 有超过内联阈值的正文 → 归档要带上 blob 文件。
	big := bytes.Repeat([]byte("Q"), maxInlineBody+32)
	tr.record(newFlow("blob-export.example", "GET", "/big", nil, big, withRespType("text/plain")))

	if n, err := tr.ExportHosts(nil, filepath.Join(t.TempDir(), "empty")); err != nil || n != 0 {
		t.Fatalf("无 host 应导出 0 条: %d %v", n, err)
	}
	var nilTraffic *Traffic
	if n, err := nilTraffic.ExportHosts([]string{"x"}, t.TempDir()); err != nil || n != 0 {
		t.Fatalf("空存储应导出 0 条: %d %v", n, err)
	}

	out := filepath.Join(t.TempDir(), "snapshot")
	n, err := tr.ExportHosts([]string{legacyHost, "blob-export.example"}, out)
	if err != nil {
		t.Fatalf("ExportHosts: %v", err)
	}
	if n != 2 {
		t.Fatalf("导出条数 = %d, want 2", n)
	}
	raw, err := os.ReadFile(filepath.Join(out, "traffic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap ArchiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Exchanges) != 2 || len(snap.Blobs) != 1 {
		t.Fatalf("归档清单不对: %d 条, %d 个 blob", len(snap.Exchanges), len(snap.Blobs))
	}
	for _, item := range snap.Exchanges {
		if item.ID == "legacy-export" {
			// 旧目录的正文没有头部/正文分离，整份文件内容进 ReqHead/RespHead。
			if !strings.Contains(item.ReqHead, "old-request") || !strings.Contains(item.RespHead, "old-response") {
				t.Fatalf("旧目录正文未进归档: %+v", item)
			}
		} else if item.RespBlob == "" {
			t.Fatalf("大正文应记为 blob 引用: %+v", item)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "blobs", snap.Blobs[0]+".bin")); err != nil {
		t.Fatalf("blob 文件未复制进归档: %v", err)
	}

	// 目录位置被文件占了 → 报错而不是写出半份归档。
	blocker := filepath.Join(t.TempDir(), "blocked")
	writeFile(t, blocker, "x")
	if _, err := tr.ExportHosts([]string{legacyHost}, blocker); err == nil {
		t.Fatal("归档目录不可创建时应报错")
	}

	// blob 文件丢失 → 导出失败（宁可失败，也不能产出缺内容的归档）。
	var hash string
	if err := tr.DB().QueryRow(`SELECT resp_blob FROM exchange_bodies WHERE resp_blob IS NOT NULL LIMIT 1`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "_blobs", "sha256", hash[:2], hash+".bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ExportHosts([]string{"blob-export.example"}, filepath.Join(t.TempDir(), "broken")); err == nil {
		t.Fatal("blob 缺失时导出应失败")
	}
}

// TestImportArchiveRejectsBadInput: 归档来自外部磁盘，任何一处不可信都不能导入。
func TestImportArchiveRejectsBadInput(t *testing.T) {
	tr, _ := openTraffic(t)

	var nilTraffic *Traffic
	if n, err := nilTraffic.ImportArchive(t.TempDir()); err != nil || n != 0 {
		t.Fatalf("空存储应导入 0 条: %d %v", n, err)
	}
	if n, err := tr.ImportArchive(t.TempDir()); err != nil || n != 0 {
		t.Fatalf("缺少 traffic.json 应静默跳过: %d %v", n, err)
	}

	cases := []struct {
		name    string
		write   func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name: "清单损坏",
			write: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "traffic.json"), "{nope")
			},
			wantErr: "invalid character",
		},
		{
			name: "版本不支持",
			write: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, "traffic.json"), ArchiveSnapshot{Version: 7})
			},
			wantErr: "unsupported traffic archive version 7",
		},
		{
			name: "blob 哈希非法",
			write: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, "traffic.json"), ArchiveSnapshot{Version: 1, Blobs: []string{"../../etc/passwd"}})
			},
			wantErr: "invalid archived traffic blob",
		},
		{
			name: "blob 文件缺失",
			write: func(t *testing.T, dir string) {
				hash := strings.Repeat("a", 64)
				writeJSON(t, filepath.Join(dir, "traffic.json"), ArchiveSnapshot{Version: 1, Blobs: []string{hash}})
			},
			wantErr: "blobs",
		},
		{
			name: "blob 校验和不匹配",
			write: func(t *testing.T, dir string) {
				// 32 字节的 b，sha256 不等于全 a。
				hash := strings.Repeat("a", 64)
				if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(dir, "blobs", hash+".bin"), "not-the-right-content")
				writeJSON(t, filepath.Join(dir, "traffic.json"), ArchiveSnapshot{Version: 1, Blobs: []string{hash}})
			},
			wantErr: "checksum mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.write(t, dir)
			_, err := tr.ImportArchive(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v, want 包含 %q", err, tc.wantErr)
			}
		})
	}
}

func writeJournal(t *testing.T, stageDir string, journal hostDeleteStageJournal) {
	t.Helper()
	writeJSON(t, filepath.Join(stageDir, hostDeleteStageJournalName), journal)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestWaitForAsyncRemoval 只是保证「后台清理」这一路径在测试里也有观察点：
// reapStage 之后 Close 会等待，不会留下临时目录。
func TestCloseWaitsForStagedCleanup(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("async.example", "GET", "/x", nil, []byte("b")))
	legacy := filepath.Join(dir, sanitize("async.example"))
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	stage, err := tr.StageDeleteHostsExact([]string{"async.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Join(dir, "_delete_staging"))
		if err == nil && len(entries) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("暂存目录未清理干净: %v", entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOpenRejectsFilePath: 数据目录被同名文件占住时必须在启动期报错，而不是
// 等到第一次记录才炸。
func TestOpenRejectsFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic")
	writeFile(t, path, "x")
	if _, err := Open(path, "127.0.0.1:0"); err == nil {
		t.Fatal("数据目录不可创建时 Open 应报错")
	}
}

func TestWriteHostDeleteStageJournalRejectsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), hostDeleteStageJournalName)
	if err := writeHostDeleteStageJournal(path, hostDeleteStageJournal{Version: 1}); err != nil {
		t.Fatal(err)
	}
	// O_EXCL：同一暂存目录里的日志绝不能被覆盖，否则恢复流程会读到半份状态。
	if err := writeHostDeleteStageJournal(path, hostDeleteStageJournal{Version: 1}); err == nil {
		t.Fatal("已存在的日志应拒绝写入")
	}
}

// TestStageDeleteHostsExactFailsWhenStagingBlocked: 暂存目录建不出来时整个删除
// 必须回滚（索引行不动、写锁释放），不能留下删了一半的状态。
func TestStageDeleteHostsExactFailsWhenStagingBlocked(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("blocked.example", "GET", "/", nil, []byte("body")))
	writeFile(t, filepath.Join(dir, "_delete_staging"), "not a dir")

	if _, err := tr.StageDeleteHostsExact([]string{"blocked.example"}); err == nil {
		t.Fatal("暂存目录不可创建时应报错")
	}
	if n := countExchanges(t, tr); n != 1 {
		t.Fatalf("失败后索引行应保留，实际 %d", n)
	}
	// 写锁必须已释放：还能继续记录说明没有死锁。
	tr.record(newFlow("blocked.example", "GET", "/after", nil, []byte("ok")))
	if n := countExchanges(t, tr); n != 2 {
		t.Fatalf("失败后应仍可写入，实际 %d 行", n)
	}
}

func TestDeleteAllFailsWhenDataDirGone(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("gone.example", "GET", "/", nil, nil))
	// Windows 上删不掉仍被 SQLite 打开的索引文件，所以只把数据目录指针指到
	// 一个不存在的路径：等价于「目录在运行中被外部清掉」。
	tr.dir = filepath.Join(dir, "vanished")
	if _, _, err := tr.DeleteAll(); err == nil {
		t.Fatal("数据目录消失时 DeleteAll 应报错")
	}
}
