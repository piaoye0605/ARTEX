package traffic

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// TestSinkResponseRequiresBothSides: 控制连接（CONNECT 后的 TLS 失败等）也会
// 触发 Response 回调，但只有同时拿到请求与响应才算一次完整交换。
func TestSinkResponseRequiresBothSides(t *testing.T) {
	tr, _ := openTraffic(t)
	s := &sink{t: tr}

	s.Response(&mproxy.Flow{})
	s.Response(&mproxy.Flow{Request: newFlow("empty.example", "GET", "/", nil, nil).Request})
	if n := countExchanges(t, tr); n != 0 {
		t.Fatalf("半截 flow 不应落库，实际 %d 条", n)
	}

	s.Response(newFlow("full.example", "GET", "/x", nil, []byte("ok")))
	if n := countExchanges(t, tr); n != 1 {
		t.Fatalf("完整 flow 应落库 1 条，实际 %d 条", n)
	}
}

// TestMaybePassthroughMarksOnlyProxyFaults covers the fail-open switch: a host
// is tunneled transparently only when the interception layer itself broke.
func TestMaybePassthroughMarksOnlyProxyFaults(t *testing.T) {
	tr, _ := openTraffic(t)
	flow := newFlow("broken.example:8443", "GET", "/", nil, nil)

	// Every early-return guard.
	tr.maybePassthrough(nil, errors.New("http2: boom"))
	tr.maybePassthrough(nil, nil)
	tr.maybePassthrough(&mproxy.Flow{}, errors.New("http2: boom"))
	tr.maybePassthrough(&mproxy.Flow{Request: &mproxy.Request{}}, errors.New("http2: boom"))
	// Target-side failure: stays intercepted.
	tr.maybePassthrough(flow, errors.New("connect: connection refused"))
	if _, hit := tr.pass.Load("broken.example"); hit {
		t.Fatal("目标不可达不应触发透传")
	}

	tr.maybePassthrough(flow, errors.New("http2: frame too large"))
	if _, hit := tr.pass.Load("broken.example"); !hit {
		t.Fatal("代理自身故障应标记该 host 透传（端口需要被剥掉）")
	}

	// sink.RequestError 只是转发，同样要生效。
	other := newFlow("second.example", "GET", "/", nil, nil)
	(&sink{t: tr}).RequestError(other, errors.New("malformed HTTP response"))
	if _, hit := tr.pass.Load("second.example"); !hit {
		t.Fatal("RequestError 未转发到透传标记")
	}
}

func TestCACertPath(t *testing.T) {
	tr := &Traffic{dir: filepath.Join("data", "traffic")}
	want := filepath.Join("data", "traffic", "_ca", "mitmproxy-ca-cert.pem")
	if got := tr.CACertPath(); got != want {
		t.Fatalf("CACertPath() = %q, want %q", got, want)
	}
}

func TestClip(t *testing.T) {
	if got := clip("short", 10); got != "short" {
		t.Fatalf("未超长不应改写，得到 %q", got)
	}
	got := clip("0123456789", 4)
	if !strings.HasPrefix(got, "0123") {
		t.Fatalf("截断前缀不对: %q", got)
	}
	if !strings.Contains(got, "共 10 字节") {
		t.Fatalf("截断提示缺少总长度: %q", got)
	}
}

// TestStartFailsOnBusyAddress 覆盖 Start 的唯一确定性失败路径：端口已被占用时
// go-mitmproxy 直接把监听错误返回给调用方，而不是静默退出。
func TestStartFailsOnBusyAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tr, err := Open(t.TempDir(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if err := tr.Start(); err == nil {
		t.Fatal("端口被占用时 Start 应返回错误")
	}
}

// TestStartRecordsThroughProxy 是全链路冒烟：真实监听、真实转发、真实落库。
func TestStartRecordsThroughProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	defer target.Close()

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	tr, err := Open(t.TempDir(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tr.proxy.Close()
		_ = tr.Close()
	})
	go func() { _ = tr.Start() }()
	waitListening(t, addr)

	proxyURL, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	resp, err := client.Get(target.URL + "/e2e")
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "hello /e2e") {
		t.Fatalf("响应被篡改: %q", body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for countExchanges(t, tr) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("代理转发的请求没有被记录下来")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, err := tr.Search("127.0.0.1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].URL != target.URL+"/e2e" || rows[0].Status != 200 {
		t.Fatalf("索引行不符合预期: %+v", rows)
	}
}

// TestSearchFiltersAndPaging covers the paged metadata query: host filter,
// default page size, and the offset arithmetic.
func TestSearchFiltersAndPaging(t *testing.T) {
	tr, _ := openTraffic(t)
	for i := 0; i < 3; i++ {
		tr.record(newFlow("a.example", "GET", fmt.Sprintf("/p%d", i), nil, []byte("a")))
	}
	tr.record(newFlow("b.example", "POST", "/q", nil, []byte("b")))

	all, err := tr.Search("", 0, 0)
	if err != nil || len(all) != 4 {
		t.Fatalf("全量 size<=0 应回落到 100 条，得到 %d err=%v", len(all), err)
	}
	onlyA, err := tr.Search("a.example", 0, 10)
	if err != nil || len(onlyA) != 3 {
		t.Fatalf("host 过滤得到 %d err=%v", len(onlyA), err)
	}
	for _, r := range onlyA {
		if r.Host != "a.example" {
			t.Fatalf("host 过滤串味: %+v", r)
		}
	}
	page1, err := tr.Search("a.example", 1, 2)
	if err != nil || len(page1) != 1 {
		t.Fatalf("第二页应为 1 条，得到 %d err=%v", len(page1), err)
	}
	// 倒序分页：page0 的前两条不能与 page1 重叠。
	page0, err := tr.Search("a.example", 0, 2)
	if err != nil || len(page0) != 2 {
		t.Fatalf("第一页应为 2 条，得到 %d err=%v", len(page0), err)
	}
	seen := map[string]bool{}
	for _, r := range append(page0, page1...) {
		if seen[r.ID] {
			t.Fatalf("分页重复行: %s", r.ID)
		}
		seen[r.ID] = true
	}
}

func countExchanges(t *testing.T, tr *Traffic) int {
	t.Helper()
	var n int
	if err := tr.DB().QueryRow(`SELECT count(*) FROM exchanges`).Scan(&n); err != nil {
		t.Fatalf("统计 exchange: %v", err)
	}
	return n
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("代理 %s 在 10s 内没有开始监听", addr)
}
