package db

import "testing"

// nkey.go 的自然键与 URL 归一化是全图去重的基石：写错一个键，同一资产就会
// 在图里裂成两份。这些测试把每个键函数的边界钉死。

func TestDomainKeyAndIPKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"EXAMPLE.com", "example.com"},
		{"  example.com  ", "example.com"},
		{"example.com.", "example.com"},
		{"EXAMPLE.COM.", "example.com"},
		{"", ""},
	}
	for _, c := range cases {
		if got := DomainKey(c.in); got != c.want {
			t.Fatalf("DomainKey(%q)=%q want %q", c.in, got, c.want)
		}
	}
	if got := IPKey(" 10.0.0.1 "); got != "10.0.0.1" {
		t.Fatalf("IPKey trims, got %q", got)
	}
}

func TestRootDomain(t *testing.T) {
	cases := []struct {
		host   string
		root   string
		isApex bool
	}{
		{"example.com", "example.com", true},          // apex 本身
		{"www.example.com", "example.com", false},     // 子域
		{"a.b.example.co.uk", "example.co.uk", false}, // 多级 public suffix
		{"example.co.uk", "example.co.uk", true},
		{"10.1.2.3", "10.1.2.3", true},           // IP 字面量 → 原样返回
		{"localhost", "localhost", true},         // 非 ICANN → 原样
		{"internal.corp", "internal.corp", true}, // 内部域后缀不可归类
		{"", "", true},
	}
	for _, c := range cases {
		root, apex := RootDomain(c.host)
		if root != c.root || apex != c.isApex {
			t.Fatalf("RootDomain(%q)=(%q,%v) want (%q,%v)", c.host, root, apex, c.root, c.isApex)
		}
	}
}

func TestCompositeKeys(t *testing.T) {
	if got := PortKey(7, "TCP", 8080); got != "7|tcp|8080" {
		t.Fatalf("PortKey=%q", got)
	}
	if got := ServiceKey(3, "HTTP"); got != "3|http" {
		t.Fatalf("ServiceKey=%q", got)
	}
	if got := SiteKey("HTTPS", "Example.com", 443); got != "https|example.com|443" {
		t.Fatalf("SiteKey=%q", got)
	}
	if got := EndpointKey(9, "post", "/api"); got != "9|POST|/api" {
		t.Fatalf("EndpointKey=%q", got)
	}
	if got := ParameterKey(5, "QUERY", "id"); got != "5|query|id" {
		t.Fatalf("ParameterKey=%q", got)
	}
	if got := TechKey("Nginx", "1.2"); got != "nginx|1.2" {
		t.Fatalf("TechKey=%q", got)
	}
	if NormalizeParamName("  UserId ") != "userid" {
		t.Fatalf("NormalizeParamName broken")
	}
}

func TestTemplatePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/user/123", "/user/{id}"},
		{"/user/123/orders/456", "/user/{id}/orders/{id}"},
		{"/f/3fa85f64-5717-4562-b3fc-2c963f66afa6", "/f/{uuid}"},
		{"/f/deadbeefdeadbeefdeadbeefdeadbeef", "/f/{hex}"},    // 32 hex
		{"/s/abcDEF123-_987xyzABC", "/s/abcDEF123-_987xyzABC"}, // 20 位，不足 24 保留
		{"/s/abcdEF12-ghijKL34-mnopQR56", "/s/{token}"},        // 24 位长 token
		{"/s/shortname", "/s/shortname"},                       // 不足 24 位保留
		{"/a/b/", "/a/b/"},
	}
	for _, c := range cases {
		if got := TemplatePath(c.in); got != c.want {
			t.Fatalf("TemplatePath(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSplitURL(t *testing.T) {
	scheme, host, port, tpl, params, err := SplitURL("https://Example.com:8443/api/users/42?page=2&tab=info", "GET")
	if err != nil {
		t.Fatal(err)
	}
	if scheme != "https" || host != "example.com" || port != 8443 || tpl != "/api/users/{id}" {
		t.Fatalf("SplitURL basic fields: %q %q %d %q", scheme, host, port, tpl)
	}
	if len(params) != 2 {
		t.Fatalf("params=%v", params)
	}

	scheme, host, port, tpl, _, err = SplitURL("http://example.com/x", "GET")
	if err != nil || scheme != "http" || host != "example.com" || port != 80 || tpl != "/x" {
		t.Fatalf("default http port: %v %q %q %d %q", err, scheme, host, port, tpl)
	}

	// 无 scheme → port 0；解析失败 → 报错
	if _, _, port, _, _, err := SplitURL("ftp://example.com", "GET"); err != nil || port != 0 {
		t.Fatalf("ftp scheme port=%d err=%v", port, err)
	}
	if _, _, _, _, _, err := SplitURL("://bad", "GET"); err == nil {
		t.Fatalf("unparseable URL must error")
	}
}
