package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// callPlatform runs a platform tool and returns (text, isError).
func callPlatform(t *testing.T, tool actool.CoreTool, in map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("tool call %s: unexpected error %v", tool.Name(), err)
	}
	return res.Flatten(), res.IsError
}

// TestDeleteAssetsByHostTool covers the host-targeted hard delete tool: empty
// host guard, root-domain deletion (root + subdomain + child service), ip
// deletion, subdomain-scoped deletion, and the deleted_by_type aggregation.
func TestDeleteAssetsByHostTool(t *testing.T) {
	_, s := newTestStack(t)
	pg := s.m.pg
	assets := pg.Assets()
	tag := fmt.Sprintf("h34-%d", time.Now().UnixNano())
	root := tag + ".example"
	sub := "a." + root
	ip := "203.0.113.77"

	tool := s.toolDeleteAssetsByHost()

	// empty / blank host rejected.
	if text, isErr := callPlatform(t, tool, map[string]any{"host": "  "}); !isErr || !strings.Contains(text, "host 不能为空") {
		t.Fatalf("blank host = %q (err=%v)", text, isErr)
	}

	rootID, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: root})
	if err != nil {
		t.Fatal(err)
	}
	subID, err := assets.UpsertSubdomain(db.UpsertSubdomainReq{Domain: sub, TaskID: 0})
	if err != nil {
		t.Fatal(err)
	}
	ipID, err := assets.UpsertIP(db.UpsertIPReq{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, h := range []string{root, sub, ip, tag + "-left.example"} {
			_, _ = assets.DeleteByHost(h)
		}
	})

	// an HTTP service hung under the subdomain (host auto-extracted from URL).
	if _, err := assets.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: "http://" + sub + ":8080/"}); err != nil {
		t.Fatal(err)
	}

	// deleting the root removes root + subdomain + service in one shot.
	text, isErr := callPlatform(t, tool, map[string]any{"host": root})
	if isErr {
		t.Fatalf("delete root: %q", text)
	}
	if !strings.Contains(text, `"host":"`+root+`"`) || !strings.Contains(text, `"deleted":3`) {
		t.Fatalf("delete root output = %s", text)
	}
	for _, id := range []int64{rootID, subID} {
		if existsAssetRow(t, pg, id) {
			t.Fatalf("asset %d survived root deletion", id)
		}
	}

	// deleting a subdomain (fresh row) removes only that host.
	if _, err := assets.UpsertSubdomain(db.UpsertSubdomainReq{Domain: sub}); err != nil {
		t.Fatal(err)
	}
	text, isErr = callPlatform(t, tool, map[string]any{"host": strings.ToUpper(sub) + "."})
	if isErr {
		t.Fatalf("delete subdomain: %q", text)
	}
	if !strings.Contains(text, `"deleted":1`) {
		t.Fatalf("delete subdomain output = %s (DomainKey normalization expected)", text)
	}

	// deleting an IP asset.
	text, isErr = callPlatform(t, tool, map[string]any{"host": ip})
	if isErr || !strings.Contains(text, `"deleted":1`) {
		t.Fatalf("delete ip = %q (err=%v)", text, isErr)
	}
	if existsAssetRow(t, pg, ipID) {
		t.Fatal("ip asset survived deletion")
	}

	// unknown host → success with deleted=0 and empty map.
	text, isErr = callPlatform(t, tool, map[string]any{"host": tag + "-left.example"})
	if isErr || !strings.Contains(text, `"deleted":0`) {
		t.Fatalf("delete unknown host = %q (err=%v)", text, isErr)
	}
}

// existsAssetRow reports whether an assets row with the given id still exists.
func existsAssetRow(t *testing.T, pg *db.DB, id int64) bool {
	t.Helper()
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM assets WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestPlatformSkillToolBranches covers the remaining guard branches of
// create_skill / update_skill_file (success paths live in platform_tools_test.go).
func TestPlatformSkillToolBranches(t *testing.T) {
	dir := t.TempDir()
	s := &Server{skillDir: dir}

	// create_skill: description required; missing instructions fall back to a stub body.
	if text, isErr := callPlatform(t, s.toolCreateSkill(), map[string]any{"name": "no-desc"}); !isErr || !strings.Contains(text, "description 必填") {
		t.Fatalf("missing description = %q (err=%v)", text, isErr)
	}
	if text, isErr := callPlatform(t, s.toolCreateSkill(), map[string]any{"name": "stub-body", "description": "d"}); isErr {
		t.Fatalf("stub body create = %q", text)
	}
	md, err := os.ReadFile(filepath.Join(dir, "stub-body", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "## stub-body") || !strings.Contains(string(md), "3. ") {
		t.Fatalf("stub body = %s", md)
	}

	// update_skill_file: invalid name, missing skill, and the default SKILL.md path.
	if text, isErr := callPlatform(t, s.toolUpdateSkillFile(), map[string]any{"name": "Bad", "content": "x"}); !isErr || !strings.Contains(text, "skill 名不合法") {
		t.Fatalf("invalid name = %q (err=%v)", text, isErr)
	}
	if text, isErr := callPlatform(t, s.toolUpdateSkillFile(), map[string]any{"name": "ghost-skill", "content": "x"}); !isErr || !strings.Contains(text, "skill 不存在") {
		t.Fatalf("missing skill = %q (err=%v)", text, isErr)
	}
	// empty file → SKILL.md overwritten.
	if text, isErr := callPlatform(t, s.toolUpdateSkillFile(), map[string]any{
		"name": "stub-body", "file": "  ", "content": "# rewritten",
	}); isErr {
		t.Fatalf("default file write = %q", text)
	}
	md, err = os.ReadFile(filepath.Join(dir, "stub-body", "SKILL.md"))
	if err != nil || string(md) != "# rewritten" {
		t.Fatalf("SKILL.md overwrite: err=%v content=%q", err, md)
	}
}

// TestCustomToolPlatformTools covers create_custom_tool / update_custom_tool
// guard branches and the DB round-trip.
func TestCustomToolPlatformTools(t *testing.T) {
	_, s := newTestStack(t)
	pg := s.m.pg
	key := fmt.Sprintf("h34_tool_%d", time.Now().UnixNano()%1_000_000)
	sysKey := key + "_sys"
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM tools WHERE key IN ($1, $2)`, key, sysKey)
	})

	create := s.toolCreateCustomTool()
	update := s.toolUpdateCustomTool()

	// create: key / kind / http-schema guards.
	if text, isErr := callPlatform(t, create, map[string]any{"key": "Bad-Key", "kind": "shell"}); !isErr || !strings.Contains(text, "key 需小写字母开头") {
		t.Fatalf("bad key = %q (err=%v)", text, isErr)
	}
	if text, isErr := callPlatform(t, create, map[string]any{"key": key, "kind": "exec"}); !isErr || !strings.Contains(text, "kind 需为") {
		t.Fatalf("bad kind = %q (err=%v)", text, isErr)
	}
	if text, isErr := callPlatform(t, create, map[string]any{"key": key, "kind": "http"}); !isErr || !strings.Contains(text, "http 工具必须提供参数 JSON Schema") {
		t.Fatalf("http without schema = %q (err=%v)", text, isErr)
	}

	// create: success with enabled=false + agents binding.
	enabled := false
	if text, isErr := callPlatform(t, create, map[string]any{
		"key": key, "kind": "command", "description": "batch34 tool",
		"exec": map[string]any{"command": "echo hi"}, "agents": []string{"auto"},
		"enabled": enabled, "deferred": true,
	}); isErr || !strings.Contains(text, "custom tool created: "+key) {
		t.Fatalf("create = %q (err=%v)", text, isErr)
	}
	row, err := pg.GetTool(key)
	if err != nil || row == nil {
		t.Fatalf("GetTool after create: err=%v row=%v", err, row)
	}
	if row.System || row.Kind != "command" || row.Enabled || !row.Deferred || len(row.Agents) != 1 || row.Agents[0] != "auto" {
		t.Fatalf("created row = %+v", row)
	}

	// create: duplicate key rejected.
	if text, isErr := callPlatform(t, create, map[string]any{"key": key, "kind": "shell"}); !isErr || !strings.Contains(text, "该 key 已存在") {
		t.Fatalf("duplicate key = %q (err=%v)", text, isErr)
	}

	// update: unknown key and system tools rejected.
	if text, isErr := callPlatform(t, update, map[string]any{"key": key + "_missing", "kind": "shell"}); !isErr || !strings.Contains(text, "只能修改自定义工具") {
		t.Fatalf("unknown key = %q (err=%v)", text, isErr)
	}
	if err := pg.SeedTool(sysKey, "system seeded", nil, nil); err != nil {
		t.Fatal(err)
	}
	if text, isErr := callPlatform(t, update, map[string]any{"key": sysKey, "kind": "shell"}); !isErr || !strings.Contains(text, "只能修改自定义工具") {
		t.Fatalf("system tool = %q (err=%v)", text, isErr)
	}
	if text, isErr := callPlatform(t, update, map[string]any{"key": key, "kind": "grpc"}); !isErr || !strings.Contains(text, "kind 需为") {
		t.Fatalf("update bad kind = %q (err=%v)", text, isErr)
	}
	if text, isErr := callPlatform(t, update, map[string]any{"key": key, "kind": "http"}); !isErr || !strings.Contains(text, "http 工具必须提供参数 JSON Schema") {
		t.Fatalf("update http without schema = %q (err=%v)", text, isErr)
	}

	// update: kind conversion to http with schema, re-enabled, exec replaced.
	yes := true
	if text, isErr := callPlatform(t, update, map[string]any{
		"key": key, "kind": "http", "description": "updated desc", "enabled": yes,
		"schema":   map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}},
		"exec":     map[string]any{"method": "GET", "url": "http://example.test"},
		"deferred": false,
	}); isErr || !strings.Contains(text, "custom tool updated: "+key) {
		t.Fatalf("update = %q (err=%v)", text, isErr)
	}
	row, err = pg.GetTool(key)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.Kind != "http" || !row.Enabled || row.Deferred || row.Description != "updated desc" || !hasSchemaProps(row.Schema) {
		t.Fatalf("updated row = %+v schema=%s", row, row.Schema)
	}
}

// TestMCPPlatformTools covers create_mcp / update_mcp guard branches and the
// DB round-trip including args/env and enabled/insecure pointer handling.
func TestMCPPlatformTools(t *testing.T) {
	_, s := newTestStack(t)
	pg := s.m.pg

	create := s.toolCreateMCP()
	update := s.toolUpdateMCP()

	if text, isErr := callPlatform(t, create, map[string]any{"name": "", "transport": ""}); !isErr || !strings.Contains(text, "name / transport 必填") {
		t.Fatalf("missing name/transport = %q (err=%v)", text, isErr)
	}

	// create: stdio server with args/env; a provided id must be forced to 0.
	no := false
	text, isErr := callPlatform(t, create, map[string]any{
		"id": 999, "name": "h34-stdio", "transport": "stdio",
		"command": "npx", "args": []string{"-y", "some-server"},
		"env": map[string]any{"TOKEN": "x"}, "enabled": no,
	})
	if isErr || !strings.Contains(text, "mcp created: id=") || !strings.Contains(text, "name=h34-stdio") {
		t.Fatalf("create mcp = %q (err=%v)", text, isErr)
	}
	var id int64
	if err := pg.QueryRow(`SELECT id FROM mcp_servers WHERE name='h34-stdio'`).Scan(&id); err != nil {
		t.Fatalf("created mcp not found: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM mcp_servers WHERE name LIKE 'h34-%'`) })
	var got struct {
		Transport string
		Command   string
		Args      string
		Env       string
		Enabled   bool
	}
	if err := pg.QueryRow(`SELECT transport, COALESCE(command,''), args::text, env::text, enabled
FROM mcp_servers WHERE id=$1`, id).Scan(&got.Transport, &got.Command, &got.Args, &got.Env, &got.Enabled); err != nil {
		t.Fatal(err)
	}
	if got.Transport != "stdio" || got.Command != "npx" || got.Enabled {
		t.Fatalf("mcp row = %+v", got)
	}
	if got.Args != `["-y", "some-server"]` || got.Env != `{"TOKEN": "x"}` {
		t.Fatalf("args/env = %s / %s", got.Args, got.Env)
	}

	// create: http transport without command/url columns colliding — minimal http entry.
	if text, isErr := callPlatform(t, create, map[string]any{"name": "h34-http", "transport": "http", "url": "http://127.0.0.1:1/mcp"}); isErr {
		t.Fatalf("create http mcp = %q", text)
	}

	// update: id required; then flip enabled/insecure and rewrite URL.
	if text, isErr := callPlatform(t, update, map[string]any{"name": "x", "transport": "http"}); !isErr || !strings.Contains(text, "id 必填") {
		t.Fatalf("missing id = %q (err=%v)", text, isErr)
	}
	yes := true
	if text, isErr := callPlatform(t, update, map[string]any{
		"id": id, "name": "h34-stdio", "transport": "stdio", "command": "node",
		"enabled": yes, "insecure": yes,
	}); isErr || !strings.Contains(text, fmt.Sprintf("mcp updated: id=%d", id)) {
		t.Fatalf("update mcp = %q (err=%v)", text, isErr)
	}
	var enabled, insecure bool
	var cmd string
	if err := pg.QueryRow(`SELECT enabled, insecure, COALESCE(command,'') FROM mcp_servers WHERE id=$1`, id).
		Scan(&enabled, &insecure, &cmd); err != nil {
		t.Fatal(err)
	}
	if !enabled || !insecure || cmd != "node" { // both pointers were true, row must reflect the input
		t.Fatalf("after update: enabled=%v insecure=%v cmd=%q", enabled, insecure, cmd)
	}
}
