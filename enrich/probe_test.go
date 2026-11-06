package enrich

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
)

// 本文件补齐 enrich_test.go 有意留下的写回面：doDNS / doHTTP 需要活的
// AssetStore（PG），加上一个本地假 DNS 服务，两条解析链路都能确定性验证。

func openAssetDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// fakeDNS 起一个只回 UDP 的本地 DNS 服务，answer 决定每种记录怎么答。
func fakeDNS(t *testing.T, answer func(q dns.Question) []dns.RR) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			m.Answer = append(m.Answer, answer(q)...)
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// TestDoDNSWritesAssetGraph: doDNS 必须把 A/AAAA 解析出的 IP upsert 成 ip 资产
// 并回填绑定域名，同时把三种记录写成 subdomain 资产。
func TestDoDNSWritesAssetGraph(t *testing.T) {
	d := openAssetDB(t)
	store := d.Assets()
	uniq := time.Now().UnixNano()
	host := fmt.Sprintf("enrich-dns-%d.test", uniq)
	ip4 := fmt.Sprintf("203.0.113.%d", 40+uniq%200)
	ip6 := fmt.Sprintf("fd00::%x", uniq%0xffff)

	addr := fakeDNS(t, func(q dns.Question) []dns.RR {
		hdr := dns.RR_Header{Name: dns.Fqdn(host), Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: 60}
		switch q.Qtype {
		case dns.TypeA:
			return []dns.RR{&dns.A{Hdr: hdr, A: net.ParseIP(ip4)}}
		case dns.TypeAAAA:
			return []dns.RR{&dns.AAAA{Hdr: hdr, AAAA: net.ParseIP(ip6)}}
		case dns.TypeCNAME:
			return []dns.RR{&dns.CNAME{Hdr: hdr, Target: dns.Fqdn("target." + host)}}
		}
		return nil
	})
	resolv, err := dnsx.New(dnsx.Options{
		BaseResolvers: []string{addr},
		MaxRetries:    2,
		QuestionTypes: []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME},
		Timeout:       3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE root_domain = $1`, host)
		d.Exec(`DELETE FROM assets WHERE type='ip' AND ip = $1`, ip4)
		d.Exec(`DELETE FROM assets WHERE type='ip' AND ip = $1`, ip6)
	})

	e := &Engine{as: store, resolv: resolv}
	e.doDNS(1, host)

	// ip 资产：两个解析结果都在，且绑定域名指向 host。
	assets, err := store.QueryByType("ip", 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	found4, found6 := false, false
	for _, a := range assets {
		if a.IP == ip4 {
			found4 = true
			if !containsString(a.BoundDomains, host) {
				t.Fatalf("ip %s 未绑定域名 %s: %v", ip4, host, a.BoundDomains)
			}
		}
		if a.IP == ip6 {
			found6 = true
		}
	}
	if !found4 || !found6 {
		t.Fatalf("解析出的 ip 资产缺失: v4=%v v6=%v", found4, found6)
	}

	// subdomain 资产：A / AAAA / CNAME 各一条。
	subs, err := store.QueryByType("subdomain", 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]string{"A": ip4, "AAAA": ip6, "CNAME": "target." + host}
	for _, a := range subs {
		if a.Domain != host {
			continue
		}
		want, ok := wanted[a.RecordType]
		if !ok {
			continue
		}
		if !containsString(a.RecordValue, want) {
			t.Fatalf("%s 记录值不符: %v, want 含 %q", a.RecordType, a.RecordValue, want)
		}
		delete(wanted, a.RecordType)
	}
	if len(wanted) != 0 {
		t.Fatalf("subdomain 资产缺失: %v", wanted)
	}
}

// TestDoDNSWithoutResolverAndOnFailure 覆盖两条早退路径：解析器没初始化、
// 解析服务不可达（都应静默返回，不产生任何资产）。
func TestDoDNSWithoutResolverAndOnFailure(t *testing.T) {
	d := openAssetDB(t)
	store := d.Assets()

	before, err := store.CountByType("ip")
	if err != nil {
		t.Fatal(err)
	}

	// 解析器为 nil（dnsx 初始化失败时的降级状态）。
	e := &Engine{as: store}
	e.doDNS(1, "no-resolver.test")

	// 解析器指向没人监听的端口：重试耗尽后 QueryMultiple 返回错误。
	dead, err := dnsx.New(dnsx.Options{
		BaseResolvers: []string{"127.0.0.1:1"},
		MaxRetries:    1,
		QuestionTypes: []uint16{dns.TypeA},
		Timeout:       500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	e2 := &Engine{as: store, resolv: dead}
	e2.doDNS(2, "dead-resolver.test")

	after, err := store.CountByType("ip")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("失败路径不应产生资产: before=%d after=%d", before, after)
	}
}

// TestDoHTTPWritesServiceAsset: 探测结果（状态码/长度/标题）必须写进 http
// service 资产，且重复探测走 upsert 合并而不是新增行。
func TestDoHTTPWritesServiceAsset(t *testing.T) {
	d := openAssetDB(t)
	store := d.Assets()
	page := "<html><head><title>Enrich Probe &amp; Co</title></head><body>ok</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Header.Get("User-Agent") != "artex-enrich/1.0" {
			http.Error(w, "bad agent", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	e := &Engine{as: store, client: buildClient(nil)}
	rawURL := srv.URL + "/probe"
	t.Cleanup(func() { d.Exec(`DELETE FROM assets WHERE url = $1`, rawURL) })

	e.doHTTP(1, rawURL)

	row := d.QueryRow(`SELECT status_code, content_length, page_title FROM assets
WHERE type='service' AND service_type='http' AND url = $1`, rawURL)
	var status, length *int
	var title *string
	if err := row.Scan(&status, &length, &title); err != nil {
		t.Fatalf("探测结果未落库: %v", err)
	}
	if status == nil || *status != 200 {
		t.Fatalf("status_code = %v, want 200", status)
	}
	if length == nil || *length != len(page) {
		t.Fatalf("content_length = %v, want %d", length, len(page))
	}
	// 标题要解 HTML 实体并去首尾空白。
	if title == nil || !strings.Contains(*title, "Enrich Probe & Co") {
		t.Fatalf("page_title = %v", title)
	}

	// 第二次探测（标题变化）应合并到同一行。
	page2 := strings.Replace(page, "Enrich Probe &amp; Co", "Second Visit", 1)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page2)
	})
	e.doHTTP(1, rawURL)
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM assets WHERE url = $1`, rawURL).Scan(&n); err != nil || n != 1 {
		t.Fatalf("重复探测应合并为一行: n=%d err=%v", n, err)
	}
	var title2 *string
	if err := d.QueryRow(`SELECT page_title FROM assets WHERE url = $1`, rawURL).Scan(&title2); err != nil {
		t.Fatal(err)
	}
	if title2 == nil || !strings.Contains(*title2, "Second Visit") {
		t.Fatalf("二次探测标题未更新: %v", title2)
	}
}

// TestDoHTTPIgnoresFailures: 目标不可达时不产生资产、不 panic。
func TestDoHTTPIgnoresFailures(t *testing.T) {
	d := openAssetDB(t)
	store := d.Assets()

	// 已关闭的端口（连接拒绝）。
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := closed.Listener.Addr().String()
	closed.Close()

	before, err := store.CountByType("service")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{as: store, client: buildClient(nil)}
	e.doHTTP(1, "http://"+addr+"/dead")
	e.doHTTP(1, "no-scheme.example/x") // hostOf 解析不出主机 → 早退

	after, err := store.CountByType("service")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("失败探测不应产生资产: before=%d after=%d", before, after)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
