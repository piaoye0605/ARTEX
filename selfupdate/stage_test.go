package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// withPaths 把包内的路径推导换成临时目录。生产代码里所有换装/回滚/暂存都经
// resolvePaths，替掉它之后这些破坏性流程才能安全地跑。
func withPaths(t *testing.T, p Paths) {
	t.Helper()
	old := resolvePaths
	resolvePaths = func() (Paths, error) { return p, nil }
	t.Cleanup(func() { resolvePaths = old })
}

// trustHost 临时把一个测试域名加进白名单。Stage/FetchLatest 全程强制 https +
// 域名白名单，不放开就一个请求都发不出去。
func trustHost(t *testing.T, host string) {
	t.Helper()
	allowedHosts[host] = true
	t.Cleanup(func() { delete(allowedHosts, host) })
}

// fakeExeBytes 返回一段"能执行且 -h 成功退出"的字节，用来充当发布包里的 artex。
func fakeExeBytes(t *testing.T, marker string) []byte {
	t.Helper()
	if runtime.GOOS != "windows" {
		return []byte("#!/bin/sh\necho " + marker + "\nexit 0\n")
	}
	src, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取测试二进制: %v", err)
	}
	// 追加的标记落在 PE 节表之外，加载器忽略；-h 让测试二进制打印用法后以 0 退出。
	return append(b, []byte("\n#"+marker+"\n")...)
}

// releaseZip 按真实发布包的结构造一个 zip：artex-<版本>-<os>-<arch>/artex[.exe]
// 外加若干干扰文件。
func releaseZip(t *testing.T, tag, marker string, withBinary bool) []byte {
	t.Helper()
	dir := "artex-" + strings.TrimPrefix(tag, "v") + "-" + runtime.GOOS + "-" + runtime.GOARCH + "/"
	name := "artex"
	if runtime.GOOS == "windows" {
		name = "artex.exe"
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if withBinary {
		w, err := zw.Create(dir + name)
		if err != nil {
			t.Fatalf("创建 zip 条目: %v", err)
		}
		if _, err := w.Write(fakeExeBytes(t, marker)); err != nil {
			t.Fatalf("写入 zip 条目: %v", err)
		}
	}
	for _, extra := range []string{"README.md", "skills/a.md"} {
		w, err := zw.Create(dir + extra)
		if err != nil {
			t.Fatalf("创建 zip 条目: %v", err)
		}
		_, _ = w.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stageServer 起一个 https 测试服务，托管发布包与 SHA256SUMS。
// sums 为 "" 时返回一个格式无法识别的清单（用来覆盖解析失败分支）。
func stageServer(t *testing.T, tag string, zipBytes []byte, sumsBody string) (*httptest.Server, *Release) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sumsBody))
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	trustHost(t, "127.0.0.1")

	name := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	rel := &Release{
		TagName: tag,
		Assets: []Asset{
			{Name: name, URL: ts.URL + "/pkg.zip", Size: int64(len(zipBytes))},
			{Name: sumsAsset, URL: ts.URL + "/sums"},
		},
	}
	return ts, rel
}

func TestStageHappyPath(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	t.Setenv(smokeEnv, "")

	const tag = "v0.9.9"
	zipBytes := releaseZip(t, tag, "newbuild", true)
	name := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, tag, zipBytes, sha256Hex(zipBytes)+"  "+name+"\n")

	var (
		mu     sync.Mutex
		phases []Phase
		last   int
	)
	prog := func(ph Phase, pct int, msg string) {
		mu.Lock()
		defer mu.Unlock()
		phases = append(phases, ph)
		if ph == PhaseStaged {
			last = pct
		}
		if msg == "" {
			t.Errorf("阶段 %s 的消息不应为空", ph)
		}
	}

	if err := Stage(context.Background(), ts.Client(), rel, "0.9.8", prog); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// 暂存件就位，且内容就是发布包里的二进制。
	if !strings.Contains(readAll(t, p.New), "newbuild") {
		t.Error("artex.new 应是解压出来的新二进制")
	}
	// 暂存件自身校验和单独落一份，下次启动换装前再校验一次。
	if want := readAll(t, p.Sum); want != sha256Hex([]byte(readAll(t, p.New))) {
		t.Errorf("暂存件校验和不对: %s", want)
	}
	m, ok := readMarker(p.Marker)
	if !ok {
		t.Fatal("暂存后应写入升级标记（自动回滚的依据）")
	}
	if m.From != "0.9.8" || m.To != "0.9.9" {
		t.Errorf("标记版本不对: from=%q to=%q", m.From, m.To)
	}
	if m.StagedAt == 0 {
		t.Error("标记应记录暂存时间")
	}
	// 中间文件不能残留，否则下次启动会被当成暂存件重试。
	for _, leftover := range []string{p.New + ".zip.part", p.New + ".part"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("临时文件 %s 应已清理", leftover)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []Phase{PhaseDownload, PhaseVerify, PhaseExtract, PhaseStaged} {
		found := false
		for _, got := range phases {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("缺少阶段 %s（实际 %v）", want, phases)
		}
	}
	if last != 100 {
		t.Errorf("就绪阶段应报 100%%，得到 %d", last)
	}
}

func TestStageRejectsMissingPlatformAsset(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)

	rel := &Release{TagName: "v0.9.9", Assets: []Asset{{Name: "artex-0.9.9-plan9-mips.zip"}}}
	err := Stage(context.Background(), http.DefaultClient, rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "该版本没有提供") {
		t.Fatalf("缺少平台包时应拒绝，得到 %v", err)
	}
}

func TestStageRejectsUnwritableDir(t *testing.T) {
	p := testPaths(t)
	p.Dir = filepath.Join(p.Dir, "does-not-exist")
	withPaths(t, p)

	err := Stage(context.Background(), http.DefaultClient, &Release{TagName: "v0.9.9"}, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "不可写") {
		t.Fatalf("目录不可写时应提前失败（别等下载完再炸），得到 %v", err)
	}
}

func TestStagePropagatesResolveError(t *testing.T) {
	old := resolvePaths
	resolvePaths = func() (Paths, error) { return Paths{}, os.ErrPermission }
	t.Cleanup(func() { resolvePaths = old })

	if err := Stage(context.Background(), http.DefaultClient, &Release{}, "0.9.8", nil); err == nil {
		t.Fatal("路径推导失败应直接返回错误")
	}
}

func TestStageRejectsMissingSumsAsset(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	rel := &Release{
		TagName: "v0.9.9",
		Assets:  []Asset{{Name: name, URL: "https://github.com/x/y.zip", Size: 10}},
	}
	err := Stage(context.Background(), http.DefaultClient, rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "没有 "+sumsAsset) {
		t.Fatalf("缺校验和清单时必须拒绝升级，得到 %v", err)
	}
}

func TestStageRejectsUnlistedAsset(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	zipBytes := releaseZip(t, "v0.9.9", "x", true)
	// 清单里只有别的平台的文件。
	ts, rel := stageServer(t, "v0.9.9", zipBytes, sha256Hex(zipBytes)+"  artex-0.9.9-plan9-mips.zip\n")

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "未收录") {
		t.Fatalf("清单里没有本平台资产时应拒绝，得到 %v", err)
	}
}

func TestStageRejectsUnparseableSums(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	zipBytes := releaseZip(t, "v0.9.9", "x", true)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, "这不是一份 sha256 清单\n")

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "内容为空或格式无法识别") {
		t.Fatalf("无法解析的清单必须拒绝，得到 %v", err)
	}
}

func TestStageRejectsChecksumMismatch(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	zipBytes := releaseZip(t, "v0.9.9", "x", true)
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	wrong := strings.Repeat("a", 64)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, wrong+"  "+name+"\n")

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "SHA256 不匹配") {
		t.Fatalf("校验和不匹配时必须拒绝，得到 %v", err)
	}
	if _, statErr := os.Stat(p.New); !os.IsNotExist(statErr) {
		t.Error("校验失败不能留下暂存件")
	}
	// 错误信息里的摘要要截短，整个 64 位摘要刷在界面上没法读。
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("错误信息应给出截断摘要: %v", err)
	}
}

func TestStageRejectsIncompleteDownload(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	zipBytes := releaseZip(t, "v0.9.9", "x", true)
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, sha256Hex(zipBytes)+"  "+name+"\n")
	// 声明比实际大：模拟连接被掐断后 body 提前结束。
	rel.Assets[0].Size = int64(len(zipBytes)) + 1

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "下载不完整") {
		t.Fatalf("字节数对不上时必须报错，得到 %v", err)
	}
}

func TestStageRejectsHTMLErrorStatus(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	zipBytes := releaseZip(t, "v0.9.9", "x", true)
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, sha256Hex(zipBytes)+"  "+name+"\n")
	rel.Assets[0].URL = ts.URL + "/boom"

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("非 200 应报 HTTP 状态，得到 %v", err)
	}
}

func TestStageRejectsBrokenZip(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	// 校验和是对的，但内容不是 zip——模拟服务端返回了一个 HTML 错误页。
	bogus := []byte("<html>404</html>")
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, "v0.9.9", bogus, sha256Hex(bogus)+"  "+name+"\n")

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "打开发布包") {
		t.Fatalf("非 zip 内容应报打开失败，得到 %v", err)
	}
}

func TestStageRejectsZipWithoutBinary(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	zipBytes := releaseZip(t, "v0.9.9", "x", false)
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, sha256Hex(zipBytes)+"  "+name+"\n")

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "没有找到") {
		t.Fatalf("包内没有可执行文件应报错，得到 %v", err)
	}
}

func TestStageRejectsUnrunnableBinary(t *testing.T) {
	p := testPaths(t)
	withPaths(t, p)
	// zip 里的 artex 存在但无法执行：冒烟测试必须在换装前挡下来。
	dir := "artex-0.9.9-" + runtime.GOOS + "-" + runtime.GOARCH + "/"
	binName := "artex"
	if runtime.GOOS == "windows" {
		binName = "artex.exe"
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(dir + binName)
	_, _ = w.Write([]byte("not an executable"))
	_ = zw.Close()
	zipBytes := buf.Bytes()

	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, sha256Hex(zipBytes)+"  "+name+"\n")

	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", nil)
	if err == nil || !strings.Contains(err.Error(), "无法在当前系统上运行") {
		t.Fatalf("冒烟测试失败时必须拒绝，得到 %v", err)
	}
	if _, statErr := os.Stat(p.New); !os.IsNotExist(statErr) {
		t.Error("冒烟失败不能留下待换装的暂存件")
	}
}

func TestStageKeepsGoingWhenMarkerWriteFails(t *testing.T) {
	p := testPaths(t)
	// 把标记路径指到一个不存在的子目录：写标记必失败，但暂存件已经就位，
	// 不该因为"失去自动回滚能力"而中断本次升级。
	p.Marker = filepath.Join(p.Dir, "nope", "artex.upgrade.json")
	withPaths(t, p)
	t.Setenv(smokeEnv, "")

	zipBytes := releaseZip(t, "v0.9.9", "newbuild", true)
	name := AssetName("v0.9.9", runtime.GOOS, runtime.GOARCH)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, sha256Hex(zipBytes)+"  "+name+"\n")

	var warned bool
	err := Stage(context.Background(), ts.Client(), rel, "0.9.8", func(ph Phase, pct int, msg string) {
		if strings.Contains(msg, "自动回滚") {
			warned = true
		}
	})
	if err != nil {
		t.Fatalf("写标记失败不应中断升级: %v", err)
	}
	if _, err := os.Stat(p.New); err != nil {
		t.Error("暂存件应已就位")
	}
	if !warned {
		t.Error("应通过进度回调告知用户失去了自动回滚保护")
	}
}

func TestFetchSumsPropagatesHTTPError(t *testing.T) {
	zipBytes := releaseZip(t, "v0.9.9", "x", true)
	ts, rel := stageServer(t, "v0.9.9", zipBytes, "")
	for i := range rel.Assets {
		if rel.Assets[i].Name == sumsAsset {
			rel.Assets[i].URL = ts.URL + "/boom"
		}
	}
	if _, err := fetchSums(context.Background(), ts.Client(), rel); err == nil {
		t.Fatal("清单下载失败应报错")
	}
}

func TestGetRejectsNonHTTPS(t *testing.T) {
	if _, err := get(context.Background(), http.DefaultClient, "http://127.0.0.1/x"); err == nil {
		t.Fatal("非 HTTPS 地址必须拒绝")
	}
	if _, err := get(context.Background(), http.DefaultClient, "https://evil.example.com/x"); err == nil {
		t.Fatal("白名单外的域名必须拒绝")
	}
}

func TestProgressWriterThrottles(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
		last  int
		msg   string
	)
	w := &progressWriter{
		total: 1000,
		name:  "pkg.zip",
		last:  time.Now(),
		prog: func(ph Phase, pct int, m string) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			last = pct
			msg = m
		},
	}
	// 300ms 内的连续写入只报一次，否则每个 32KiB 分块都推一条 SSE。
	if n, err := w.Write(make([]byte, 200)); err != nil || n != 200 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, err := w.Write(make([]byte, 300)); err != nil || n != 300 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	mu.Lock()
	if calls != 0 {
		t.Errorf("限频窗口内不应上报，得到 %d 次", calls)
	}
	mu.Unlock()

	// 过了窗口就该上报，且百分比按已写字节算。
	time.Sleep(320 * time.Millisecond)
	if _, err := w.Write(make([]byte, 500)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("窗口外应上报一次，得到 %d 次", calls)
	}
	if last != 100 {
		t.Errorf("进度百分比 = %d，期望 100", last)
	}
	if !strings.Contains(msg, "下载中") {
		t.Errorf("进度文案异常: %q", msg)
	}
}

func TestProgressWriterWithoutTotal(t *testing.T) {
	var pct int
	w := &progressWriter{prog: func(_ Phase, p int, _ string) {
		pct = p
	}, last: time.Now().Add(-time.Second)}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	// 服务端没给 Content-Length 时不能瞎报百分比。
	if pct != -1 {
		t.Errorf("未知总大小时 pct 应为 -1，得到 %d", pct)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		512:                              "512 B",
		1024:                             "1.0 KB",
		1536:                             "1.5 KB",
		1 << 20:                          "1.0 MB",
		3 << 30:                          "3.0 GB",
		1 << 40:                          "1.0 TB",
		1024 * 1024 * 1024 * 1024 * 1024: "1024.0 TB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q，期望 %q", in, got, want)
		}
	}
}

func TestShortDigest(t *testing.T) {
	long := strings.Repeat("a", 64)
	if got := short(long); got != strings.Repeat("a", 12)+"…" {
		t.Errorf("长摘要应截断到 12 位，得到 %q", got)
	}
	if got := short("deadbeef"); got != "deadbeef" {
		t.Errorf("短摘要应原样返回，得到 %q", got)
	}
}
