package server

import (
	"bytes"
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

// ---------- pure helpers ----------

func TestCleanStrs(t *testing.T) {
	got := cleanStrs([]string{" a ", "", "b", "\t", "c d"})
	want := []string{"a", "b", "c d"}
	if len(got) != len(want) {
		t.Fatalf("cleanStrs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cleanStrs = %q, want %q", got, want)
		}
	}
	if r := cleanStrs(nil); r == nil || len(r) != 0 {
		t.Fatalf("cleanStrs(nil) = %v, want empty non-nil", r)
	}
}

func TestRewriteSkillFrontmatter(t *testing.T) {
	// No frontmatter / unclosed frontmatter are hard errors.
	for _, bad := range []string{"no frontmatter at all", "---\nname: x\nbody continues"} {
		if _, err := rewriteSkillFrontmatter([]byte(bad), nil, nil, nil, nil); err == nil {
			t.Errorf("rewriteSkillFrontmatter(%q) succeeded, want error", bad)
		}
	}

	const src = "---\nname: demo\ndescription: old\ninvalid-key without colon value\nlicense: MIT\n---\n\n# Body\n\nstep 1\n"
	desc := "new description"
	lic := "Apache-2.0"
	mcps := []string{" mcp-a ", "", "mcp-b"}

	out, err := rewriteSkillFrontmatter([]byte(src), &mcps, &desc, &lic, nil)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"description: new description",
		"license: Apache-2.0",
		"mcps: mcp-a, mcp-b",
		"invalid-key without colon value", // non-key line preserved verbatim
		"name: demo",                      // untouched key preserved
		"# Body\n\nstep 1\n",              // instruction body preserved
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "old") {
		t.Errorf("stale description survived:\n%s", s)
	}

	// mcps=nil leaves the line untouched; mcps=[] (non-nil empty) removes it.
	kept, err := rewriteSkillFrontmatter([]byte(src), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kept), "license: MIT") {
		t.Errorf("nil pointers must keep original values:\n%s", kept)
	}
	removed, err := rewriteSkillFrontmatter([]byte(src), &[]string{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(removed), "mcps") {
		t.Errorf("empty mcps slice must drop the line:\n%s", removed)
	}

	// A value update for a key that does not exist yet is appended.
	compat := "go1.22+"
	appended, err := rewriteSkillFrontmatter([]byte(src), nil, nil, nil, &compat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appended), "compatibility: go1.22+") {
		t.Errorf("new key not appended:\n%s", appended)
	}
}

// ---------- HTTP: create / list / update meta ----------

// skillTestName yields a per-run skill name: validSkillName allows [a-z0-9-] only
// (no underscore), and the shared postgres visibility/usage tables need uniqueness.
func skillTestName(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func TestSkillCreateAndList(t *testing.T) {
	m, s := newTestStack(t)
	_ = m
	name := skillTestName("skilltest")

	// Invalid name / missing description are rejected before any disk write.
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"Bad Name","description":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"skilltest","description":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty description: status=%d", rec.Code)
	}

	rec := crudDo(t, s, http.MethodPost, "/api/skills",
		`{"name":"`+name+`","description":"demo skill","mcps":["mcp-a"],"instructions":"## Steps\n\n1. do it"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Duplicate create conflicts.
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"`+name+`","description":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: status=%d", rec.Code)
	}

	// The scaffolded SKILL.md is spec-shaped: frontmatter + given instructions.
	raw, err := os.ReadFile(filepath.Join(s.skillDir, name, "SKILL.md"))
	if err != nil {
		t.Fatalf("scaffold missing: %v", err)
	}
	for _, want := range []string{"name: " + name, "description: demo skill", "mcps: mcp-a", "## Steps"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("SKILL.md missing %q:\n%s", want, raw)
		}
	}

	// Empty instructions scaffold a numbered template instead.
	scaffold := skillTestName("skilltest")
	if rec := crudDo(t, s, http.MethodPost, "/api/skills", `{"name":"`+scaffold+`","description":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("scaffold create: status=%d", rec.Code)
	}
	raw2, _ := os.ReadFile(filepath.Join(s.skillDir, scaffold, "SKILL.md"))
	if !strings.Contains(string(raw2), "1. \n2. \n3. \n") {
		t.Errorf("scaffold body missing template:\n%s", raw2)
	}

	// The list decorates from the usage ledger and the frontmatter.
	if err := m.pg.InsertSkillUsage(&db.SkillUsage{Skill: name, AgentKey: "planner", ArgsLen: 10, Found: true}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	rec = crudDo(t, s, http.MethodGet, "/api/skills", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d", rec.Code)
	}
	var list struct {
		Skills []struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			MCPs        []string `json:"mcps"`
			Files       []string `json:"files"`
			Calls       int      `json:"calls"`
			Agents      []string `json:"usage_agents"`
		} `json:"skills"`
	}
	crudJSON(t, rec, &list)
	var found *int
	for i := range list.Skills {
		if list.Skills[i].Name == name {
			found = &i
		}
	}
	if found == nil {
		t.Fatalf("created skill not listed: %+v", list.Skills)
	}
	node := list.Skills[*found]
	if node.Description != "demo skill" || node.Calls != 1 || len(node.MCPs) != 1 || len(node.Files) == 0 {
		t.Fatalf("listed node = %+v", node)
	}
	if len(node.Agents) != 1 || node.Agents[0] != "planner" {
		t.Fatalf("usage agents = %v", node.Agents)
	}

	// cleanup: remove the on-disk skills (PG usage rows are keyed by unique name).
	_ = os.RemoveAll(filepath.Join(s.skillDir, name))
	_ = os.RemoveAll(filepath.Join(s.skillDir, scaffold))
}

func TestSkillUpdateMeta(t *testing.T) {
	_, s := newTestStack(t)
	name := skillTestName("skilltest")
	body := "---\nname: " + name + "\ndescription: old\n---\n\nkeep this body\n"
	if err := os.MkdirAll(filepath.Join(s.skillDir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.skillDir, name, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if rec := crudDo(t, s, http.MethodPut, "/api/skills/BadName/meta", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/nope-meta/meta", `{"description":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown skill: status=%d", rec.Code)
	}

	rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/meta",
		`{"description":"updated","mcps":["m1","m2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(filepath.Join(s.skillDir, name, "SKILL.md"))
	out := string(raw)
	if !strings.Contains(out, "description: updated") || !strings.Contains(out, "mcps: m1, m2") {
		t.Errorf("frontmatter not updated:\n%s", out)
	}
	if !strings.Contains(out, "keep this body") {
		t.Errorf("body clobbered:\n%s", out)
	}
	_ = os.RemoveAll(filepath.Join(s.skillDir, name))
}

// ---------- HTTP: file management ----------

func TestSkillFileCRUD(t *testing.T) {
	_, s := newTestStack(t)
	name := skillTestName("skilltest")
	if err := os.MkdirAll(filepath.Join(s.skillDir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.skillDir, name, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unknown skill 404s on every file verb.
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/nope-files/files", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPut, "/api/skills/nope-files/files/a.md", `{"content":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("write unknown: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/skills/nope-files/dirs", `{"path":"d"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("mkdir unknown: status=%d", rec.Code)
	}

	// Invalid paths are refused with 400 before touching the filesystem.
	for _, p := range []string{"..%2fx", "a%2f%2fb"} {
		if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files/"+p, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad path %q: status=%d", p, rec.Code)
		}
	}

	// Create a dir, write a nested file, read it back, list, delete.
	if rec := crudDo(t, s, http.MethodPost, "/api/skills/"+name+"/dirs", `{"path":"scripts"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mkdir: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := crudDo(t, s, http.MethodPut, "/api/skills/"+name+"/files/scripts/run.py", `{"content":"print(1)"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write: status=%d", rec.Code)
	}
	rec = crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files/scripts/run.py", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status=%d", rec.Code)
	}
	var fileOut struct {
		Content string `json:"content"`
		File    string `json:"file"`
	}
	crudJSON(t, rec, &fileOut)
	if fileOut.Content != "print(1)" || fileOut.File != "scripts/run.py" {
		t.Fatalf("read back = %+v", fileOut)
	}
	if _, err := os.Stat(filepath.Join(s.skillDir, name, "scripts", "run.py")); err != nil {
		t.Fatalf("file not on disk: %v", err)
	}

	rec = crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files", "")
	var list struct {
		Files []string `json:"files"`
	}
	crudJSON(t, rec, &list)
	if len(list.Files) != 3 { // SKILL.md + scripts/ + scripts/run.py
		t.Fatalf("files = %q", list.Files)
	}

	// Missing file read is 404; unknown delete is 404.
	if rec := crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/files/ghost.md", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("read missing: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+name+"/files/ghost.md", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+name+"/files/scripts/run.py", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(s.skillDir, name, "scripts", "run.py")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still on disk: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(s.skillDir, name))
}

// ---------- HTTP: usage ledger / missing list ----------

func TestSkillUsageAndMissing(t *testing.T) {
	m, s := newTestStack(t)
	name := skillTestName("skilltest")
	if err := os.MkdirAll(filepath.Join(s.skillDir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Join(s.skillDir, name))

	// Two resolved calls (planner ×2, mainagent ×1) and one miss. The ledger's
	// newest-first ordering is by ts; sleep between inserts so the timestamps
	// don't tie within one microsecond.
	seeds := []db.SkillUsage{
		{Skill: name, AgentKey: "planner", ArgsLen: 42, Found: true},
		{Skill: name, AgentKey: "planner", ArgsLen: 7, Found: true},
		{Skill: name, AgentKey: "mainagent", ArgsLen: 1, Found: true},
		{Skill: name + "-ghost", AgentKey: "worker", Found: false},
	}
	for i := range seeds {
		if err := m.pg.InsertSkillUsage(&seeds[i]); err != nil {
			t.Fatalf("seed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if rec := crudDo(t, s, http.MethodGet, "/api/skills/BadName/usage", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name: status=%d", rec.Code)
	}
	rec := crudDo(t, s, http.MethodGet, "/api/skills/"+name+"/usage?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: status=%d", rec.Code)
	}
	var usage struct {
		Calls []struct {
			AgentKey string `json:"agent_key"`
			ArgsLen  int    `json:"args_len"`
		} `json:"calls"`
	}
	crudJSON(t, rec, &usage)
	if len(usage.Calls) != 3 {
		t.Fatalf("calls = %+v", usage.Calls)
	}
	// args_len 1 (mainagent) was inserted last → newest-first ordering.
	if usage.Calls[0].ArgsLen != 1 || usage.Calls[0].AgentKey != "mainagent" {
		t.Fatalf("newest call = %+v", usage.Calls[0])
	}
	if usage.Calls[2].ArgsLen != 42 || usage.Calls[2].AgentKey != "planner" {
		t.Fatalf("oldest call = %+v", usage.Calls[2])
	}

	rec = crudDo(t, s, http.MethodGet, "/api/skills/missing?limit=50", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("missing: status=%d", rec.Code)
	}
	var missing struct {
		Missing []struct {
			Skill string `json:"skill"`
			Calls int    `json:"calls"`
		} `json:"missing"`
	}
	crudJSON(t, rec, &missing)
	var hit *int
	for i := range missing.Missing {
		if missing.Missing[i].Skill == name+"-ghost" {
			hit = &i
		}
	}
	if hit == nil || missing.Missing[*hit].Calls != 1 {
		t.Fatalf("miss not in gap list: %+v", missing.Missing)
	}
}

// ---------- HTTP: delete + visibility ----------

func TestSkillDeleteAndVisibility(t *testing.T) {
	m, s := newTestStack(t)
	name := skillTestName("skilltest")
	if err := os.MkdirAll(filepath.Join(s.skillDir, name), 0o755); err != nil {
		t.Fatal(err)
	}

	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/BadName", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name: status=%d", rec.Code)
	}

	// agents.key uses ^[a-z][a-z0-9_]*$ (underscore, no hyphen) — the opposite of
	// the skill-name rule, so the suffix helper for agents differs.
	agent, err := m.pg.CreateAgent("agent_"+strconv.FormatInt(time.Now().UnixNano(), 10), "visibility tester", "fixture")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agentID := agent.ID

	// Visibility starts empty, toggle on makes the agent appear.
	rec := crudDo(t, s, http.MethodGet, "/api/visibility/skill/"+name, "")
	var vis struct {
		Agents []string `json:"agents"`
	}
	crudJSON(t, rec, &vis)
	if len(vis.Agents) != 0 {
		t.Fatalf("initial visibility = %v", vis.Agents)
	}
	body := `{"agent_id":"` + strconv.FormatInt(agentID, 10) + `","skill_name":"` + name + `","visible":true}`
	if rec := crudDo(t, s, http.MethodPost, "/api/visibility/skill/toggle", body); rec.Code != http.StatusOK {
		t.Fatalf("toggle on: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = crudDo(t, s, http.MethodGet, "/api/visibility/skill/"+name, "")
	crudJSON(t, rec, &vis)
	if len(vis.Agents) != 1 || vis.Agents[0] != strconv.FormatInt(agentID, 10) {
		t.Fatalf("visibility after toggle = %v", vis.Agents)
	}

	// Deleting the skill removes its visibility rows too.
	if rec := crudDo(t, s, http.MethodDelete, "/api/skills/"+name, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(s.skillDir, name)); !os.IsNotExist(err) {
		t.Fatalf("skill dir still on disk: %v", err)
	}
	agents, err := m.pg.SkillAgents(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("visibility rows survived delete: %v", agents)
	}
}

// ---------- HTTP: upload overwrite path ----------

// uploadSkillTo posts a zip to /api/skills/upload through the authed handler so the
// overwrite query param can be exercised (uploadZip in skill_upload_test.go posts
// to the handler directly and takes no query).
func uploadSkillTo(t *testing.T, s *Server, query, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/skills/upload"+query, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSkillUploadOverwrite(t *testing.T) {
	_, s := newTestStack(t)
	data := buildZip(t, zipFile{name: "owdemo/SKILL.md", body: "---\nname: owdemo\ndescription: v1\n---\n", method: 8})

	if rec := uploadSkillTo(t, s, "", "owdemo.zip", data); rec.Code != http.StatusCreated {
		t.Fatalf("first upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Same name again without ?overwrite=true is a 409.
	if rec := uploadSkillTo(t, s, "", "owdemo.zip", data); rec.Code != http.StatusConflict {
		t.Fatalf("re-upload: status=%d want 409", rec.Code)
	}
	// With overwrite the replacement lands (201, v2 content on disk).
	v2 := buildZip(t, zipFile{name: "owdemo/SKILL.md", body: "---\nname: owdemo\ndescription: v2\n---\n", method: 8})
	if rec := uploadSkillTo(t, s, "?overwrite=true", "owdemo.zip", v2); rec.Code != http.StatusCreated {
		t.Fatalf("overwrite: status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(s.skillDir, "owdemo", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "description: v2") {
		t.Fatalf("overwrite did not replace content:\n%s", raw)
	}
	_ = os.RemoveAll(filepath.Join(s.skillDir, "owdemo"))
}
