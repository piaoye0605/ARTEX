package server

import (
	"net/http"
	"strings"
	"testing"
	"text/template"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// This file pins the prompt-editing control plane (server_prompts.go): the
// template helpers behind validation/preview, and the handler round-trips for
// prompt, wrap-up and task-timeout wrap-up overrides.
//
// Shared-DB discipline: the round-trip runs on a throw-away custom agent. The
// only built-in touched is prompt/reset, which rewrites the code default (i.e.
// restores rather than customises).

func TestPromptTemplateHelpers(t *testing.T) {
	// withGlobalVars: a stored catalog entry shadowed by a global runtime var is
	// dropped so the UI never sees duplicate names.
	catalog := withGlobalVars([]db.PromptVar{
		{Name: "Now", Description: "shadowed", Example: "stale"},
		{Name: "TaskGoal", Description: "goal", Example: "own admin"},
	})
	if len(catalog) != 3 {
		t.Fatalf("global var merge = %+v", catalog)
	}
	names := map[string]string{}
	for _, v := range catalog {
		names[v.Name] = v.Description
	}
	if names["Now"] == "shadowed" || names["DataDir"] == "" || names["TaskGoal"] != "goal" {
		t.Fatalf("global var shadowing wrong: %+v", catalog)
	}

	// validateTemplate: allowed var, unknown var, syntax error.
	if bad := validateTemplate("hello {{.Now}} / {{.TaskGoal}}", catalog); bad != "" {
		t.Fatalf("valid template rejected: %s", bad)
	}
	if bad := validateTemplate("hello {{.Bogus}}", catalog); !strings.Contains(bad, "不在该 agent 允许列表") {
		t.Fatalf("unknown var verdict = %q", bad)
	}
	if bad := validateTemplate("{{.Now", catalog); !strings.Contains(bad, "模板语法错误") {
		t.Fatalf("syntax error verdict = %q", bad)
	}

	// renderPrompt: catalog example, then sample override, then the two error paths.
	if got, err := renderPrompt("hi {{.TaskGoal}}", catalog, nil); err != nil || got != "hi own admin" {
		t.Fatalf("render example = %q (%v)", got, err)
	}
	if got, err := renderPrompt("hi {{.TaskGoal}}", catalog, map[string]string{"TaskGoal": "sampled"}); err != nil || got != "hi sampled" {
		t.Fatalf("render sample = %q (%v)", got, err)
	}
	if _, err := renderPrompt("{{.TaskGoal", catalog, nil); err == nil {
		t.Fatal("broken template must not render")
	}
	if _, err := renderPrompt("{{.Missing}}", catalog, nil); err == nil {
		t.Fatal("missing key must fail (missingkey=error)")
	}

	// templateFields walks if/range/with bodies and de-duplicates names.
	tmpl := template.Must(template.New("p").Parse(
		`{{if .Now}}{{.Now}}{{else}}{{.DataDir}}{{end}}{{range .Now}}{{.Now}}{{end}}{{with .TaskGoal}}{{.TaskGoal}}{{end}}{{.Now}}`))
	fields := templateFields(tmpl)
	if strings.Join(fields, ",") != "Now,DataDir,TaskGoal" {
		t.Fatalf("template fields = %v", fields)
	}
	empty := template.Must(template.New("e").Parse(""))
	if got := templateFields(empty); len(got) != 0 {
		t.Fatalf("empty template fields = %v", got)
	}
}

func TestPromptHandlersRoundTrip(t *testing.T) {
	m, s := newTestStack(t)
	key := agentProbeKey("prompt")
	t.Cleanup(func() {
		_ = m.pg.DeleteAgent(key)
		_, _ = m.pg.Exec(`DELETE FROM triggers WHERE agent_key=$1`, key)
	})
	if rec := crudDo(t, s, http.MethodPost, "/api/agents", `{"key":"`+key+`","name":"prompt probe"}`); rec.Code != http.StatusOK {
		t.Fatalf("create agent: status=%d body=%s", rec.Code, rec.Body.String())
	}
	base := "/api/agents/" + key

	// --- prompt save: malformed body, unknown variable, success ---
	if rec := crudDo(t, s, http.MethodPut, base+"/prompt", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("prompt bad json: status=%d", rec.Code)
	}
	rec := crudDo(t, s, http.MethodPut, base+"/prompt", `{"template":"hello {{.Bogus}}"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不在该 agent 允许列表") {
		t.Fatalf("prompt unknown var: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodPut, base+"/prompt", `{"template":"hello {{.Now}}","note":"probe edit"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("prompt save: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var saved struct {
		Version int64 `json:"version"`
	}
	crudJSON(t, rec, &saved)
	if saved.Version <= 0 {
		t.Fatalf("prompt save version = %+v", saved)
	}

	// --- versions + variables ---
	rec = crudDo(t, s, http.MethodGet, base+"/prompts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("versions: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var versions struct {
		Versions []map[string]any `json:"versions"`
	}
	crudJSON(t, rec, &versions)
	if len(versions.Versions) == 0 {
		t.Fatalf("no prompt versions recorded: %s", rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodGet, base+"/variables", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Now") || !strings.Contains(rec.Body.String(), "DataDir") {
		t.Fatalf("variables: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- preview: sample override, syntax error, and the empty-template fallback ---
	rec = crudDo(t, s, http.MethodPost, base+"/prompt/preview", `{"template":"at {{.Now}}","sample":{"Now":"T0"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var preview struct {
		Rendered string `json:"rendered"`
		Error    string `json:"error"`
	}
	crudJSON(t, rec, &preview)
	if preview.Rendered != "at T0" || preview.Error != "" {
		t.Fatalf("preview sample = %+v", preview)
	}
	rec = crudDo(t, s, http.MethodPost, base+"/prompt/preview", `{"template":"{{.Now"}`)
	preview = struct {
		Rendered string `json:"rendered"`
		Error    string `json:"error"`
	}{}
	crudJSON(t, rec, &preview)
	if rec.Code != http.StatusOK || preview.Rendered != "" || preview.Error == "" {
		t.Fatalf("preview syntax error = status=%d %+v", rec.Code, preview)
	}
	// 空模板 → 回落到已保存的当前提示词（结果内容不重要，重要的是不 500）
	if rec := crudDo(t, s, http.MethodPost, base+"/prompt/preview", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("preview fallback: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- wrapup: bad json, prompt+turns, negative clamp, omitted turns, reset ---
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrapup bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup", `{"prompt":"wrap up","max_turns":3}`); rec.Code != http.StatusOK {
		t.Fatalf("wrapup save: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup", `{"prompt":"wrap up v2","max_turns":-2}`); rec.Code != http.StatusOK {
		t.Fatalf("wrapup negative turns: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup", `{"prompt":"wrap up v3"}`); rec.Code != http.StatusOK {
		t.Fatalf("wrapup without turns: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodPost, base+"/wrapup/reset", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"wrapup_default"`) {
		t.Fatalf("wrapup reset: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- task-timeout wrapup: bad json, keep turns, negative clamp, reset ---
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup/task-timeout", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("timeout wrapup bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup/task-timeout", `{"prompt":"timeout text"}`); rec.Code != http.StatusOK {
		t.Fatalf("timeout wrapup keep turns: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, base+"/wrapup/task-timeout", `{"prompt":"timeout text 2","max_turns":-1}`); rec.Code != http.StatusOK {
		t.Fatalf("timeout wrapup clamp: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodPost, base+"/wrapup/task-timeout/reset", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"task_timeout_wrapup_default"`) {
		t.Fatalf("timeout wrapup reset: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- prompt reset: custom agents have no code default; built-ins do ---
	rec = crudDo(t, s, http.MethodPost, base+"/prompt/reset", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "无内置默认提示词") {
		t.Fatalf("custom agent reset: status=%d body=%s", rec.Code, rec.Body.String())
	}
	builtin := "pentest"
	if _, has := agent.BuiltinPromptSeeds()[builtin]; !has {
		t.Fatalf("seed agent %q missing from built-in catalog", builtin)
	}
	rec = crudDo(t, s, http.MethodPost, "/api/agents/"+builtin+"/prompt/reset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("builtin prompt reset: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reset struct {
		Version int64 `json:"version"`
	}
	crudJSON(t, rec, &reset)
	if reset.Version <= 0 {
		t.Fatalf("reset version = %+v", reset)
	}

	// --- unknown agent key is a 404 for every prompt route ---
	missing := agentProbeKey("missing")
	rec = crudDo(t, s, http.MethodGet, "/api/agents/"+missing+"/prompts", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/agents/"+missing+"/prompt", `{"template":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent prompt: status=%d", rec.Code)
	}
}
