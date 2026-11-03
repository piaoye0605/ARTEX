package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) pgListTools(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ts, err := pg.ListTools()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ts == nil {
		ts = []*db.Tool{}
	}
	// Usage is best-effort decoration. Runtime tool resolution keeps using the plain
	// catalog query, so agent assembly never pays for this aggregate.
	counts, countErr := pg.ToolUsageCounts()
	if countErr != nil {
		log.Printf("[tools] 读取调用统计失败: %v", countErr)
	} else {
		for _, tool := range ts {
			tool.Calls = counts[tool.Key]
		}
	}
	writeJSON(w, 200, map[string]any{"tools": ts})
}

// pgUpdateTool saves the page-editable fields of a built-in tool: description,
// parameter schema (structure is expected unchanged — only per-param description/
// default move), agent binding, and enabled. key is taken from the path and never
// changes (it is welded to the Go handler).
func (s *Server) pgUpdateTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	cur, err := pg.GetTool(key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if cur == nil {
		writeErr(w, 404, "工具不存在: "+key)
		return
	}
	var body struct {
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Agents      []string        `json:"agents"`
		Enabled     bool            `json:"enabled"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	agents, _ := json.Marshal(body.Agents)
	if err := pg.UpdateTool(key, body.Description, body.Schema, agents, body.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgResetTool overwrites a tool row with its code-defined defaults (description,
// schema, agent binding) and re-enables it — the explicit "恢复默认" action, since
// startup seeding is first-insert-only and never overwrites edits.
func (s *Server) pgResetTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	for _, sd := range agent.BuiltinToolSeeds() {
		if sd.Key != key {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		agents, _ := json.Marshal(sd.Agents)
		if err := pg.UpsertToolForce(sd.Key, sd.Desc, schema, agents); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// orchestration/platform tools (auto agent) — default-bind to "auto".
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range append(s.orchestrationTools(), s.platformTools()...) {
		if t.Name() != key {
			continue
		}
		schema, _ := json.Marshal(t.InputSchema())
		if err := pg.UpsertToolForce(t.Name(), t.Description(), schema, autoAgents); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeErr(w, 404, "非内置工具或不存在: "+key)
}

// ---------- mcp ----------

func (s *Server) pgListMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ms, err := pg.ListMCP()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ms == nil {
		ms = []*db.MCPServer{}
	}
	writeJSON(w, 200, map[string]any{"servers": ms})
}

func (s *Server) pgSaveMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var m db.MCPServer
	if err := decode(r, &m); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	isNew := m.ID == 0
	id, err := pg.SaveMCP(&m)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// On initial add, auto-discover + cache the tool list so the UI shows it right
	// away (bounded so a slow/broken server can't hang the request). Skipped on
	// plain updates (e.g. enable toggles) to avoid re-spawning the server each time.
	if isNew && m.Enabled {
		m.ID = id
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		if derr := s.discoverAndCacheMCP(ctx, &m); derr != nil {
			log.Printf("[mcp] %s 添加后工具发现失败: %v", m.Name, derr)
		}
		cancel()
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) pgDeleteMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	if err := pg.DeleteMCP(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// pgRefreshMCP re-discovers one MCP's tools on demand and re-caches them, so a
// config change or an earlier discovery failure can be fixed without a restart.
func (s *Server) pgRefreshMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	all, err := pg.ListMCP()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var target *db.MCPServer
	for _, m := range all {
		if m.ID == id {
			target = m
			break
		}
	}
	if target == nil {
		writeErr(w, 404, "MCP 不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.discoverAndCacheMCP(ctx, target); err != nil {
		writeErr(w, 502, "工具发现失败："+err.Error())
		return
	}
	tools, _ := pg.MCPToolsDetailed(id)
	writeJSON(w, 200, map[string]any{"tools": tools})
}

// pgMCPTools returns one MCP's cached tools (name + description) for the detail UI.
func (s *Server) pgMCPTools(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	tools, err := pg.MCPToolsDetailed(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tools == nil {
		tools = []db.MCPTool{}
	}
	writeJSON(w, 200, map[string]any{"tools": tools})
}

// ---------- skills (文件系统) ----------
