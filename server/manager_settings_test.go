package server

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ---------- pure: stripProxyArgs ----------

func TestStripProxyArgs(t *testing.T) {
	in := []string{"--headless", "--proxy-server", "http://p:1", "--proxy-bypass=x", "--proxy-server=y", "--no-sandbox"}
	got := stripProxyArgs(in)
	want := []string{"--headless", "--no-sandbox"}
	if len(got) != len(want) {
		t.Fatalf("stripProxyArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stripProxyArgs = %q, want %q", got, want)
		}
	}
	// Input must not be mutated (the caller re-appends from current state).
	if in[0] != "--headless" || in[1] != "--proxy-server" || in[3] != "--proxy-bypass=x" || in[4] != "--proxy-server=y" {
		t.Fatalf("input mutated: %q", in)
	}
	if out := stripProxyArgs(nil); out == nil || len(out) != 0 {
		t.Fatalf("nil input = %v, want empty non-nil", out)
	}
}

// ---------- settings: web search / global proxy / browser MCP sync ----------

// snapshotSettings reads the persisted web-search/proxy settings so the test can
// restore them: the settings table lives in the shared postgres database, and
// NewManager seeds new managers from it.
func snapshotSettings(t *testing.T, m *Manager) map[string]string {
	t.Helper()
	keys := []string{settingWebSearchOn, settingWebSearchBackend, settingBraveKey,
		settingTavilyKey, settingWebSearchProxy, settingGlobalProxy, settingTrafficCapture}
	out := map[string]string{}
	for _, k := range keys {
		if v, ok, _ := m.pg.GetSetting(k); ok {
			out[k] = v
		}
	}
	return out
}

func TestSetWebSearch(t *testing.T) {
	m, _ := newTestStack(t)
	before := snapshotSettings(t, m)
	defer func() {
		for k, v := range before {
			_ = m.pg.SetSetting(k, v)
		}
	}()

	// Empty backend falls back to the default.
	if err := m.SetWebSearch(true, "", nil, nil, nil); err != nil {
		t.Fatalf("on: %v", err)
	}
	if !m.webSearchOn || m.webSearchBackend != defaultWebSearchBackend {
		t.Fatalf("after on: on=%v backend=%q", m.webSearchOn, m.webSearchBackend)
	}

	brave, tavily, proxy := "brave-key", "tavily-key", "http://127.0.0.1:8888"
	if err := m.SetWebSearch(true, "brave", &brave, &tavily, &proxy); err != nil {
		t.Fatalf("keys: %v", err)
	}
	if m.webSearchBackend != "brave" || m.braveKey != "brave-key" || m.tavilyKey != "tavily-key" || m.webSearchProxy != proxy {
		t.Fatalf("after keys: on=%v backend=%q brave=%q tavily=%q proxy=%q",
			m.webSearchOn, m.webSearchBackend, m.braveKey, m.tavilyKey, m.webSearchProxy)
	}
	if v, _, _ := m.pg.GetSetting(settingBraveKey); v != "brave-key" {
		t.Fatalf("brave key not persisted: %q", v)
	}

	// Toggling with nil pointers must not wipe saved keys (documented contract).
	if err := m.SetWebSearch(false, "", nil, nil, nil); err != nil {
		t.Fatalf("off: %v", err)
	}
	if m.braveKey != "brave-key" || m.tavilyKey != "tavily-key" || m.webSearchProxy != proxy {
		t.Fatalf("nil pointers wiped keys: brave=%q tavily=%q proxy=%q", m.braveKey, m.tavilyKey, m.webSearchProxy)
	}

	// Pointers to "" clear them explicitly.
	empty := ""
	if err := m.SetWebSearch(true, "", &empty, &empty, &empty); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if m.braveKey != "" || m.tavilyKey != "" || m.webSearchProxy != "" {
		t.Fatalf("explicit clear failed: brave=%q tavily=%q proxy=%q", m.braveKey, m.tavilyKey, m.webSearchProxy)
	}
}

func TestSetGlobalProxyAndBrowserMCP(t *testing.T) {
	m, _ := newTestStack(t)
	before := snapshotSettings(t, m)
	defer func() {
		for k, v := range before {
			_ = m.pg.SetSetting(k, v)
		}
	}()

	// Unsupported scheme is rejected before persisting anything.
	if err := m.SetGlobalProxy("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("ftp proxy accepted, want error")
	}
	if m.globalProxy != "" {
		t.Fatalf("rejected proxy leaked into state: %q", m.globalProxy)
	}

	// A valid proxy persists and reaches the in-memory state.
	if err := m.SetGlobalProxy("socks5://127.0.0.1:1080"); err != nil {
		t.Fatalf("valid proxy: %v", err)
	}
	if m.globalProxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("globalProxy = %q", m.globalProxy)
	}
	if v, _, _ := m.pg.GetSetting(settingGlobalProxy); v != "socks5://127.0.0.1:1080" {
		t.Fatalf("setting = %q", v)
	}
	// Direct connection is legal (empty string).
	if err := m.SetGlobalProxy(""); err != nil {
		t.Fatalf("direct: %v", err)
	}
	if m.globalProxy != "" {
		t.Fatalf("globalProxy after direct = %q", m.globalProxy)
	}

	// syncBrowserMCPProxy strips the recording proxy + CA when capture is off,
	// keeping unrelated args/env intact.
	servers, err := m.pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	var browser *db.MCPServer
	for _, s := range servers {
		if s.Name == browserMCPName {
			browser = s
		}
	}
	if browser == nil {
		t.Skip("seeded browser MCP missing from shared db")
	}
	origArgs, origEnv := browser.Args, browser.Env
	browser.Args = json.RawMessage(`["--headless","--proxy-server","http://old:1","--proxy-server=http://old:2","--no-sandbox"]`)
	browser.Env = json.RawMessage(`{"NODE_EXTRA_CA_CERTS":"/old/ca.pem","OTHER":"1"}`)
	if _, err := m.pg.SaveMCP(browser); err != nil {
		t.Fatalf("seed stale args: %v", err)
	}
	defer func() {
		browser.Args, browser.Env = origArgs, origEnv
		_, _ = m.pg.SaveMCP(browser)
	}()

	m.syncBrowserMCPProxy()
	fresh, err := m.pg.ListMCP()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range fresh {
		if s.Name != browserMCPName {
			continue
		}
		var args []string
		if err := json.Unmarshal(s.Args, &args); err != nil {
			t.Fatalf("args = %s", s.Args)
		}
		for _, a := range args {
			if a == "--proxy-server" || (len(a) >= 15 && a[:15] == "--proxy-server=") {
				t.Fatalf("stale proxy arg survived: %q", args)
			}
		}
		if len(args) != 2 || args[0] != "--headless" || args[1] != "--no-sandbox" {
			t.Fatalf("args = %q, want [--headless --no-sandbox]", args)
		}
		var env map[string]string
		if err := json.Unmarshal(s.Env, &env); err != nil {
			t.Fatalf("env = %s", s.Env)
		}
		if _, ok := env["NODE_EXTRA_CA_CERTS"]; ok {
			t.Fatalf("stale CA env survived: %v", env)
		}
		if env["OTHER"] != "1" {
			t.Fatalf("unrelated env dropped: %v", env)
		}
	}
}

// ---------- task categories: live-DTO refresh contracts ----------

func TestTaskCategoryLifecycle(t *testing.T) {
	m, _ := newTestStack(t)
	task, err := m.CreateTask("category lifecycle", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Category unique keys are lowercased names in the shared postgres db —
	// suffix with the run id so parallel/repeated runs cannot collide.
	catName := "Ops " + strconv.FormatInt(time.Now().UnixNano(), 10)
	cat, err := m.pg.CreateTaskCategory(catName)
	if err != nil {
		t.Fatal(err)
	}
	catID := cat.ID
	if _, err := m.SetTaskCategory(task.ID, &catID); err != nil {
		t.Fatalf("set: %v", err)
	}
	if st := task.lifecycleSnapshot(); st.CategoryID == nil || *st.CategoryID != catID || st.CategoryName != catName {
		t.Fatalf("snapshot after set = %+v", st)
	}

	// Rename refreshes every live DTO referencing the category.
	secName := "SecOps " + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := m.RenameTaskCategory(catID, secName); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if st := task.lifecycleSnapshot(); st.CategoryName != secName {
		t.Fatalf("snapshot after rename = %+v", st)
	}

	// Bulk move to uncategorized: the returned set marks what actually moved.
	updated, _, err := m.SetTasksCategory([]string{task.ID}, nil)
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if !updated[task.ID] {
		t.Fatalf("updated = %v", updated)
	}
	if st := task.lifecycleSnapshot(); st.CategoryID != nil || st.CategoryName != "" {
		t.Fatalf("snapshot after bulk clear = %+v", st)
	}

	// Bad ids surface the not-found error instead of silently passing.
	if _, err := m.SetTaskCategory("not-a-number", nil); err == nil {
		t.Fatal("bad task id accepted")
	}
	if _, _, err := m.SetTasksCategory([]string{"nope"}, nil); err == nil {
		t.Fatal("bad bulk id accepted")
	}

	// Delete cascades the reset onto live tasks.
	cat2, err := m.pg.CreateTaskCategory("Temp " + strconv.FormatInt(time.Now().UnixNano(), 10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetTaskCategory(task.ID, &cat2.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := m.DeleteTaskCategory(cat2.ID)
	if err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if st := task.lifecycleSnapshot(); st.CategoryID != nil || st.CategoryName != "" {
		t.Fatalf("snapshot after delete = %+v", st)
	}
}

// ---------- status transitions ----------

func TestSetTaskStatusGuarded(t *testing.T) {
	m, _ := newTestStack(t)
	task, err := m.CreateTask("guarded status", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// First terminal writer wins.
	won, err := m.SetTaskStatusGuarded(task.ID, "done")
	if err != nil || !won {
		t.Fatalf("first = won=%v err=%v", won, err)
	}
	st := task.lifecycleSnapshot()
	if st.Status != "done" || st.CompletedAt == 0 {
		t.Fatalf("snapshot after win = %+v", st)
	}

	// A second terminal status must lose the race.
	won, err = m.SetTaskStatusGuarded(task.ID, "failed")
	if err != nil {
		t.Fatalf("second err: %v", err)
	}
	if won {
		t.Fatal("second terminal write won, want lost")
	}
	if st := task.lifecycleSnapshot(); st.Status != "done" {
		t.Fatalf("status changed after lost race = %+v", st)
	}

	// Non-numeric id errors out.
	if _, err := m.SetTaskStatusGuarded("bad", "done"); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestSetTaskStatusAndDequeue(t *testing.T) {
	m, _ := newTestStack(t)
	task, err := m.CreateTask("plain status", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if st := task.lifecycleSnapshot(); st.Status != "done" {
		t.Fatalf("snapshot = %+v", st)
	}

	// Dequeue on a non-queued task is a harmless no-op; bad id errors.
	if err := m.DequeueTask(task.ID, true); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if err := m.DequeueTask("bad", true); err == nil {
		t.Fatal("bad dequeue id accepted")
	}
	if st := task.lifecycleSnapshot(); st.Queued || st.QueueMode != "" {
		t.Fatalf("dequeue mutated state = %+v", st)
	}
}

// ---------- ReplaceTaskLLMProfiles ----------

func TestReplaceTaskLLMProfilesTerminal(t *testing.T) {
	m, _ := newTestStack(t)
	task, err := m.CreateTask("llm chain terminal", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Make the task terminal first: terminal tasks must take the early return
	// (no blocked-intent reopen attempt) and keep their status.
	if _, err := m.SetTaskStatusGuarded(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	id, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// A chain referencing a non-existent profile may fail at the db layer; both a
	// db-level error and a clean no-op are acceptable — the contract under test is
	// that a terminal task never panics and never leaves the terminal state.
	_, _ = m.ReplaceTaskLLMProfiles(task.ID, []int64{id}, id)
	if st := task.lifecycleSnapshot(); st.Status != "done" {
		t.Fatalf("terminal status clobbered = %+v", st)
	}
}
