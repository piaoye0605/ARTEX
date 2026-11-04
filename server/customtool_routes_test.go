package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// This file pins the custom-tool ROUTE contracts that previously had no coverage
// (the pure exec helpers and the http/python executors are pinned in
// customtool_test.go): the CRUD routes around the tools table (system rows are
// immutable through this API), the editor's dry-run endpoint (which executes
// real command/http specs), the tools-table → CoreTool bridge, and the python
// interpreter resolution chain.

func cleanupCustomTool(t *testing.T, m *Manager, keys ...string) {
	t.Helper()
	for _, key := range keys {
		_ = m.pg.DeleteCustomTool(key)
	}
}

func TestCustomToolCRUDRoutes(t *testing.T) {
	m, s := newTestStack(t)
	key := fmt.Sprintf("probe_tool_%d", time.Now().UnixNano())
	defer cleanupCustomTool(t, m, key, key+"_http", key+"_shell")

	// Validation happens before persistence: bad key, bad kind, schema-less http.
	if rec := crudDo(t, s, http.MethodPost, "/api/tools/custom", `{"key":"Bad-Key","kind":"command"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad key: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tools/custom", `{"key":"`+key+`","kind":"rpc"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tools/custom", `{"key":"`+key+`","kind":"http"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("schema-less http: status=%d", rec.Code)
	}

	exec := `{"command":"echo {args}","timeout_ms":5000}`
	schema := `{"type":"object","properties":{"args":{"type":"string"}}}`
	body := `{"key":"` + key + `","description":"probe tool","kind":"command","enabled":true,"agents":["pentester"],"exec":` + exec + `}`
	rec := crudDo(t, s, http.MethodPost, "/api/tools/custom", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key string `json:"key"`
	}
	crudJSON(t, rec, &created)
	if created.Key != key {
		t.Fatalf("created=%+v", created)
	}

	// Duplicate key (custom or builtin) is a 409.
	if rec := crudDo(t, s, http.MethodPost, "/api/tools/custom", body); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: status=%d", rec.Code)
	}

	// Update: only custom rows are editable — unknown and system keys are 400.
	patch := `{"description":"updated tool","kind":"command","enabled":true,"exec":` + exec + `}`
	if rec := crudDo(t, s, http.MethodPut, "/api/tools/custom/"+key, patch); rec.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/tools/custom/no_such_tool_xyz", patch); rec.Code != http.StatusBadRequest {
		t.Fatalf("update unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/tools/custom/Bash", patch); rec.Code != http.StatusBadRequest {
		t.Fatalf("update system tool: status=%d", rec.Code)
	}
	got, err := m.pg.GetTool(key)
	if err != nil || got == nil || got.System || got.Description != "updated tool" {
		t.Fatalf("after update=%+v err=%v", got, err)
	}

	// http kind persists only with a real schema (server-side guard re-checked).
	rec = crudDo(t, s, http.MethodPost, "/api/tools/custom",
		`{"key":"`+key+`_http","kind":"http","schema":`+schema+`,"exec":{"url":"http://x/{p}"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("http create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete echoes the key; the row is gone afterwards.
	if rec := crudDo(t, s, http.MethodDelete, "/api/tools/custom/"+key, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	if gone, _ := m.pg.GetTool(key); gone != nil {
		t.Fatal("row survived delete")
	}

	// customTools skips shell-kind rows (env hints only, never callable tools).
	if rec := crudDo(t, s, http.MethodPost, "/api/tools/custom",
		`{"key":"`+key+`_shell","kind":"shell","exec":{"shell":"pwsh"}}`); rec.Code != http.StatusOK {
		t.Fatalf("shell create: status=%d", rec.Code)
	}
	tools, err := s.customTools()
	if err != nil {
		t.Fatalf("customTools: %v", err)
	}
	for _, tl := range tools {
		if tl.Name() == key+"_shell" {
			t.Fatalf("shell tool surfaced as callable: %s", tl.Name())
		}
	}
}

func TestCustomToolTestRoute(t *testing.T) {
	_, s := newTestStack(t)
	test := "/api/tools/custom/test"

	// shell is an environment declaration — explicitly not executable here;
	// unknown kinds and malformed bodies are 400s before any execution.
	if rec := crudDo(t, s, http.MethodPost, test, `{"kind":"shell"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("shell kind: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, test, `{"kind":"wat"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, test, `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", rec.Code)
	}

	var resp struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
	}

	// command: empty command is an error RESULT (200 + is_error), not HTTP 500.
	rec := crudDo(t, s, http.MethodPost, test, `{"kind":"command","exec":{"command":"  "}}`)
	crudJSON(t, rec, &resp)
	if !resp.IsError || resp.Output == "" {
		t.Fatalf("empty command resp=%+v", resp)
	}

	// command with a real command: the shared bash floor runs PowerShell on
	// Windows, so a PowerShell echo round-trips end to end.
	cmd := fmt.Sprintf(`Write-Output probe-%d`, time.Now().UnixNano())
	rec = crudDo(t, s, http.MethodPost, test, `{"kind":"command","exec":{"command":"`+cmd+`"}}`)
	crudJSON(t, rec, &resp)
	if resp.IsError || !strings.Contains(resp.Output, "probe-") {
		t.Fatalf("command run: %+v", resp)
	}

	// http: real request against httptest; {param} placeholders render from params.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pong":true}`))
	}))
	defer srv.Close()
	spec := fmt.Sprintf(`{"kind":"http","exec":{"method":"GET","url":"%s/ping/{name}","timeout_ms":5000},"params":{"name":"abc"}}`, srv.URL)
	rec = crudDo(t, s, http.MethodPost, test, spec)
	crudJSON(t, rec, &resp)
	// The body is embedded in the output JSON, so its quotes arrive escaped —
	// assert on substrings that survive the encoding.
	if resp.IsError || !strings.Contains(resp.Output, `"status":200`) || !strings.Contains(resp.Output, "pong") {
		t.Fatalf("http run: %+v", resp)
	}

	// http failure (unreachable port) is an error result, still HTTP 200.
	rec = crudDo(t, s, http.MethodPost, test, `{"kind":"http","exec":{"url":"http://127.0.0.1:1/x","timeout_ms":2000}}`)
	crudJSON(t, rec, &resp)
	if !resp.IsError {
		t.Fatalf("unreachable http: %+v", resp)
	}
}

func TestCustomToolGuardsAndBridge(t *testing.T) {
	m, s := newTestStack(t)

	// timeoutOr: non-positive falls back to the default.
	if d := timeoutOr(0, 5); d != 5*time.Millisecond {
		t.Fatalf("timeoutOr zero=%v", d)
	}
	if d := timeoutOr(-1, 5); d != 5*time.Millisecond {
		t.Fatalf("timeoutOr negative=%v", d)
	}
	if d := timeoutOr(250, 5); d != 250*time.Millisecond {
		t.Fatalf("timeoutOr explicit=%v", d)
	}

	// hasSchemaProps: only an object with ≥1 property counts.
	if hasSchemaProps(nil) || hasSchemaProps(json.RawMessage(`{}`)) ||
		hasSchemaProps(json.RawMessage(`{"properties":{}}`)) ||
		hasSchemaProps(json.RawMessage(`not-json`)) {
		t.Fatal("schema-less inputs accepted")
	}
	if !hasSchemaProps(json.RawMessage(`{"properties":{"p":{"type":"string"}}}`)) {
		t.Fatal("valid schema rejected")
	}

	// pythonInterpreter: a stored setting wins over live detection; blank stored
	// values fall through. Read-only check against whatever the shared DB has.
	stored, ok, _ := m.pg.GetSetting(settingPythonInterp)
	got := s.pythonInterpreter()
	if ok && strings.TrimSpace(stored) != "" {
		if got != strings.TrimSpace(stored) {
			t.Fatalf("stored setting ignored: got %q want %q", got, stored)
		}
	} else if got == "" {
		t.Log("no python on PATH — empty interpreter is the documented behavior")
	}

	// The built CoreTool dispatches by kind through its run closure — verified
	// end-to-end with an http tool against httptest, exactly like a real call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tool-run-ok"))
	}))
	defer srv.Close()

	key := fmt.Sprintf("built_tool_%d", time.Now().UnixNano())
	defer cleanupCustomTool(t, m, key)
	tool := &db.Tool{
		Key: key, Description: "built", Kind: "http",
		Schema: json.RawMessage(`{"type":"object","properties":{"base":{"type":"string"}}}`),
		Exec:   json.RawMessage(`{"method":"GET","url":"{base}/x"}`),
	}
	core := s.buildCustomTool(tool)
	if core.Name() != key {
		t.Fatalf("name=%q", core.Name())
	}
	input, _ := json.Marshal(map[string]any{"base": srv.URL})
	res, err := core.Call(context.Background(), input, &actool.ToolContext{WorkingDir: s.m.dir})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError || !strings.Contains(res.Flatten(), "tool-run-ok") {
		t.Fatalf("result=%+v", res)
	}

	// An unknown kind inside the row surfaces as an error result, not a panic.
	bad := s.buildCustomTool(&db.Tool{Key: "bad_kind_tool", Kind: "wat"})
	res, err = bad.Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError {
		t.Fatalf("bad kind result=%+v err=%v", res, err)
	}
}
