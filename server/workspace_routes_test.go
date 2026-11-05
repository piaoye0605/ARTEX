package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the contracts of the workspace file manager (workspace.go):
// browse / read / write / mkdir / delete / download / upload of the shared work
// dir. The layer is pure filesystem — no database — so every test asserts real
// on-disk effects against newTestStack's temp work dir (s.m.dir).
//
// The security property under test: user-supplied paths are *neutralized*
// (filepath.Clean collapses ".."), never executed — nothing can escape the root.

func TestWorkspaceListAndReadRoutes(t *testing.T) {
	m, s := newTestStack(t)
	root := m.dir

	// Fixture: one subdir, one text file, one binary file (NUL byte → not utf8
	// inlinable), one oversized file (> 2 MiB read cap).
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), maxWorkspaceRead+1)
	if err := os.WriteFile(filepath.Join(root, "big.log"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	// Root listing: path param empty is legal, reports "" and sorts dirs first.
	var list struct {
		Path    string `json:"path"`
		Entries []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			Dir   bool   `json:"dir"`
			Size  int64  `json:"size"`
			MTime int64  `json:"mtime"`
		} `json:"entries"`
	}
	rec := crudDo(t, s, http.MethodGet, "/api/workspace/list", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list root: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := decodeJSON(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Path != "" {
		t.Fatalf("root path=%q want empty", list.Path)
	}
	if len(list.Entries) < 4 {
		t.Fatalf("root entries=%d body=%s", len(list.Entries), rec.Body.String())
	}
	// Dirs first, then case-insensitive by name.
	for i := 1; i < len(list.Entries); i++ {
		prev, cur := list.Entries[i-1], list.Entries[i]
		if prev.Dir != cur.Dir && !prev.Dir {
			t.Fatalf("file sorted before dir: %+v then %+v", prev, cur)
		}
		if prev.Dir == cur.Dir && strings.ToLower(prev.Name) > strings.ToLower(cur.Name) {
			t.Fatalf("name order broken: %q then %q", prev.Name, cur.Name)
		}
	}
	var sawDir bool
	for _, e := range list.Entries {
		if e.Name == "sub" {
			sawDir = e.Dir
			if e.Path != "sub" {
				t.Fatalf("entry path=%q want forward-slash rel path", e.Path)
			}
			if e.MTime <= 0 {
				t.Fatalf("mtime=%d", e.MTime)
			}
		}
	}
	if !sawDir {
		t.Fatal("subdir missing from listing")
	}

	// Traversal is neutralized, not rejected: "../a.txt" lands on root/a.txt.
	rec = crudDo(t, s, http.MethodGet, "/api/workspace/read?path="+url.QueryEscape("../a.txt"), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello workspace") {
		t.Fatalf("traversal read: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Deep climb still lands inside the root ("a/../../a.txt" → "/a.txt").
	rec = crudDo(t, s, http.MethodGet, "/api/workspace/read?path="+url.QueryEscape("sub/../../a.txt"), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello workspace") {
		t.Fatalf("deep traversal: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Text read: full envelope.
	var text struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		Binary  bool   `json:"binary"`
		Content string `json:"content"`
	}
	if err := decodeJSON(crudDo(t, s, http.MethodGet, "/api/workspace/read?path=a.txt", "").Body.Bytes(), &text); err != nil {
		t.Fatal(err)
	}
	if text.Binary || text.Content != "hello workspace" || text.Size != 15 {
		t.Fatalf("text read=%+v", text)
	}

	// Binary read reports binary without content (200, not an error). Fresh
	// struct: absent JSON keys don't zero fields left over from a previous decode.
	var bin struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		Binary  bool   `json:"binary"`
		Content string `json:"content"`
	}
	if err := decodeJSON(crudDo(t, s, http.MethodGet, "/api/workspace/read?path=bin.dat", "").Body.Bytes(), &bin); err != nil {
		t.Fatal(err)
	}
	if !bin.Binary || bin.Content != "" {
		t.Fatalf("binary read=%+v", bin)
	}

	// Oversize read: too_large + binary, still 200 — the client is expected to
	// fall back to download, so this is deliberately not a 4xx.
	var bigResp struct {
		TooLarge bool  `json:"too_large"`
		Binary   bool  `json:"binary"`
		Size     int64 `json:"size"`
	}
	if err := decodeJSON(crudDo(t, s, http.MethodGet, "/api/workspace/read?path=big.log", "").Body.Bytes(), &bigResp); err != nil {
		t.Fatal(err)
	}
	if !bigResp.TooLarge || !bigResp.Binary || bigResp.Size != int64(len(big)) {
		t.Fatalf("oversize read=%+v", bigResp)
	}

	// Guards: missing → 404, dir-as-file → 400, list on file → 400.
	if rec := crudDo(t, s, http.MethodGet, "/api/workspace/read?path=nope.txt", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing read: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/workspace/read?path=sub", ""); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "是目录") {
		t.Fatalf("dir read: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/workspace/list?path=a.txt", ""); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "不是目录") {
		t.Fatalf("list on file: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/workspace/list?path=../nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list missing: status=%d", rec.Code)
	}
}

func TestWorkspaceWriteMkdirDeleteRoutes(t *testing.T) {
	m, s := newTestStack(t)
	root := m.dir

	// Write creates missing parent dirs and returns the normalized rel path.
	rec := crudDo(t, s, http.MethodPost, "/api/workspace/write", `{"path":"deep/nested/out.txt","content":"line1\nline2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write: status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "deep", "nested", "out.txt"))
	if err != nil || string(data) != "line1\nline2" {
		t.Fatalf("written file=%q err=%v", data, err)
	}

	// Overwrite replaces the whole content.
	crudDo(t, s, http.MethodPost, "/api/workspace/write", `{"path":"deep/nested/out.txt","content":"v2"}`)
	if data, err = os.ReadFile(filepath.Join(root, "deep", "nested", "out.txt")); err != nil || string(data) != "v2" {
		t.Fatalf("overwrite=%q err=%v", data, err)
	}

	// Guards: root itself, dir target, bad json.
	if rec := crudDo(t, s, http.MethodPost, "/api/workspace/write", `{"path":"","content":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("write root: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/workspace/write", `{"path":"deep","content":"x"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "目标是目录") {
		t.Fatalf("write onto dir: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/workspace/write", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("write bad json: status=%d", rec.Code)
	}

	// MkdirAll semantics + the same root guard.
	if rec := crudDo(t, s, http.MethodPost, "/api/workspace/mkdir", `{"path":"x/y/z"}`); rec.Code != http.StatusOK {
		t.Fatalf("mkdir: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fi, err := os.Stat(filepath.Join(root, "x", "y", "z")); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir not on disk: %v", err)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/workspace/mkdir", `{"path":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("mkdir root: status=%d", rec.Code)
	}

	// Delete removes a file; a second delete 404s.
	if rec := crudDo(t, s, http.MethodDelete, "/api/workspace/delete?path=deep/nested/out.txt", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete file: status=%d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "deep", "nested", "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("file survived delete: %v", err)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/workspace/delete?path=deep/nested/out.txt", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete: status=%d", rec.Code)
	}

	// Delete removes a whole subtree.
	if rec := crudDo(t, s, http.MethodDelete, "/api/workspace/delete?path=x", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete tree: status=%d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); !os.IsNotExist(err) {
		t.Fatalf("tree survived delete: %v", err)
	}

	// The work-dir root itself is protected.
	if rec := crudDo(t, s, http.MethodDelete, "/api/workspace/delete?path=", ""); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "根目录") {
		t.Fatalf("delete root: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkspaceDownloadAndUploadRoutes(t *testing.T) {
	m, s := newTestStack(t)
	root := m.dir
	if err := os.MkdirAll(filepath.Join(root, "uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "报告 一号.txt"), []byte("中文内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Download streams the file with an attachment disposition; RFC 5987
	// filename* carries the UTF-8 name, plain filename is a sanitized fallback.
	rec := crudDo(t, s, http.MethodGet, "/api/workspace/download?path="+url.QueryEscape("报告 一号.txt"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download: status=%d body=%s", rec.Code, rec.Body.String())
	}
	disp := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disp, "attachment") || !strings.Contains(disp, "filename*=UTF-8''") {
		t.Fatalf("disposition=%q", disp)
	}
	if body, _ := io.ReadAll(rec.Body); string(body) != "中文内容" {
		t.Fatalf("download body=%q", body)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/workspace/download?path=missing.bin", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("download missing: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/workspace/download?path=uploads", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("download dir: status=%d", rec.Code)
	}

	// Upload: two files in one request land in the target dir; a filename with a
	// path component is stripped to its base before saving.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range map[string]string{
		"one.txt":            "file one",
		"evil/../../two.txt": "file two",
	} {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "/api/workspace/upload?path=uploads", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"uploaded":2`) {
		t.Fatalf("upload count: %s", rr.Body.String())
	}
	for name, want := range map[string]string{"one.txt": "file one", "two.txt": "file two"} {
		data, err := os.ReadFile(filepath.Join(root, "uploads", name))
		if err != nil || string(data) != want {
			t.Fatalf("upload %s: got %q err=%v", name, data, err)
		}
	}
	// The path component never escaped the target dir.
	if _, err := os.Stat(filepath.Join(root, "evil")); !os.IsNotExist(err) {
		t.Fatalf("upload filename path component created a dir: %v", err)
	}

	// Guards: missing target dir, no file field.
	if rec := crudDo(t, s, http.MethodPost, "/api/workspace/upload?path=no-such-dir", ""); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "目标目录不存在") {
		t.Fatalf("upload missing dir: status=%d body=%s", rec.Code, rec.Body.String())
	}
	emptyBuf := &bytes.Buffer{}
	emptyMw := multipart.NewWriter(emptyBuf)
	_ = emptyMw.WriteField("other", "x")
	_ = emptyMw.Close()
	req2, _ := http.NewRequest(http.MethodPost, "/api/workspace/upload?path=uploads", emptyBuf)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", emptyMw.FormDataContentType())
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusBadRequest || !strings.Contains(rr2.Body.String(), "缺少上传文件") {
		t.Fatalf("upload no file: status=%d body=%s", rr2.Code, rr2.Body.String())
	}
}

func TestWorkspacePathHelpers(t *testing.T) {
	m, s := newTestStack(t)

	// wsRel: the root maps to "", nested paths use forward slashes even on Windows.
	if got := s.wsRel(filepath.Clean(m.dir)); got != "" {
		t.Fatalf("wsRel(root)=%q", got)
	}
	nested := filepath.Join(m.dir, "a", "b.txt")
	if got := s.wsRel(nested); got != "a/b.txt" {
		t.Fatalf("wsRel(nested)=%q", got)
	}

	// sanitizeFilename strips the characters that would break a quoted
	// Content-Disposition token: quotes, backslashes, CR/LF.
	for _, tc := range []struct{ in, want string }{
		{`re"port.txt`, "report.txt"},
		{`back\slash.txt`, "backslash.txt"},
		{"line1\nline2\r.txt", "line1line2.txt"},
		{"clean.txt", "clean.txt"},
	} {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Fatalf("sanitizeFilename(%q)=%q want %q", tc.in, got, tc.want)
		}
	}

	// wsResolve: empty → root; traversal collapses into the root; rooted paths
	// are returned absolute. It never returns a path outside m.dir.
	if abs, ok := s.wsResolve(""); !ok || filepath.Clean(abs) != filepath.Clean(m.dir) {
		t.Fatalf("wsResolve(empty)=(%q,%v)", abs, ok)
	}
	abs, ok := s.wsResolve("../..")
	if !ok || !strings.HasPrefix(abs, filepath.Clean(m.dir)) {
		t.Fatalf("wsResolve traversal=(%q,%v)", abs, ok)
	}
}

// decodeJSON decodes one workspace envelope response body.
func decodeJSON(data []byte, dst any) error {
	return json.Unmarshal(data, dst)
}
