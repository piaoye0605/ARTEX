package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file pins the skill filesystem control plane (server_skills.go): name and
// relative-path validation, SKILL.md frontmatter rewriting, and the CRUD/upload
// routes. Everything runs against a real Manager and a per-test skillDir.

func TestSkillNameAndPathValidation(t *testing.T) {
	for _, name := range []string{"abc", "a1", "a-b", "技能", "技能2", "abc技能", "技能١"} {
		if !validSkillName(name) {
			t.Errorf("validSkillName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "A", "9a", "-a", "a-", "a--b", "a.b", "a b", "a/b", "a_b", strings.Repeat("a", 65)} {
		if validSkillName(name) {
			t.Errorf("validSkillName(%q) = true, want false", name)
		}
	}

	for _, p := range []string{"SKILL.md", "scripts/run.sh", "中文/说明.md", "a b/c.txt"} {
		if clean, msg := skillRelPath(p); clean == "" || msg != "" {
			t.Errorf("skillRelPath(%q) = %q/%q, want accepted", p, clean, msg)
		}
	}
	cases := map[string]string{
		"":                       "empty or too long",
		"../etc/passwd":          "'..' not allowed",
		"a/../../b":              "'..' not allowed",
		"a\\b":                   "illegal character",
		"a%2fb":                  "illegal character",
		"a#b":                    "illegal character",
		"a:b":                    "illegal character",
		"/abs":                   "must be relative",
		"a//b":                   "no empty segments",
		strings.Repeat("a", 600): "empty or too long",
	}
	for p, want := range cases {
		if _, msg := skillRelPath(p); !strings.Contains(msg, want) {
			t.Errorf("skillRelPath(%q) rejected with %q, want %q", p, msg, want)
		}
	}
	// 控制字符与非 ASCII 空格（NBSP）都必须在字符检查阶段就被拒绝。
	if _, msg := skillRelPath("a\x00b"); !strings.Contains(msg, "illegal character") {
		t.Errorf("NUL path verdict = %q", msg)
	}
	if _, msg := skillRelPath("a\u00a0b"); !strings.Contains(msg, "illegal character") {
		t.Errorf("NBSP path verdict = %q", msg)
	}
	if got := cleanStrs([]string{" a ", "", "  ", "b"}); strings.Join(got, ",") != "a,b" {
		t.Errorf("cleanStrs = %v", got)
	}
}

func TestRewriteSkillFrontmatterAndNameParse(t *testing.T) {
	body := "---\nname: probe\ndescription: old\nunknown: keep\n  plain line\n---\n# Body\n\nsteps\n"
	license := "MIT"
	compat := "linux"
	desc := " new description "
	mcps := []string{" fs ", "", "browser"}
	out, err := rewriteSkillFrontmatter([]byte(body), &mcps, &desc, &license, &compat)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got := string(out)
	for _, want := range []string{"description: new description", "license: MIT", "compatibility: linux", "mcps: fs, browser", "unknown: keep", "  plain line", "# Body"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rewritten frontmatter missing %q:\n%s", want, got)
		}
	}
	// mcps 传空切片 → 现有 mcps 行被移除
	empty := []string{}
	out, err = rewriteSkillFrontmatter(out, &empty, nil, nil, nil)
	if err != nil {
		t.Fatalf("rewrite clear: %v", err)
	}
	if strings.Contains(string(out), "mcps:") {
		t.Fatalf("mcps not cleared:\n%s", out)
	}
	// 没有 frontmatter / 未闭合 → 报错
	if _, err := rewriteSkillFrontmatter([]byte("# just markdown\n"), nil, nil, nil, nil); err == nil {
		t.Fatal("frontmatter-less file must fail")
	}
	if _, err := rewriteSkillFrontmatter([]byte("---\nname: x\n"), nil, nil, nil, nil); err == nil {
		t.Fatal("unclosed frontmatter must fail")
	}

	if got := skillNameFromFrontmatter([]byte("---\nname: \"中文技能\"\n---\n")); got != "中文技能" {
		t.Fatalf("quoted name = %q", got)
	}
	if got := skillNameFromFrontmatter([]byte("---\ndescription: x\n---\n")); got != "" {
		t.Fatalf("missing name = %q", got)
	}
	if got := skillNameFromFrontmatter([]byte("no frontmatter")); got != "" {
		t.Fatalf("no frontmatter name = %q", got)
	}
}

// skillZip builds an in-memory zip from name → content.
func skillZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatalf("zip create %s: %v", n, err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatalf("zip write %s: %v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// skillUploadDo posts a multipart body with (optionally) one file part.
func skillUploadDo(t *testing.T, s *Server, path string, payload []byte, field string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if field != "" {
		fw, err := mw.CreateFormFile(field, "probe.zip")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if _, err := fw.Write(payload); err != nil {
			t.Fatalf("form write: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSkillFilesystemRoutes(t *testing.T) {
	_, s := newTestStack(t)
	name := fmt.Sprintf("batch33-%d", time.Now().UnixNano())
	second := name + "-scaffold"
	t.Cleanup(func() {
		for _, n := range []string{name, second} {
			_ = crudDo(t, s, http.MethodDelete, "/api/skills/"+n, "")
		}
	})

	// --- create: malformed body, bad name, missing description, ok, duplicate ---
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"Bad Name","description":"d"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad name: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"`+name+`","description":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create no description: status=%d", rec.Code)
	}
	rec := crudDo(t, s, http.MethodPost, "/api/skills",
		`{"name":"`+name+`","description":"probe skill","license":"MIT","compatibility":"linux","mcps":[" fs ",""],"instructions":"## Steps\n1. probe"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"`+name+`","description":"d"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 不带 instructions/license → 脚手架正文分支
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"`+second+`","description":"scaffolded"}`); rec.Code != http.StatusCreated {
		t.Fatalf("scaffold create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- list: 目录项带 "/"，文件用 "/" 分隔 ---
	rec = crudDo(t, s, http.MethodGet, "/api/skills", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), name) {
		t.Fatalf("list skills: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), ".upload-") {
		t.Fatalf("list leaked temp upload dirs: %s", rec.Body.String())
	}

	// --- meta: 改 mcps/description，未知键保留 ---
	rec = crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/meta", `{"mcps":["browser"],"license":"Apache-2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta update: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files/SKILL.md", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mcps: browser") || !strings.Contains(rec.Body.String(), "license: Apache-2.0") {
		t.Fatalf("meta not persisted: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/bad.name/meta", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("meta bad name: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/meta", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("meta bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"-missing/meta", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("meta missing skill: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- files: 写入子目录、读取、列表、目录项、404 ---
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/files/scripts/run.sh", `{"content":"echo hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("write file: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files/scripts/run.sh", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo hi") {
		t.Fatalf("read file: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files/nope.txt", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("read missing file: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "scripts/run.sh") || !strings.Contains(rec.Body.String(), "scripts/") {
		t.Fatalf("list files: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+name+"-missing/files", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list missing skill files: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/files/..%2Fescape.txt", `{"content":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("write traversal path: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/files/ok.txt", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("write bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"-missing/files/ok.txt", `{"content":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("write into missing skill: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/bad.name/files/SKILL.md", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("read bad skill name: status=%d", rec.Code)
	}

	// --- dirs: 创建、非法路径、缺技能、坏 JSON ---
	if rec := crudDo(t, s, http.MethodPost, "/api/skills/"+name+"/dirs", `{"path":"assets"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create dir: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills/"+name+"/dirs", `{"path":"/abs"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create abs dir: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills/"+name+"/dirs", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create dir bad json: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills/"+name+"-missing/dirs", `{"path":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("create dir missing skill: status=%d", rec.Code)
	}

	// --- delete path: 文件删除、缺失、非法路径 ---
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+name+"/files/scripts/run.sh", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete file: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+name+"/files/scripts/run.sh", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing file: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+name+"/files/..%2Fescape.txt", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete traversal path: status=%d", rec.Code)
	}

	// --- usage / missing 列表 ---
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/usage?limit=5", ""); rec.Code != http.StatusOK {
		t.Fatalf("skill usage: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/bad.name/usage", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("skill usage bad name: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/missing?limit=3", ""); rec.Code != http.StatusOK {
		t.Fatalf("missing skills: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// --- delete skill: 非法名、成功、二次删除也返回 200（幂等） ---
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/bad.name", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete bad skill name: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+second, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete skill: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+second+"/files", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted skill still listed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSkillZipUpload(t *testing.T) {
	_, s := newTestStack(t)
	uploaded := fmt.Sprintf("batch33-up-%d", time.Now().UnixNano())
	derived := fmt.Sprintf("batch33-dir-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, n := range []string{uploaded, derived} {
			_ = crudDo(t, s, http.MethodDelete, "/api/skills/"+n, "")
		}
	})

	good := skillZip(t, map[string]string{
		"SKILL.md":     "---\nname: " + uploaded + "\ndescription: uploaded probe\n---\nbody\n",
		"scripts/a.sh": "echo a\n",
	})
	if rec := skillUploadDo(t, s, "/api/skills/upload", good, "file"); rec.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var up struct {
		Name  string `json:"name"`
		Files int    `json:"files"`
	}
	rec := skillUploadDo(t, s, "/api/skills/upload", good, "file")
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = skillUploadDo(t, s, "/api/skills/upload?overwrite=true", good, "file")
	if rec.Code != http.StatusCreated {
		t.Fatalf("overwrite upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &up)
	if up.Name != uploaded || up.Files != 2 {
		t.Fatalf("upload result = %+v", up)
	}

	// 缺少 file 字段、非 zip 内容、无 SKILL.md、SKILL.md 名字非法、路径穿越
	if rec := skillUploadDo(t, s, "/api/skills/upload", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("upload missing field: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := skillUploadDo(t, s, "/api/skills/upload", []byte("not a zip"), "file"); rec.Code != http.StatusBadRequest {
		t.Fatalf("upload non-zip: status=%d body=%s", rec.Code, rec.Body.String())
	}
	noMD := skillZip(t, map[string]string{"README.md": "hi"})
	if rec := skillUploadDo(t, s, "/api/skills/upload", noMD, "file"); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "未找到 SKILL.md") {
		t.Fatalf("upload without SKILL.md: status=%d body=%s", rec.Code, rec.Body.String())
	}
	badName := skillZip(t, map[string]string{"SKILL.md": "---\nname: Bad Name\n---\n"})
	if rec := skillUploadDo(t, s, "/api/skills/upload", badName, "file"); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "名称无效") {
		t.Fatalf("upload bad name: status=%d body=%s", rec.Code, rec.Body.String())
	}
	traversal := skillZip(t, map[string]string{
		"SKILL.md":   "---\nname: " + derived + "\n---\n",
		"../evil.sh": "x",
	})
	if rec := skillUploadDo(t, s, "/api/skills/upload", traversal, "file"); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "非法路径") {
		t.Fatalf("upload traversal: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// SKILL.md 在单层目录里 → 技能名回落到目录名
	nested := skillZip(t, map[string]string{
		derived + "/SKILL.md": "---\ndescription: nested probe\n---\n",
		derived + "/a.txt":    "x",
		"outside.txt":         "skipped",
	})
	rec = skillUploadDo(t, s, "/api/skills/upload", nested, "file")
	if rec.Code != http.StatusCreated {
		t.Fatalf("nested upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &up)
	if up.Name != derived || up.Files != 2 {
		t.Fatalf("nested upload result = %+v", up)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+derived+"/files", ""); !strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("nested upload files: %s", rec.Body.String())
	}
}
