package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/traffic"
)

// Coverage for manager.go's proxy/traffic plumbing and the degraded paths of
// the settings getters: the capture-on/off accessor matrix, global-proxy
// validation, web-search backends missing their key, and the JSON helpers the
// browser-MCP proxy sync depends on.

// TestManagerWithProxyStartsTraffic exercises NewManager's traffic branch, which
// a proxy-less test stack never reaches (traffic.Open is only called when
// proxyAddr is non-empty).
func TestManagerWithProxyStartsTraffic(t *testing.T) {
	// Port 0 → the proxy picks a free port, so parallel CI runs never collide.
	m, err := NewManager(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewManager(proxy): %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if m.Traffic() == nil {
		t.Fatal("traffic ledger not opened with a proxy address")
	}
	prevCapture := m.TrafficEnabled()
	t.Cleanup(func() { _ = m.SetTrafficEnabled(prevCapture) })

	// Capture OFF: agents go direct (or via the global proxy), no MITM CA, no
	// traffic tools injected.
	if err := m.SetTrafficEnabled(false); err != nil {
		t.Fatalf("disable capture: %v", err)
	}
	if got := m.ProxyCACert(); got != "" {
		t.Fatalf("CA with capture off=%q", got)
	}
	if got := m.HostTools(); got != nil {
		t.Fatalf("host tools with capture off=%d", len(got))
	}

	// Capture ON: the recording proxy supplies both the address and its CA, and
	// the traffic tools become host tools.
	if err := m.SetTrafficEnabled(true); err != nil {
		t.Fatalf("enable capture: %v", err)
	}
	if got := m.ProxyAddr(); got == "" {
		t.Fatal("ProxyAddr empty with capture on")
	}
	if got := m.ProxyCACert(); got == "" {
		t.Fatal("ProxyCACert empty with capture on")
	}
	if got := m.HostTools(); len(got) == 0 {
		t.Fatal("traffic host tools missing with capture on")
	}

	// SetGlobalProxy on a manager that owns a MITM also pushes it upstream.
	prevProxy := m.GlobalProxy()
	t.Cleanup(func() { _ = m.SetGlobalProxy(prevProxy) })
	if err := m.SetGlobalProxy("http://127.0.0.1:3128"); err != nil {
		t.Fatalf("set upstream proxy: %v", err)
	}

	// ... and with capture off the same value becomes the agent-facing address.
	_ = m.SetTrafficEnabled(false)
	if got := m.ProxyAddr(); got != "http://127.0.0.1:3128" {
		t.Fatalf("ProxyAddr with capture off=%q", got)
	}
}

func TestGlobalProxyValidation(t *testing.T) {
	m, _ := newTestStack(t)
	prev := m.GlobalProxy()
	t.Cleanup(func() { _ = m.SetGlobalProxy(prev) })

	// Only http/https/socks5 (optionally with credentials) are accepted.
	for _, bad := range []string{"ftp://proxy:21", "not-a-url", "://nope"} {
		if err := m.SetGlobalProxy(bad); err == nil {
			t.Fatalf("invalid proxy %q accepted", bad)
		}
	}
	// Surrounding whitespace is trimmed before persisting.
	if err := m.SetGlobalProxy("  socks5://user:pass@127.0.0.1:1080  "); err != nil {
		t.Fatalf("valid proxy rejected: %v", err)
	}
	if got := m.GlobalProxy(); got != "socks5://user:pass@127.0.0.1:1080" {
		t.Fatalf("stored proxy=%q", got)
	}
	// Empty means "direct" and is always valid.
	if err := m.SetGlobalProxy(""); err != nil {
		t.Fatalf("clear proxy: %v", err)
	}
	if got := m.GlobalProxy(); got != "" {
		t.Fatalf("cleared proxy=%q", got)
	}
}

// TestWebSearchBackendsWithoutKeys pins the rule that a keyed backend selected
// without its key degrades to disabled instead of silently dropping the tool.
func TestWebSearchBackendsWithoutKeys(t *testing.T) {
	m, _ := newTestStack(t)
	on, backend, brave, tavily, proxy := m.WebSearch()
	t.Cleanup(func() { _ = m.SetWebSearch(on, backend, &brave, &tavily, &proxy) })

	blank := ""
	// brave-free without a key → off.
	if err := m.SetWebSearch(true, "brave-free", &blank, nil, nil); err != nil {
		t.Fatalf("set brave-free: %v", err)
	}
	if o := m.WebSearchOpts(); o.Enabled {
		t.Fatalf("brave-free without key still enabled: %+v", o)
	}
	// ... and with a key → on.
	key := "sk-brave-probe"
	if err := m.SetWebSearch(true, "brave-free", &key, nil, nil); err != nil {
		t.Fatalf("set brave key: %v", err)
	}
	if o := m.WebSearchOpts(); !o.Enabled || o.BraveKey != key {
		t.Fatalf("brave-free with key: %+v", o)
	}

	// tavily without a key → off.
	if err := m.SetWebSearch(true, "tavily", nil, &blank, nil); err != nil {
		t.Fatalf("set tavily: %v", err)
	}
	if o := m.WebSearchOpts(); o.Enabled {
		t.Fatalf("tavily without key still enabled: %+v", o)
	}

	// An empty backend name falls back to the default backend.
	if err := m.SetWebSearch(true, "   ", nil, nil, nil); err != nil {
		t.Fatalf("set blank backend: %v", err)
	}
	if _, got, _, _, _ := m.WebSearch(); got != defaultWebSearchBackend {
		t.Fatalf("blank backend resolved to %q", got)
	}

	// A corrupt empty backend value in the struct also reports the default.
	m.mu.Lock()
	m.webSearchBackend = ""
	m.mu.Unlock()
	if _, got, _, _, _ := m.WebSearch(); got != defaultWebSearchBackend {
		t.Fatalf("empty struct backend resolved to %q", got)
	}

	// deepseek borrows credentials from the active LLM profile (none configured
	// here → empty strings, still a valid opts struct).
	if err := m.SetWebSearch(true, deepSeekWebSearchBackend, nil, nil, nil); err != nil {
		t.Fatalf("set deepseek: %v", err)
	}
	o := m.WebSearchOpts()
	if !o.Enabled || o.Backend != deepSeekWebSearchBackend {
		t.Fatalf("deepseek opts=%+v", o)
	}
	if base, apiKey, model := m.deepSeekSearchCreds(); base == "" && apiKey == "" && model != "" {
		t.Fatalf("inconsistent deepseek creds: %q/%q/%q", base, apiKey, model)
	}
}

// TestManagerGettersWithoutDB covers the nil-PG guards used during early boot.
func TestManagerGettersWithoutDB(t *testing.T) {
	bare := &Manager{}
	if bare.LLMPoolEnabled() {
		t.Fatal("LLMPoolEnabled true without a DB")
	}
	if bare.LLMPoolBindFallback() {
		t.Fatal("LLMPoolBindFallback true without a DB")
	}
	if got := bare.GlobalProxy(); got != "" {
		t.Fatalf("GlobalProxy without state=%q", got)
	}
	if got := bare.ProxyAddr(); got != "" {
		t.Fatalf("ProxyAddr without traffic/proxy=%q", got)
	}
	if got := bare.ProxyCACert(); got != "" {
		t.Fatalf("ProxyCACert without traffic=%q", got)
	}
	if got := bare.HostTools(); got != nil {
		t.Fatalf("HostTools without traffic=%d", len(got))
	}
}

// TestJSONHelpersForProxySync covers the codec helpers the browser-MCP sync uses.
func TestJSONHelpersForProxySync(t *testing.T) {
	// stripProxyArgs removes both flag forms and leaves other args intact.
	got := stripProxyArgs([]string{"--headless", "--proxy-server", "http://a:1", "--proxy-bypass=localhost", "--no-sandbox"})
	want := []string{"--headless", "--no-sandbox"}
	if len(got) != len(want) {
		t.Fatalf("stripProxyArgs=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stripProxyArgs=%v want %v", got, want)
		}
	}
	if got := stripProxyArgs(nil); len(got) != 0 {
		t.Fatalf("stripProxyArgs(nil)=%v", got)
	}

	// decodeStrSlice/decodeStrMap tolerate nil and junk payloads.
	if got := decodeStrSlice(nil); len(got) != 0 {
		t.Fatalf("decodeStrSlice(nil)=%v", got)
	}
	if got := decodeStrSlice(json.RawMessage(`not json`)); len(got) != 0 {
		t.Fatalf("decodeStrSlice(junk)=%v", got)
	}
	if got := decodeStrSlice(json.RawMessage(`["a","b"]`)); len(got) != 2 || got[1] != "b" {
		t.Fatalf("decodeStrSlice=%v", got)
	}
	if got := decodeStrMap(nil); got == nil || len(got) != 0 {
		t.Fatalf("decodeStrMap(nil)=%v", got)
	}
	if got := decodeStrMap(json.RawMessage(`{"K":"V"}`)); got["K"] != "V" {
		t.Fatalf("decodeStrMap=%v", got)
	}

	// encodeJSON round-trips and never returns a nil slice for valid input.
	raw := encodeJSON([]string{"--a"})
	var back []string
	if err := json.Unmarshal(raw, &back); err != nil || len(back) != 1 || back[0] != "--a" {
		t.Fatalf("encodeJSON=%q err=%v", raw, err)
	}
	// Unsupported type → nil payload rather than a panic.
	if raw := encodeJSON(make(chan int)); raw != nil {
		t.Fatalf("encodeJSON(chan)=%q", raw)
	}
}

// TestSyncBrowserMCPProxyTogglesCapture drives the browser MCP argument/env
// rewrite: capture on injects --proxy-server + NODE_EXTRA_CA_CERTS, capture off
// strips them again.
func TestSyncBrowserMCPProxyTogglesCapture(t *testing.T) {
	m, _ := newTestStack(t)
	if m.Traffic() == nil {
		var err error
		m.traffic, err = traffic.Open(t.TempDir(), "127.0.0.1:0")
		if err != nil {
			t.Skipf("traffic unavailable (%v)", err)
		}
		t.Cleanup(func() { _ = m.traffic.Close() })
	}
	prevCapture := m.TrafficEnabled()
	t.Cleanup(func() { _ = m.SetTrafficEnabled(prevCapture) })

	// Find the seeded browser MCP; without it the sync is a documented no-op.
	servers, err := m.pg.ListMCP()
	if err != nil {
		t.Fatalf("list mcp: %v", err)
	}
	var found bool
	var prevArgs, prevEnv json.RawMessage
	for _, srv := range servers {
		if srv.Name == browserMCPName {
			found = true
			prevArgs, prevEnv = srv.Args, srv.Env
			break
		}
	}
	if !found {
		m.syncBrowserMCPProxy() // no-op path
		return
	}
	t.Cleanup(func() {
		for _, srv := range servers {
			if srv.Name == browserMCPName {
				srv.Args, srv.Env = prevArgs, prevEnv
				_, _ = m.pg.SaveMCP(srv)
			}
		}
	})

	// Capture ON → args carry the proxy, env carries the CA path.
	if err := m.SetTrafficEnabled(true); err != nil {
		t.Fatalf("enable capture: %v", err)
	}
	args, env := browserMCPState(t, m)
	if !containsArg(args, "--proxy-server") {
		t.Fatalf("proxy arg missing: %v", args)
	}
	if env["NODE_EXTRA_CA_CERTS"] == "" {
		t.Fatalf("CA env missing: %v", env)
	}

	// Capture OFF → both stripped, other args untouched.
	if err := m.SetTrafficEnabled(false); err != nil {
		t.Fatalf("disable capture: %v", err)
	}
	args, env = browserMCPState(t, m)
	if containsArg(args, "--proxy-server") || strings.HasPrefix(strings.Join(args, " "), "--proxy-bypass") {
		t.Fatalf("proxy args survived capture-off: %v", args)
	}
	if _, ok := env["NODE_EXTRA_CA_CERTS"]; ok {
		t.Fatalf("CA env survived capture-off: %v", env)
	}
}

func browserMCPState(t *testing.T, m *Manager) ([]string, map[string]string) {
	t.Helper()
	servers, err := m.pg.ListMCP()
	if err != nil {
		t.Fatalf("list mcp: %v", err)
	}
	for _, srv := range servers {
		if srv.Name == browserMCPName {
			return decodeStrSlice(srv.Args), decodeStrMap(srv.Env)
		}
	}
	t.Fatalf("browser MCP disappeared")
	return nil, nil
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want || (len(a) > len(want) && a[:len(want)+1] == want+"=") {
			return true
		}
	}
	return false
}
