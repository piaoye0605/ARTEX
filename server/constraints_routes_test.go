package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// This file pins the human constraint CRUD in constraints_api.go:
//   - kind is normalised (trim + lowercase) and must be allow|deny
//   - text is trimmed and required
//   - list order is allow before deny, ids ascending within each group
//   - editing/deleting a missing constraint is a 500 (store error), a bad
//     path id is a 400 — the two failure modes are deliberately distinct
//   - the injection switches default to true and follow the settings table

func constraintCall(t *testing.T, s *Server, method, target, id, cid, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, target, reader)
	req.SetPathValue("id", id)
	if cid != "" {
		req.SetPathValue("cid", cid)
	}
	rec := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		s.listConstraints(rec, req)
	case http.MethodPost:
		s.addConstraint(rec, req)
	case http.MethodPatch:
		s.editConstraint(rec, req)
	default:
		s.deleteConstraint(rec, req)
	}
	return rec
}

func TestConstraintRoutesLifecycle(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	task, err := m.CreateTask(fmt.Sprintf("constraints %d", stamp), "constraints goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDOf(task)
	pathID := i64s(taskID)
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	target := "/api/tasks/" + pathID + "/constraints"

	// A fresh task has no constraints; the list is an empty JSON array.
	rec := constraintCall(t, s, http.MethodGet, target, pathID, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Constraints []ConstraintDTO `json:"constraints"`
	}
	assetsDecode(t, rec, &listed)
	if len(listed.Constraints) != 0 {
		t.Fatalf("unexpected constraints=%+v", listed.Constraints)
	}

	// Kind is normalised (trim + lowercase) and text trimmed on the way in.
	rec = constraintCall(t, s, http.MethodPost, target, pathID, "", `{"text":"允许目录扫描","kind":"ALLOW"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add allow status=%d body=%s", rec.Code, rec.Body.String())
	}
	var allow ConstraintDTO
	assetsDecode(t, rec, &allow)
	if allow.Kind != "allow" || allow.Text != "允许目录扫描" || allow.Origin != "human" || allow.ID == "" {
		t.Fatalf("allow dto=%+v", allow)
	}
	rec = constraintCall(t, s, http.MethodPost, target, pathID, "", `{"text":"禁止 DoS","kind":"deny"}`)
	var deny ConstraintDTO
	assetsDecode(t, rec, &deny)
	if deny.Kind != "deny" || deny.ID == "" {
		t.Fatalf("deny dto=%+v", deny)
	}

	for name, badBody := range map[string]string{
		"blank text":  `{"text":"   ","kind":"allow"}`,
		"empty text":  `{"text":"","kind":"allow"}`,
		"bad kind":    `{"text":"x","kind":"block"}`,
		"missing all": `{}`,
		"bad json":    `{"text":`,
	} {
		if rec := constraintCall(t, s, http.MethodPost, target, pathID, "", badBody); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}

	// List order: allow before deny regardless of insertion order; ts present.
	rec = constraintCall(t, s, http.MethodGet, target, pathID, "", "")
	assetsDecode(t, rec, &listed)
	if len(listed.Constraints) != 2 ||
		listed.Constraints[0].Kind != "allow" || listed.Constraints[1].Kind != "deny" ||
		listed.Constraints[0].TS == "" {
		t.Fatalf("list=%+v", listed.Constraints)
	}

	// Edit rewrites kind + text.
	edited := `{"text":"允许子域枚举","kind":"deny"}`
	rec = constraintCall(t, s, http.MethodPatch, target+"/cid", pathID, allow.ID, edited)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var patch ConstraintDTO
	assetsDecode(t, rec, &patch)
	if patch.ID != allow.ID || patch.Kind != "deny" || patch.Text != "允许子域枚举" {
		t.Fatalf("patch dto=%+v", patch)
	}

	// Bad path ids are 400; a missing constraint is a store error (500).
	for _, bad := range []string{"abc", "0", "-2"} {
		if rec := constraintCall(t, s, http.MethodPatch, target+"/cid", pathID, bad, edited); rec.Code != http.StatusBadRequest {
			t.Fatalf("patch bad cid %q status=%d", bad, rec.Code)
		}
		if rec := constraintCall(t, s, http.MethodDelete, target+"/cid", pathID, bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("delete bad cid %q status=%d", bad, rec.Code)
		}
	}
	if rec := constraintCall(t, s, http.MethodPatch, target+"/cid", pathID, "999999999", edited); rec.Code != http.StatusInternalServerError {
		t.Fatalf("patch missing cid status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := constraintCall(t, s, http.MethodPatch, target+"/cid", pathID, deny.ID, `{"text":"  ","kind":"deny"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch blank text status=%d", rec.Code)
	}

	// Delete then delete again: 200 then a store error.
	if rec := constraintCall(t, s, http.MethodDelete, target+"/cid", pathID, deny.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := constraintCall(t, s, http.MethodDelete, target+"/cid", pathID, deny.ID, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("double delete status=%d", rec.Code)
	}
	rec = constraintCall(t, s, http.MethodGet, target, pathID, "", "")
	assetsDecode(t, rec, &listed)
	if len(listed.Constraints) != 1 || listed.Constraints[0].ID != allow.ID {
		t.Fatalf("post-delete list=%+v", listed.Constraints)
	}

	// Unknown task is a 404 for every method.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if rec := constraintCall(t, s, method, "/api/tasks/999999999/constraints", "999999999", "1", `{}`); rec.Code != http.StatusNotFound {
			t.Fatalf("%s unknown task status=%d", method, rec.Code)
		}
	}
}

func TestConstraintInjectionSwitches(t *testing.T) {
	m, s := newTestStack(t)
	if !s.constraintInjectPlanner() || !s.constraintInjectWorker() {
		t.Fatal("injection switches must default to true")
	}
	if err := m.pg.SetSetting(settingConstraintsInjectPlanner, "false"); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(settingConstraintsInjectWorker, "false"); err != nil {
		t.Fatal(err)
	}
	if s.constraintInjectPlanner() || s.constraintInjectWorker() {
		t.Fatal("injection switches did not follow the settings table")
	}
	// Restore the defaults for other tests sharing the database.
	_ = m.pg.SetSetting(settingConstraintsInjectPlanner, "true")
	_ = m.pg.SetSetting(settingConstraintsInjectWorker, "true")
}

func TestNormalizeConstraintKindAndDTOs(t *testing.T) {
	for in, want := range map[string]string{
		"allow": "allow", " DENY ": "deny", "Allow": "allow",
		"block": "", "": "", " allow extra": "",
	} {
		if got := normalizeConstraintKind(in); got != want {
			t.Fatalf("normalizeConstraintKind(%q)=%q want %q", in, got, want)
		}
	}
	// The converter never returns nil so JSON renders [] rather than null.
	if got := constraintDTOs(nil); got == nil || len(got) != 0 {
		t.Fatalf("constraintDTOs(nil)=%#v", got)
	}
}
