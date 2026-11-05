package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Coverage for task_intercept.go (task-scoped asset block/allow rules) and
// chatupload.go (method-1 file uploads). Both are thin control-plane layers,
// so the tests pin the full request/response contract: validation errors,
// CRUD round trips, and the upload dir/naming rules.

// --- task_intercept.go ---

func TestTaskInterceptRulesCRUD(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	task, err := m.CreateTask(fmt.Sprintf("intercept %d", stamp), "intercept goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	base := "/api/tasks/" + task.ID + "/intercept-rules"

	// Validation happens before any DB write.
	if rec := crudDo(t, s, http.MethodPost, base, `{"action":"maybe","kind":"exact_domain","pattern":"x.com"}`); rec.Code != http.StatusBadRequest ||
		!bytes.Contains(rec.Body.Bytes(), []byte("block 或 allow")) {
		t.Fatalf("bad action: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"kind":"exact_domain","pattern":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank pattern: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"kind":"domain","pattern":"x.com"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{"kind":"exact_ip","pattern":"999.1.1.1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ip: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, base, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/abc/intercept-rules", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad task id: status=%d", rec.Code)
	}

	// Create with defaults: action defaults to block, pattern is trimmed.
	rec := crudDo(t, s, http.MethodPost, base, `{"kind":"exact_domain","pattern":" secret.example.com ","note":"leak"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rule db.AssetInterceptRule
	crudJSON(t, rec, &rule)
	if rule.ID == 0 || rule.Action != "block" || rule.Pattern != "secret.example.com" {
		t.Fatalf("created rule=%+v", rule)
	}

	// List shows it.
	rec = crudDo(t, s, http.MethodGet, base, "")
	var list struct {
		Rules []db.AssetInterceptRule `json:"rules"`
	}
	crudJSON(t, rec, &list)
	if len(list.Rules) != 1 || list.Rules[0].ID != rule.ID {
		t.Fatalf("list=%+v", list.Rules)
	}

	// Update switches it to an allow rule.
	rid := strconv.FormatInt(rule.ID, 10)
	rec = crudDo(t, s, http.MethodPut, base+"/"+rid, `{"action":"allow","kind":"fuzzy_url","pattern":"/status","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &rule)
	if rule.Action != "allow" || rule.Kind != "fuzzy_url" {
		t.Fatalf("updated rule=%+v", rule)
	}

	// Toggle flips enabled without touching the rest.
	rec = crudDo(t, s, http.MethodPost, base+"/"+rid+"/toggle", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Updating a missing rule errors; deleting one reports deleted=false.
	if rec := crudDo(t, s, http.MethodPut, base+"/999999", `{"kind":"exact_domain","pattern":"x.com"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("update missing: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodDelete, base+"/999999", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete missing: status=%d", rec.Code)
	}
	var miss struct {
		Deleted bool `json:"deleted"`
	}
	crudJSON(t, rec, &miss)
	if miss.Deleted {
		t.Fatalf("delete missing reported success: %+v", miss)
	}

	// Delete removes it; the list is back to (non-nil) empty.
	rec = crudDo(t, s, http.MethodDelete, base+"/"+rid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var del struct {
		Deleted bool `json:"deleted"`
	}
	crudJSON(t, rec, &del)
	if !del.Deleted {
		t.Fatalf("deleted flag=%+v", del)
	}
	rec = crudDo(t, s, http.MethodGet, base, "")
	crudJSON(t, rec, &list)
	if len(list.Rules) != 0 {
		t.Fatalf("list after delete=%+v", list.Rules)
	}
}

// --- chatupload.go ---

// multipartUpload builds a POST /api/chat/upload request with the given files.
func multipartUpload(t *testing.T, s *Server, query string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload"+query, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

type uploadedAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	Abs  string `json:"abs"`
}

func TestChatUploadValidation(t *testing.T) {
	_, s := newTestStack(t)
	h := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	// Query-string validation fires before multipart parsing.
	for _, q := range []string{"?scope=carrier", "?scope=task&id=../etc", "?scope=session&id=a/b"} {
		req := httptest.NewRequest(http.MethodPost, "/api/chat/upload"+q, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("upload %q: status=%d body=%s (want 400)", q, rec.Code, rec.Body.String())
		}
	}

	// scope=task on a missing task id is 404.
	rec := multipartUpload(t, s, "?scope=task&id=999999", map[string]string{"a.txt": "hi"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing task: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A task marked deleting refuses uploads (operation gate).
	m, s2 := newTestStack(t)
	stamp := time.Now().UnixNano()
	task, err := m.CreateTask(fmt.Sprintf("upload del %d", stamp), "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	s2.engine.deleting.Store(task.ID, true)
	rec = multipartUpload(t, s2, "?scope=task&id="+task.ID, map[string]string{"a.txt": "hi"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting task: status=%d body=%s (want 409)", rec.Code, rec.Body.String())
	}
	s2.engine.deleting.Delete(task.ID)

	// A body that is not multipart fails parsing with 400.
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope=staging&id=draft-x", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "text/plain")
	token, err2 := signJWT(s.jwtKey)
	if err2 != nil {
		t.Fatalf("sign jwt: %v", err2)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-multipart: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Multipart without any "file" field is 400.
	if rec := multipartUpload(t, s, "?scope=staging&id=draft-1", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no file: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChatUploadStagingAndTaskScopes(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()

	// staging scope lands in drafts/<id>/uploads and needs no task.
	rec := multipartUpload(t, s, "?scope=staging&id=draft-"+strconv.FormatInt(stamp, 10),
		map[string]string{"report.txt": "附件正文"})
	if rec.Code != http.StatusOK {
		t.Fatalf("staging upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Attachments []uploadedAttachment `json:"attachments"`
	}
	crudJSON(t, rec, &out)
	if len(out.Attachments) != 1 {
		t.Fatalf("attachments=%+v", out.Attachments)
	}
	att := out.Attachments[0]
	if att.Name != "report.txt" || att.Path != "uploads/report.txt" || att.Size != int64(len("附件正文")) || att.Abs == "" {
		t.Fatalf("attachment=%+v", att)
	}
	if _, err := os.Stat(att.Abs); err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}

	// Re-uploading the same name never clobbers: the second copy is name-1.
	rec = multipartUpload(t, s, "?scope=staging&id=draft-"+strconv.FormatInt(stamp, 10),
		map[string]string{"report.txt": "第二份"})
	crudJSON(t, rec, &out)
	if len(out.Attachments) != 1 || out.Attachments[0].Name != "report-1.txt" {
		t.Fatalf("dedup attachment=%+v", out.Attachments)
	}

	// task scope resolves the real task dir.
	task, err := m.CreateTask(fmt.Sprintf("upload %d", stamp), "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	rec = multipartUpload(t, s, "?scope=task&id="+task.ID, map[string]string{"notes.md": "# notes"})
	if rec.Code != http.StatusOK {
		t.Fatalf("task upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &out)
	if len(out.Attachments) != 1 || out.Attachments[0].Path != "uploads/notes.md" {
		t.Fatalf("task attachment=%+v", out.Attachments)
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "tasks", task.ID, "uploads", "notes.md")); err != nil {
		t.Fatalf("task upload dir wrong: %v", err)
	}
}

// buildTaskInterceptRules (used by the create-task API) validates and converts
// each entry; an empty list is a nil-no-op.
func TestBuildTaskInterceptRules(t *testing.T) {
	if out, _ := buildTaskInterceptRules(nil); out != nil {
		t.Fatalf("empty rules=%+v", out)
	}
	out, err := buildTaskInterceptRules([]taskInterceptRuleReq{
		{Kind: "exact_domain", Pattern: " a.example.com "},
		{Enabled: true, Action: "allow", Kind: "cidr", Pattern: "10.0.0.0/8", Note: "lab"},
	})
	if err != nil {
		t.Fatalf("valid rules: %v", err)
	}
	if len(out) != 2 || out[0].Action != "block" || out[0].Pattern != "a.example.com" ||
		out[1].Action != "allow" || out[1].Kind != "cidr" {
		t.Fatalf("out=%+v", out)
	}
	if _, err := buildTaskInterceptRules([]taskInterceptRuleReq{{Kind: "bogus", Pattern: "x"}}); err == nil {
		t.Fatal("invalid kind accepted")
	}
}

func TestChatUploadPureHelpers(t *testing.T) {
	dir := t.TempDir()
	// uniqueUploadPath walks name-1, name-2… until a free slot.
	first := uniqueUploadPath(dir, "a.log")
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := uniqueUploadPath(dir, "a.log")
	if second == first || filepath.Base(second) != "a-1.log" {
		t.Fatalf("second slot=%q", second)
	}

	// composeAgentMessage is identity without attachments; with attachments it
	// appends an absolute-path manifest.
	if got := composeAgentMessage("看下这个", nil, dir); got != "看下这个" {
		t.Fatalf("no-att compose=%q", got)
	}
	atts := []chatAttachment{{Name: "a.log", Path: "uploads/a.log", Size: 2 << 20}}
	got := composeAgentMessage("看下这个", atts, dir)
	if !bytes.Contains([]byte(got), []byte("看下这个")) ||
		!bytes.Contains([]byte(got), []byte(filepath.Join(dir, "uploads/a.log"))) ||
		!bytes.Contains([]byte(got), []byte("2.0 MB")) {
		t.Fatalf("compose=%q", got)
	}

	// userActivityWithAttachments keeps Summary plain and packs the detail.
	a := userActivityWithAttachments("mainagent", "正文", atts)
	if a.Kind != "user" || a.Worker != "mainagent" || a.Summary != "正文" {
		t.Fatalf("activity=%+v", a)
	}
	var detail struct {
		Text        string           `json:"text"`
		Attachments []chatAttachment `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(a.Detail), &detail); err != nil || detail.Text != "正文" || len(detail.Attachments) != 1 {
		t.Fatalf("detail=%q err=%v", a.Detail, err)
	}
	bare := userActivityWithAttachments("mainagent", "裸", nil)
	if bare.Detail != "" {
		t.Fatalf("bare detail=%q", bare.Detail)
	}

	// humanBytes covers all three branches.
	for n, want := range map[int64]string{512: "512 B", 4096: "4.0 KB", 3 << 20: "3.0 MB"} {
		if got := humanBytes(n); got != want {
			t.Fatalf("humanBytes(%d)=%q want %q", n, got, want)
		}
	}
}
