package enrich

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The engine's write side (doDNS/doHTTP) needs a live AssetStore, so this file pins
// everything that is observable without a database: the queue/cooldown plumbing, the
// proxy-aware HTTP client, and the parsing helpers (title/host). The DB-writing tails
// of doHTTP/doDNS stay untested by design — their statements are a single upsert call.

// newTestEngine builds an Engine without the dnsx resolver or the worker pool, so the
// queue/cooldown logic can be exercised deterministically.
func newTestEngine(buf int) *Engine {
	return &Engine{jobs: make(chan job, buf), closed: make(chan struct{})}
}

func cooldownKey(k jobKind, id int64) string {
	return string(rune(k)) + ":" + strconv.FormatInt(id, 10)
}

// waitFor polls cond for up to 2s (workers are asynchronous).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestEnqueueDropsInvalidJobs(t *testing.T) {
	var nilEngine *Engine
	nilEngine.ResolveDomain(1, "x") // must not panic
	nilEngine.ProbeSite(1, "http://x")

	e := newTestEngine(4)
	e.ResolveDomain(0, "example.com")  // id 0
	e.ResolveDomain(-3, "example.com") // negative id
	e.ResolveDomain(7, "")             // empty arg
	e.ProbeSite(8, "   ")              // "blank" is not blank to enqueue — see below
	if got := len(e.jobs); got != 1 {
		t.Fatalf("queued=%d, want only the whitespace arg job (arg check is == \"\")", got)
	}
	j := <-e.jobs
	if j.kind != jobHTTP || j.id != 8 {
		t.Fatalf("queued job=%+v", j)
	}
}

func TestEnqueueDropsWhenQueueFull(t *testing.T) {
	e := newTestEngine(1)
	e.ResolveDomain(1, "a.example")
	e.ResolveDomain(2, "b.example") // queue full → dropped, never blocks
	if got := len(e.jobs); got != 1 {
		t.Fatalf("queued=%d", got)
	}
	if j := <-e.jobs; j.id != 1 {
		t.Fatalf("first job replaced: %+v", j)
	}
}

func TestCooldownSuppressesRepeatAndExpires(t *testing.T) {
	e := newTestEngine(1)
	j := job{kind: jobDNS, id: 42, arg: "example.com"}

	if e.onCooldown(j) {
		t.Fatal("first run was skipped")
	}
	first, ok := e.cool.Load(cooldownKey(jobDNS, 42))
	if !ok {
		t.Fatal("cooldown entry not stored")
	}
	if !e.onCooldown(j) {
		t.Fatal("repeat inside the window was not suppressed")
	}
	// A different id / kind is a different key.
	if e.onCooldown(job{kind: jobDNS, id: 43}) || e.onCooldown(job{kind: jobHTTP, id: 42}) {
		t.Fatal("cooldown key collides across ids or kinds")
	}

	// Expired entries re-run and refresh the timestamp.
	e.cool.Store(cooldownKey(jobDNS, 42), time.Now().Add(-cooldown-time.Second))
	if e.onCooldown(j) {
		t.Fatal("expired entry still suppressed")
	}
	second, _ := e.cool.Load(cooldownKey(jobDNS, 42))
	if ts, ok := second.(time.Time); !ok || ts.Before(first.(time.Time)) {
		t.Fatalf("timestamp not refreshed: %v", second)
	}

	// A non-time value in the map is treated as "no cooldown", not a panic.
	e.cool.Store(cooldownKey(jobDNS, 99), "stale")
	if e.onCooldown(job{kind: jobDNS, id: 99}) {
		t.Fatal("non-time entry suppressed the job")
	}
}

func TestCloseIsIdempotentAndNilSafe(t *testing.T) {
	var nilEngine *Engine
	nilEngine.Close()

	e := newTestEngine(1)
	e.Close()
	e.Close() // sync.Once — second close must not panic
	select {
	case <-e.closed:
	default:
		t.Fatal("closed channel not closed")
	}
}

func TestWorkerStopsOnClose(t *testing.T) {
	e := newTestEngine(4)
	go e.worker()

	// resolv is nil → doDNS returns immediately, but the cooldown entry proves the job
	// was picked up (and not dropped by the queue).
	e.enqueue(job{kind: jobDNS, id: 1, arg: "example.com"})
	waitFor(t, "dns job pickup", func() bool {
		_, ok := e.cool.Load(cooldownKey(jobDNS, 1))
		return ok
	})

	// doHTTP with an unparsable URL bails out before touching the asset store.
	e.enqueue(job{kind: jobHTTP, id: 2, arg: "://missing-scheme"})
	waitFor(t, "http job pickup", func() bool {
		_, ok := e.cool.Load(cooldownKey(jobHTTP, 2))
		return ok
	})

	// An unknown kind falls through the switch without side effects.
	e.enqueue(job{kind: jobKind(9), id: 3, arg: "x"})
	waitFor(t, "unknown kind pickup", func() bool {
		_, ok := e.cool.Load(cooldownKey(jobKind(9), 3))
		return ok
	})

	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}
}

func TestNewDefaultsWorkersAndClient(t *testing.T) {
	// proxy is evaluated lazily, so the engine is usable with a nil-ish resolver.
	var mu sync.Mutex
	calls := 0
	proxy := func() string {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "http://127.0.0.1:8080"
	}
	e := New(nil, proxy, 0) // workers<=0 → 4 workers
	defer e.Close()
	if e.jobs == nil || e.client == nil {
		t.Fatalf("engine not initialised: %+v", e)
	}
	if cap(e.jobs) != queueSize {
		t.Fatalf("queue cap=%d", cap(e.jobs))
	}

	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	got, err := e.client.Transport.(*http.Transport).Proxy(req)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if got == nil || got.Host != "127.0.0.1:8080" {
		t.Fatalf("proxy url=%v", got)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("proxy() called %d times, want once per request", n)
	}
}

func TestBuildClientDirectAndProxyModes(t *testing.T) {
	// proxy == nil → no Proxy func at all (straight dial).
	direct := buildClient(nil)
	tr := direct.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("nil proxy func installed a proxy hook")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS verification must be skipped for pentest probes")
	}
	if direct.Timeout != httpTimeout || tr.TLSHandshakeTimeout != httpTimeout {
		t.Fatalf("timeouts: client=%v handshake=%v", direct.Timeout, tr.TLSHandshakeTimeout)
	}
	// Redirects are surfaced as-is (the caller re-checks scope at probe time).
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	if err := direct.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect=%v", err)
	}

	// proxy func returning "" means direct: nil URL, nil error.
	off := buildClient(func() string { return "" })
	url, err := off.Transport.(*http.Transport).Proxy(req)
	if url != nil || err != nil {
		t.Fatalf("empty proxy → %v, %v", url, err)
	}

	// A proxy address is parsed verbatim, including credentials.
	on := buildClient(func() string { return "socks5://user:pw@10.0.0.1:1080" })
	url, err = on.Transport.(*http.Transport).Proxy(req)
	if err != nil {
		t.Fatalf("proxy parse: %v", err)
	}
	if url.Scheme != "socks5" || url.Host != "10.0.0.1:1080" || url.User.Username() != "user" {
		t.Fatalf("proxy url=%v", url)
	}
}

func TestExtractTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<html><head><title>Hello</title></head>`, "Hello"},
		{"<title id=\"x\" class=\"y\">  spaced  </title>", "spaced"},
		{"<TITLE>Upper</TITLE>", "Upper"},
		{"<title>\n multi\n line\n</title>", "multi\n line"},
		{"<title>Tom &amp; Jerry &lt;3</title>", "Tom & Jerry <3"},
		{"<title></title>", ""},
		{"<title><title>inner</title></title>", "<title>inner"},
		{"<p>no title here</p>", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractTitle([]byte(c.in)); got != c.want {
			t.Fatalf("extractTitle(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestHostOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://a.example.com:8443/x", "a.example.com"},
		{"http://127.0.0.1/x", "127.0.0.1"},
		{"https://[::1]:443/x", "::1"},
		{"HTTP://Upper.Example.COM/", "Upper.Example.COM"}, // url.Parse preserves host case
		{"example.com", ""},                                // no scheme → treated as a path
		{"/relative", ""},
		{"://broken", ""},
		{"http://", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := hostOf(c.in); got != c.want {
			t.Fatalf("hostOf(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestUniqPreservesOrder(t *testing.T) {
	if got := uniq(nil); len(got) != 0 {
		t.Fatalf("nil → %v", got)
	}
	got := uniq([]string{"b", "a", "b", "", "a", "c"})
	if strings.Join(got, ",") != "b,a,,c" {
		t.Fatalf("uniq=%v", got)
	}
}

func TestSmallHelpers(t *testing.T) {
	if !id0(0) || !id0(-1) || id0(1) {
		t.Fatal("id0")
	}
	if itoa(-12) != "-12" || itoa(0) != "0" {
		t.Fatal("itoa")
	}
	// isBlank covers the scalar shapes templates render from.
	for _, v := range []any{nil, "", float64(0), 0} {
		if !isBlank(v) {
			t.Fatalf("isBlank(%#v)=false", v)
		}
	}
	for _, v := range []any{"x", 1.5, 2, true, []string{}} {
		if isBlank(v) {
			t.Fatalf("isBlank(%#v)=true", v)
		}
	}
}

// Guard against a silently broken URL parse in hostOf's contract above.
func TestHostOfMatchesStdlib(t *testing.T) {
	u, err := url.Parse("https://x.example/y")
	if err != nil {
		t.Fatalf("stdlib parse: %v", err)
	}
	if u.Hostname() != hostOf("https://x.example/y") {
		t.Fatal("hostOf diverged from url.Hostname")
	}
}
