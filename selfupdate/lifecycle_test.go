package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBootstrapSkipsInSmokeMode 覆盖冒烟测试子进程的短路：子进程绝不能再走一遍
// 换装逻辑，否则会把自己的暂存件当成待安装版本。
func TestBootstrapSkipsInSmokeMode(t *testing.T) {
	t.Setenv(smokeEnv, "1")
	action, st := Bootstrap()
	if action != Continue {
		t.Fatalf("冒烟模式下应 Continue，得到 %v", action)
	}
	if st.Pending || st.RolledBack || st.FailedStage || st.Detail != "" {
		t.Errorf("冒烟模式下状态应为空，得到 %+v", st)
	}
}

func TestBootstrapPlainStart(t *testing.T) {
	t.Setenv(smokeEnv, "")
	withPaths(t, testPaths(t))

	action, st := Bootstrap()
	if action != Continue {
		t.Fatalf("无暂存件无标记时应 Continue，得到 %v", action)
	}
	if st.Pending || st.RolledBack || st.FailedStage {
		t.Errorf("普通启动状态应全为 false，得到 %+v", st)
	}
}

func TestBootstrapAppliesStagedBinary(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	withPaths(t, p)

	action, st := Bootstrap()
	if action != Restart {
		t.Fatalf("暂存件校验通过应要求重启，得到 %v", action)
	}
	if !st.Pending {
		t.Error("刚换装完应标记 Pending（下次启动确认稳定性）")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("应已换装到新版本")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧版本应备份下来，供自动回滚")
	}
	// 标记要留着，下次启动（跑新版）靠它计数并在连续崩溃时回滚。
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("换装后必须保留升级标记")
	}
}

func TestBootstrapDiscardsBadStagedBinary(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	// 暂存后文件被掉包：校验和不再匹配。
	fakeUnrunnable(t, p.New)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	action, st := Bootstrap()
	if action != Continue {
		t.Fatalf("校验失败时应继续跑旧版，得到 %v", action)
	}
	if !st.FailedStage || st.Detail == "" {
		t.Errorf("状态应说明暂存失败，得到 %+v", st)
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Error("校验失败时绝不能动当前版本")
	}
	for _, leftover := range []string{p.New, p.Sum, p.Marker} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s 应被清理，否则每次启动都会重试", leftover)
		}
	}
}

func TestBootstrapCountsAttemptsThenRollsBack(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: maxAttempts}); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	action, st := Bootstrap()
	if action != Restart {
		t.Fatalf("超过尝试上限应回滚并重启，得到 %v", action)
	}
	if !st.RolledBack {
		t.Error("状态应标记 RolledBack")
	}
	if !strings.Contains(st.Detail, "0.3.7") {
		t.Errorf("回滚说明应带上回退到的版本: %q", st.Detail)
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Error("应已换回旧版本")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("回滚后标记必须清除，否则会无限回滚")
	}
}

func TestBootstrapRollbackFailureStopsRestarting(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	// 没有 .old：回滚无从下手。
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: maxAttempts}); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	action, st := Bootstrap()
	if action != Continue {
		t.Fatalf("回滚失败时必须停止重启循环，得到 %v", action)
	}
	if !strings.Contains(st.Detail, "回滚失败") {
		t.Errorf("状态应说明回滚失败: %+v", st)
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("回滚失败后也要清标记，否则每次启动都重试")
	}
}

func TestBootstrapPendingAttemptDetail(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	action, st := Bootstrap()
	if action != Continue {
		t.Fatalf("还在尝试期内应正常启动，得到 %v", action)
	}
	if !st.Pending {
		t.Error("应标记 Pending")
	}
	got, ok := readMarker(p.Marker)
	if !ok || got.Attempts != 2 {
		t.Errorf("尝试次数应累加到 2，得到 %+v (ok=%v)", got, ok)
	}
}

func TestBootstrapIgnoresCorruptMarker(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	if err := os.WriteFile(p.Marker, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	if action, st := Bootstrap(); action != Continue || st.Pending || st.RolledBack {
		t.Fatalf("损坏的标记应被忽略，得到 %v %+v", action, st)
	}
}

func TestSettleClearsMarkerThroughWrapper(t *testing.T) {
	p := testPaths(t)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	Settle()
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("稳定运行后标记应清除")
	}
}

func TestSettleSkipsWhenPathsUnavailable(t *testing.T) {
	old := resolvePaths
	resolvePaths = func() (Paths, error) { return Paths{}, os.ErrPermission }
	t.Cleanup(func() { resolvePaths = old })

	Settle() // 不 panic 即为通过
}

func TestRollbackSwapsAndIsReversible(t *testing.T) {
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.6", To: "0.3.7"}); err != nil {
		t.Fatal(err)
	}
	withPaths(t, p)

	if err := Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("当前版本应退回 v1")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("备份应变成 v2，这样还能再滚回去")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("手动回滚后标记应清除")
	}
}

func TestRollbackRejectsMissingBackup(t *testing.T) {
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	withPaths(t, p)

	err := Rollback()
	if err == nil || !strings.Contains(err.Error(), "没有可回滚的上一版本") {
		t.Fatalf("没有备份时应拒绝，得到 %v", err)
	}
}

func TestRollbackRejectsUnrunnableBackup(t *testing.T) {
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeUnrunnable(t, p.Old)
	withPaths(t, p)

	err := Rollback()
	if err == nil || !strings.Contains(err.Error(), "拒绝回滚") {
		t.Fatalf("上一版本跑不起来时必须拒绝回滚（否则更糟），得到 %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v2") {
		t.Error("拒绝回滚时不能动当前版本")
	}
}

func TestHasBackup(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	if HasBackup() {
		t.Error("没有 .old 时不应报告有备份")
	}
	if err := os.WriteFile(p.Old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasBackup() {
		t.Error("存在 .old 时应报告有备份")
	}
}

func TestSwapErrors(t *testing.T) {
	t.Run("清理旧备份失败", func(t *testing.T) {
		p := testPaths(t)
		// 非空目录无法删除：Windows 与 Unix 都拒绝。
		if err := os.MkdirAll(filepath.Join(p.Old, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		fakeBin(t, p.Current, "cur", 0)
		stage(t, p, "new", 0)
		err := swap(p)
		if err == nil || !strings.Contains(err.Error(), "清理旧备份") {
			t.Fatalf("得到 %v", err)
		}
	})

	t.Run("当前版本不存在", func(t *testing.T) {
		p := testPaths(t)
		stage(t, p, "new", 0)
		err := swap(p)
		if err == nil || !strings.Contains(err.Error(), "备份当前版本") {
			t.Fatalf("得到 %v", err)
		}
	})

	t.Run("装入失败后回滚当前版本", func(t *testing.T) {
		p := testPaths(t)
		fakeBin(t, p.Current, "cur", 0)
		// 没有 .new：第二次 rename 必失败，此时必须把已挪走的当前版本放回去。
		err := swap(p)
		if err == nil || !strings.Contains(err.Error(), "装入新版本") {
			t.Fatalf("得到 %v", err)
		}
		if _, statErr := os.Stat(p.Current); statErr != nil {
			t.Fatal("装入失败后必须恢复当前版本，否则下次启动没有可执行文件")
		}
		if !strings.Contains(readAll(t, p.Current), "cur") {
			t.Error("恢复的应是原来的当前版本")
		}
	})
}

func TestApplyStagedKeepsGoingWhenMarkerWriteFails(t *testing.T) {
	t.Setenv(smokeEnv, "")
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	// 标记路径不可写：换装本身已经完成，不该因此回退。
	p.Marker = filepath.Join(p.Dir, "nope", "artex.upgrade.json")
	withPaths(t, p)

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("换装成功即应重启，得到 %v", action)
	}
	if !st.Pending {
		t.Error("状态应为 Pending")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("应已换装")
	}
}

func TestConfirmRollbackWriteFailureStillPending(t *testing.T) {
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	p.Marker = filepath.Join(p.Dir, "nope", "artex.upgrade.json")
	withPaths(t, p)

	action, st := confirmOrRollback(p, marker{From: "0.3.7", To: "0.3.8"})
	if action != Continue || !st.Pending {
		t.Fatalf("写标记失败不该阻断启动，得到 %v %+v", action, st)
	}
}

func TestVerifyStagedMissingSum(t *testing.T) {
	p := testPaths(t)
	fakeBin(t, p.New, "new", 0)
	err := verifyStaged(p)
	if err == nil || !strings.Contains(err.Error(), "读取校验和") {
		t.Fatalf("缺校验和文件时应报错，得到 %v", err)
	}
}

func TestFileSHA256MissingFile(t *testing.T) {
	if _, err := fileSHA256(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("文件不存在时应报错")
	}
}

func TestOrUnknown(t *testing.T) {
	if got := orUnknown("  "); got != "未知版本" {
		t.Errorf("空版本号应显示为未知版本，得到 %q", got)
	}
	if got := orUnknown("0.3.8"); got != "0.3.8" {
		t.Errorf("非空版本号应原样返回，得到 %q", got)
	}
}

func TestReadMarkerRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readMarker(path); ok {
		t.Error("结构不对的标记应视为不存在")
	}
	if _, ok := readMarker(filepath.Join(dir, "absent.json")); ok {
		t.Error("文件不存在应视为没有标记")
	}
}

func TestInDockerDoesNotPanic(t *testing.T) {
	// 结果随环境变化（CI 的容器里可能是 true），这里只要求判定逻辑本身可用。
	_ = InDocker()
}

func TestNewClientProxyAndRedirects(t *testing.T) {
	direct := NewClient("")
	tr, ok := direct.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 应是 *http.Transport")
	}
	if tr.Proxy != nil {
		t.Error("未配置代理时不应有 Proxy 函数")
	}
	if direct.Timeout <= 0 {
		t.Error("下载整包不能用请求级超时卡死，应设置客户端总超时")
	}

	proxied := NewClient("  http://127.0.0.1:7890  ")
	if tr, ok := proxied.Transport.(*http.Transport); !ok || tr.Proxy == nil {
		t.Error("配置了代理时应装上 Proxy")
	}
	// 解析不了的代理地址不能把整个客户端搞坏，按直连处理。
	if tr, ok := NewClient("://bad").Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Error("非法代理地址应退化为直连")
	}

	// 重定向是绕过白名单最省事的路子，必须每一跳都校验。
	via := make([]*http.Request, 10)
	if err := direct.CheckRedirect(reqFor(t, "https://github.com/x"), via); err == nil {
		t.Error("重定向超过 10 跳应失败")
	}
	if err := direct.CheckRedirect(reqFor(t, "https://github.com/x"), nil); err != nil {
		t.Errorf("白名单内的重定向应放行: %v", err)
	}
	if err := direct.CheckRedirect(reqFor(t, "https://evil.example.com/x"), nil); err == nil {
		t.Error("重定向到白名单外必须拒绝")
	}
}

// fetchServer 起一个 https 服务扮演 GitHub API，并临时把 latestURL 指过去。
func fetchServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	trustHost(t, "127.0.0.1")
	old := latestURL
	latestURL = ts.URL + "/latest"
	t.Cleanup(func() { latestURL = old })
	return ts
}

func TestFetchLatestParsesRelease(t *testing.T) {
	published := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	ts := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); !strings.Contains(got, "github") {
			t.Errorf("Accept 头异常: %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "artex-selfupdate" {
			t.Errorf("User-Agent 头异常: %q", got)
		}
		_ = json.NewEncoder(w).Encode(Release{
			TagName:     "v0.9.9",
			Name:        "0.9.9",
			Body:        "notes",
			PublishedAt: published,
			HTMLURL:     "https://github.com/x/y",
			Assets:      []Asset{{Name: "artex-0.9.9-linux-amd64.zip", URL: "https://github.com/x/a.zip", Size: 42}},
		})
	})

	rel, err := FetchLatest(context.Background(), ts.Client())
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if rel.TagName != "v0.9.9" || rel.Body != "notes" {
		t.Errorf("解析结果不对: %+v", rel)
	}
	if !rel.PublishedAt.Equal(published) {
		t.Errorf("发布时间解析错误: %v", rel.PublishedAt)
	}
	a, ok := rel.FindAsset("ARTEX-0.9.9-linux-amd64.zip") // 大小写不敏感
	if !ok || a.Size != 42 {
		t.Errorf("FindAsset 未按名字匹配: %+v ok=%v", a, ok)
	}
	if _, ok := rel.FindAsset("nope.zip"); ok {
		t.Error("不存在的资产不应匹配")
	}
}

func TestFetchLatestErrorPaths(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"限流403", http.StatusForbidden, "", "限流"},
		{"限流429", http.StatusTooManyRequests, "", "限流"},
		{"无发布404", http.StatusNotFound, "", "尚未发布"},
		{"其它状态", http.StatusInternalServerError, "", "GitHub 返回 500"},
		{"坏JSON", http.StatusOK, "{", "解析 Release 失败"},
		{"缺tag", http.StatusOK, `{"tag_name":"  "}`, "缺少 tag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := FetchLatest(context.Background(), ts.Client())
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("期望错误含 %q，得到 %v", tc.wantSub, err)
			}
		})
	}
}

func TestFetchLatestNetworkFailure(t *testing.T) {
	ts := fetchServer(t, func(w http.ResponseWriter, r *http.Request) {})
	ts.Close() // 服务已关，连接必失败
	_, err := FetchLatest(context.Background(), ts.Client())
	if err == nil || !strings.Contains(err.Error(), "访问 GitHub 失败") {
		t.Fatalf("网络失败应给出可操作提示（可配代理），得到 %v", err)
	}
}

func TestFetchLatestRejectsNonWhitelistedURL(t *testing.T) {
	old := latestURL
	latestURL = "https://evil.example.com/latest"
	t.Cleanup(func() { latestURL = old })

	if _, err := FetchLatest(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("白名单外的 URL 必须拒绝")
	}
}

// reqFor 造一个只带 URL 的请求，供 CheckRedirect 断言用。
func reqFor(t *testing.T, raw string) *http.Request {
	t.Helper()
	return &http.Request{URL: mustParse(t, raw)}
}
