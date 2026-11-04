package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// This file pins the LLM configuration control plane: profile CRUD + activation
// (single-default invariant, active-delete guard), the global retry policy, the
// failover pool status/reset endpoints, and the provider model-list probe
// (endpoint selection + key resolution). Profiles/settings are LIBRARY-WIDE, so
// every test snapshots the current active profile and retry policy and restores
// them on cleanup.

// snapshotLLMActivation records which profile is active (0 = none) and restores
// exactly that state when the test ends.
func snapshotLLMActivation(t *testing.T, m *Manager) {
	t.Helper()
	active, err := m.pg.ActiveProfile()
	if err != nil {
		t.Fatalf("read active profile: %v", err)
	}
	var prev int64
	if active != nil {
		prev = active.ID
	}
	t.Cleanup(func() {
		if prev > 0 {
			_ = m.pg.SetActiveProfile(prev)
			return
		}
		_, _ = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`)
	})
}

// snapshotRetryPolicy restores the library-wide retry policy on cleanup.
func snapshotRetryPolicy(t *testing.T, m *Manager) {
	t.Helper()
	prev := m.pg.LLMRetryPolicy()
	t.Cleanup(func() { _ = m.pg.SetLLMRetryPolicy(prev) })
}

func newTestProfile(t *testing.T, m *Manager, name string) *db.LLMProfile {
	t.Helper()
	id, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: name, Format: "openai", Model: "gpt-test", APIKey: "sk-test-" + name,
		Streaming: true, Priority: 7,
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(id) })
	p, err := m.pg.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("reload profile %d: %v", id, err)
	}
	return p
}

func TestLLMProfileListAndSaveRoutes(t *testing.T) {
	m, s := newTestStack(t)
	snapshotLLMActivation(t, m)

	// List: DTO shape (string ids) and — critically — no api_key leak.
	rec := crudDo(t, s, http.MethodGet, "/api/llm/profiles", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"api_key"`) {
		t.Fatal("profile list leaked an api_key field")
	}
	var listed struct {
		Profiles []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Format    string `json:"format"`
			Hint      string `json:"api_key_hint"`
			Streaming bool   `json:"streaming"`
		} `json:"profiles"`
	}
	crudJSON(t, rec, &listed)

	name := fmt.Sprintf("route-prof-%d", time.Now().UnixNano())
	key := "sk-route-" + name[len(name)-6:]

	// Create: streaming absent must DEFAULT to true (the *bool shadow contract);
	// negative max_tokens clamps to 0; max_tokens_field is cleared for non-openai.
	body := fmt.Sprintf(`{"name":"%s","format":"anthropic","model":"claude-test","api_key":"%s","max_tokens":-5,"max_tokens_field":"max_completion_tokens"}`, name, key)
	rec = crudDo(t, s, http.MethodPost, "/api/llm/profiles", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	crudJSON(t, rec, &created)
	if created.ID <= 0 {
		t.Fatalf("created id=%d", created.ID)
	}
	defer func() { _ = m.pg.DeleteProfile(created.ID) }()

	got, err := m.pg.ProfileByID(created.ID)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.Streaming {
		t.Fatal("absent streaming did not default to true")
	}
	if got.MaxTokens != 0 {
		t.Fatalf("negative max_tokens not clamped: %d", got.MaxTokens)
	}
	if got.MaxTokensField != "" {
		t.Fatalf("max_tokens_field must be cleared for anthropic, got %q", got.MaxTokensField)
	}
	if !strings.HasPrefix(got.APIKey, "sk-route-") {
		t.Fatalf("stored key not persisted: %q", got.APIKey)
	}

	// Hint + leak contract come from the LIST projection (ProfileByID reads the
	// raw key, the list reads api_key_hint): the last 4 chars only.
	rec = crudDo(t, s, http.MethodGet, "/api/llm/profiles", "")
	if strings.Contains(rec.Body.String(), key) {
		t.Fatal("profile list leaked the raw API key")
	}
	crudJSON(t, rec, &listed)
	hint := ""
	for _, p := range listed.Profiles {
		if p.ID == i64s(created.ID) {
			hint = p.Hint
		}
	}
	if hint != "…"+key[len(key)-4:] {
		t.Fatalf("hint=%q want suffix of key", hint)
	}

	// Explicit streaming:false persists (the case the plain bool got wrong).
	rec = crudDo(t, s, http.MethodPost, "/api/llm/profiles",
		fmt.Sprintf(`{"id":%d,"name":"%s","format":"anthropic","model":"claude-test","streaming":false}`, created.ID, name))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ = m.pg.ProfileByID(created.ID)
	if got.Streaming {
		t.Fatal("explicit streaming:false ignored")
	}
	// Empty api_key on update keeps the stored key.
	if !strings.HasPrefix(got.APIKey, "sk-route-") {
		t.Fatalf("stored key changed on empty update: %q", got.APIKey)
	}

	// Unknown format surfaces the DB CHECK as a 500 (route does not pre-validate).
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/profiles", `{"name":"bad-format","format":"rpc","model":"m"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad format: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Malformed JSON is a 400 before touching the DB.
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/profiles", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
}

func TestLLMProfileActivateAndDeleteRoutes(t *testing.T) {
	m, s := newTestStack(t)
	snapshotLLMActivation(t, m)

	first := newTestProfile(t, m, fmt.Sprintf("act-one-%d", time.Now().UnixNano()))
	second := newTestProfile(t, m, fmt.Sprintf("act-two-%d", time.Now().UnixNano()))

	// Activate invalidates the previous default: exactly one is_default remains.
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/profiles/active",
		fmt.Sprintf(`{"id":%d}`, first.ID)); rec.Code != http.StatusOK {
		t.Fatalf("activate: status=%d body=%s", rec.Code, rec.Body.String())
	}
	all, err := m.pg.ListProfiles()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range all {
		if p.IsDefault != (p.ID == first.ID) {
			t.Fatalf("default invariant broken: profile %d is_default=%v", p.ID, p.IsDefault)
		}
	}

	// Activating an unknown id is a 500 (SetActiveProfile → not found).
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/profiles/active", `{"id":987654321}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("activate unknown: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/profiles/active", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("activate bad json: status=%d", rec.Code)
	}

	// The ACTIVE profile cannot be deleted — 409 with the actionable message.
	rec := crudDo(t, s, http.MethodDelete, fmt.Sprintf("/api/llm/profiles/%d", first.ID), "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "不能删除") {
		t.Fatalf("delete active: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// A non-active profile deletes cleanly; unknown id is 404.
	rec = crudDo(t, s, http.MethodDelete, fmt.Sprintf("/api/llm/profiles/%d", second.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete inactive: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The delete envelope carries the raw numeric id (not the DTO string form).
	var del struct {
		Deleted int64 `json:"deleted"`
	}
	crudJSON(t, rec, &del)
	if del.Deleted != second.ID {
		t.Fatalf("deleted=%d want %d", del.Deleted, second.ID)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/llm/profiles/987654321", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/llm/profiles/abc", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLLMRetryPolicyRoutes(t *testing.T) {
	m, s := newTestStack(t)
	snapshotRetryPolicy(t, m)

	// Defaults: five rules, always present (zero = "use default" in the UI).
	rec := crudDo(t, s, http.MethodGet, "/api/llm/retry-policy", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get policy: status=%d", rec.Code)
	}
	var pol struct {
		Connect db.RetryRule `json:"connect"`
		Empty   db.RetryRule `json:"empty"`
		Stream  db.RetryRule `json:"stream"`
		Breaker db.RetryRule `json:"breaker"`
		Intent  db.RetryRule `json:"intent"`
	}
	crudJSON(t, rec, &pol)

	// Save: response echoes the persisted (clamped) policy.
	body := `{"connect":{"attempts":5,"interval_ms":120},"empty":{"attempts":1,"interval_ms":0},` +
		`"stream":{"attempts":2,"interval_ms":500},"breaker":{"attempts":-1,"interval_ms":0},"intent":{"attempts":3,"interval_ms":3000}}`
	rec = crudDo(t, s, http.MethodPost, "/api/llm/retry-policy", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("save policy: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var saved db.LLMRetryPolicy
	crudJSON(t, rec, &saved)
	if saved.Connect.Attempts != 5 || saved.Connect.IntervalMS != 120 || saved.Breaker.Attempts != -1 {
		t.Fatalf("saved=%+v", saved)
	}
	persisted := m.pg.LLMRetryPolicy()
	if persisted.Connect.Attempts != 5 || persisted.Intent.IntervalMS != 3000 {
		t.Fatalf("persisted=%+v", persisted)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/retry-policy", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
}

func TestLLMPoolStatusAndResetRoutes(t *testing.T) {
	m, s := newTestStack(t)
	snapshotLLMActivation(t, m)
	prof := newTestProfile(t, m, fmt.Sprintf("pool-%d", time.Now().UnixNano()))

	type member struct {
		ProfileID string `json:"profile_id"`
		Name      string `json:"name"`
		Active    bool   `json:"active"`
		State     string `json:"state"`
		Fails     int    `json:"fails"`
		LastError string `json:"last_error"`
	}
	var status struct {
		Enabled      bool     `json:"enabled"`
		BindFallback bool     `json:"bind_fallback"`
		Chain        []member `json:"chain"`
	}

	// Fresh profile appears in the chain as healthy.
	rec := crudDo(t, s, http.MethodGet, "/api/llm/pool", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pool status: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &status)
	found := false
	for _, mb := range status.Chain {
		if mb.ProfileID == i64s(prof.ID) {
			found = true
			if mb.State != "ok" || mb.Fails != 0 {
				t.Fatalf("fresh member=%+v", mb)
			}
		}
	}
	if !found {
		t.Fatalf("profile %d missing from chain", prof.ID)
	}

	// A hard trip flips the member to tripped; reset by id restores it.
	s.llmHealth.Trip(prof.ID, "probe breaker", true)
	crudJSON(t, crudDo(t, s, http.MethodGet, "/api/llm/pool", ""), &status)
	state := ""
	for _, mb := range status.Chain {
		if mb.ProfileID == i64s(prof.ID) {
			state = mb.State
			if mb.LastError != "probe breaker" {
				t.Fatalf("last_error=%q", mb.LastError)
			}
		}
	}
	if state != "tripped" {
		t.Fatalf("after trip state=%q", state)
	}
	rec = crudDo(t, s, http.MethodPost, "/api/llm/pool/reset", fmt.Sprintf(`{"id":%d}`, prof.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset one: status=%d", rec.Code)
	}
	crudJSON(t, rec, &status)
	for _, mb := range status.Chain {
		if mb.ProfileID == i64s(prof.ID) && mb.State != "ok" {
			t.Fatalf("reset did not clear breaker: %+v", mb)
		}
	}

	// id=0 clears every breaker (the "全部恢复" button).
	s.llmHealth.Trip(prof.ID, "probe again", true)
	rec = crudDo(t, s, http.MethodPost, "/api/llm/pool/reset", `{"id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset all: status=%d", rec.Code)
	}
	crudJSON(t, rec, &status)
	for _, mb := range status.Chain {
		if mb.ProfileID == i64s(prof.ID) && mb.State == "tripped" {
			t.Fatalf("reset-all left a tripped member: %+v", mb)
		}
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/pool/reset", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("reset bad json: status=%d", rec.Code)
	}
}

func TestLLMListModelsRoute(t *testing.T) {
	m, s := newTestStack(t)

	// No key anywhere → ok:false with the actionable message (not an error code).
	rec := crudDo(t, s, http.MethodPost, "/api/llm/models", `{"provider":"openai","base_url":"http://127.0.0.1:1"}`)
	var resp struct {
		OK     bool     `json:"ok"`
		Error  string   `json:"error"`
		Models []string `json:"models"`
	}
	crudJSON(t, rec, &resp)
	if resp.OK || !strings.Contains(resp.Error, "API Key") {
		t.Fatalf("missing key resp=%+v", resp)
	}

	var gotAuth, gotKeyHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKeyHdr = r.Header.Get("x-api-key")
		switch r.URL.Path {
		case "/models", "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"},{"id":""}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// OpenAI format: Bearer auth on <base>/models.
	rec = crudDo(t, s, http.MethodPost, "/api/llm/models",
		fmt.Sprintf(`{"provider":"openai","base_url":"%s","api_key":"sk-openai"}`, srv.URL))
	crudJSON(t, rec, &resp)
	if !resp.OK || len(resp.Models) != 2 || resp.Models[0] != "model-a" {
		t.Fatalf("openai models resp=%+v", resp)
	}
	if gotAuth != "Bearer sk-openai" {
		t.Fatalf("openai auth header=%q", gotAuth)
	}

	// Anthropic format: x-api-key on <base>/v1/models.
	rec = crudDo(t, s, http.MethodPost, "/api/llm/models",
		fmt.Sprintf(`{"provider":"anthropic","base_url":"%s/v1/messages","api_key":"sk-ant"}`, srv.URL))
	crudJSON(t, rec, &resp)
	if !resp.OK || len(resp.Models) != 2 {
		t.Fatalf("anthropic models resp=%+v", resp)
	}
	if gotKeyHdr != "sk-ant" {
		t.Fatalf("anthropic x-api-key=%q", gotKeyHdr)
	}

	// Anthropic-compatible third party under /anthropic: the /models fallback at
	// the stripped root is what actually answers.
	rec = crudDo(t, s, http.MethodPost, "/api/llm/models",
		fmt.Sprintf(`{"provider":"anthropic","base_url":"%s/anthropic","api_key":"sk-third"}`, srv.URL))
	crudJSON(t, rec, &resp)
	if !resp.OK || len(resp.Models) != 2 {
		t.Fatalf("third-party fallback resp=%+v", resp)
	}
	if gotAuth != "Bearer sk-third" {
		t.Fatalf("fallback should use bearer auth, got %q", gotAuth)
	}

	// Stored-key fallback: no api_key in the body, profile_id supplies it.
	prof := newTestProfile(t, m, fmt.Sprintf("models-%d", time.Now().UnixNano()))
	rec = crudDo(t, s, http.MethodPost, "/api/llm/models",
		fmt.Sprintf(`{"provider":"openai","base_url":"%s","profile_id":%d}`, srv.URL, prof.ID))
	crudJSON(t, rec, &resp)
	if !resp.OK || gotAuth != "Bearer "+prof.APIKey {
		t.Fatalf("profile-key fallback resp=%+v auth=%q", resp, gotAuth)
	}

	// 200 with no usable ids is ok:true + empty list (not an error).
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer empty.Close()
	rec = crudDo(t, s, http.MethodPost, "/api/llm/models",
		fmt.Sprintf(`{"provider":"openai","base_url":"%s","api_key":"sk-e"}`, empty.URL))
	crudJSON(t, rec, &resp)
	if !resp.OK || resp.Models == nil || len(resp.Models) != 0 {
		t.Fatalf("empty models resp=%+v body=%s", resp, rec.Body.String())
	}

	// Non-200 upstream → ok:false with the status echoed (still HTTP 200).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer bad.Close()
	rec = crudDo(t, s, http.MethodPost, "/api/llm/models",
		fmt.Sprintf(`{"provider":"openai","base_url":"%s","api_key":"sk-bad"}`, bad.URL))
	crudJSON(t, rec, &resp)
	if resp.OK || !strings.Contains(resp.Error, "401") {
		t.Fatalf("upstream 401 resp=%+v", resp)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/llm/models", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}
}
