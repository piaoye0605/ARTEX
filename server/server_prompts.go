package server

import (
	"bytes"
	"net/http"
	"text/template"
	"text/template/parse"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) pgSavePrompt(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct{ Template, Note string }
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	vars, _ := pg.PromptVars(a.ID)
	if bad := validateTemplate(body.Template, withGlobalVars(vars)); bad != "" {
		writeErr(w, 400, bad)
		return
	}
	ver, err := pg.SavePrompt(a.ID, body.Template, body.Note, "ui")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"version": ver})
}

// pgResetPrompt restores an agent's prompt body to the in-code built-in default
// (段 [A]). Only built-in agents have a code default; custom agents have none.
func (s *Server) pgResetPrompt(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	tmpl, has := agent.BuiltinPromptSeeds()[a.Key]
	if !has {
		writeErr(w, 400, "该 agent 无内置默认提示词，无法恢复")
		return
	}
	ver, err := pg.ResetPromptToDefault(a.ID, tmpl)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"version": ver})
}

// pgSaveWrapup stores an agent's wrap-up (settlement) prompt — the text injected on
// timeout / step-exhaustion. Empty body clears the override → built-in default.
func (s *Server) pgSaveWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	// MaxTurns is optional (pointer): omit to leave the stored turn budget untouched.
	var body struct {
		Prompt   string `json:"prompt"`
		MaxTurns *int   `json:"max_turns"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetAgentWrapupPrompt(a.Key, body.Prompt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if body.MaxTurns != nil {
		n := *body.MaxTurns
		if n < 0 {
			n = 0
		}
		if err := pg.SetAgentWrapupMaxTurns(a.Key, n); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgResetWrapup clears an agent's wrap-up prompt override so the code built-in
// default is used again.
func (s *Server) pgResetWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if err := pg.SetAgentWrapupPrompt(a.Key, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := pg.SetAgentWrapupMaxTurns(a.Key, 0); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":                       true,
		"wrapup_default":           agent.WrapupDefault(a.Key),
		"wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key),
	})
}

// pgSaveTaskTimeoutWrapup stores an agent's TASK-TIMEOUT wrap-up prompt + turn budget
// (worker/planner only). Empty prompt / 0 turns clear the override → built-in default.
func (s *Server) pgSaveTaskTimeoutWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Prompt   string `json:"prompt"`
		MaxTurns *int   `json:"max_turns"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	turns := a.TaskTimeoutWrapupMaxTurns // 未传则保留原值
	if body.MaxTurns != nil {
		turns = *body.MaxTurns
		if turns < 0 {
			turns = 0
		}
	}
	if err := pg.SetAgentTaskTimeoutWrapup(a.Key, body.Prompt, turns); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgResetTaskTimeoutWrapup clears the task-timeout wrap-up override → built-in default.
func (s *Server) pgResetTaskTimeoutWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if err := pg.SetAgentTaskTimeoutWrapup(a.Key, "", 0); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":                                    true,
		"task_timeout_wrapup_default":           agent.TaskTimeoutWrapupDefault(a.Key),
		"task_timeout_wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key),
	})
}

func (s *Server) pgListPromptVersions(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	vers, err := pg.ListPromptVersions(a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"versions": vers})
}

func (s *Server) pgPromptVars(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	vars, err := pg.PromptVars(a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"variables": withGlobalVars(vars)})
}

func (s *Server) pgPreviewPrompt(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Template string            `json:"template"`
		Sample   map[string]string `json:"sample"`
	}
	_ = decode(r, &body)
	vars, _ := pg.PromptVars(a.ID)
	if body.Template == "" {
		body.Template, _ = pg.CurrentPrompt(a.ID)
	}
	rendered, err := renderPrompt(body.Template, withGlobalVars(vars), body.Sample)
	if err != nil {
		writeJSON(w, 200, map[string]any{"rendered": "", "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"rendered": rendered})
}

// globalPromptVars are runtime variables available to EVERY agent (built-in and
// custom) regardless of its per-agent catalog. Each agent's render path fills them
// (see agent.nowStr, rendered fresh each turn), so a prompt may always reference
// {{.Now}} — e.g. subtract it from a fixed start stamp to reason about elapsed time.
var globalPromptVars = []db.PromptVar{
	{Name: "Now", Description: "服务端当前时间（每次运行实时刷新；可与固定起始时间相减判断已用时长）", Example: "2026-08-11 14:30:00 CST", Source: "runtime"},
	{Name: "DataDir", Description: "服务端数据根目录（所有任务/会话产物的根；各 agent 实际写盘在其下的子目录，如 <DataDir>/<taskID>）", Example: "/app/data", Source: "runtime"},
}

// withGlobalVars appends the universal runtime vars onto an agent's own catalog,
// so validation / the UI variable list / preview all recognize {{.Now}} etc.
// A stored catalog entry that collides with a global name is dropped: the global
// runtime var is authoritative (it's what the render path actually resolves), and
// this keeps the returned list name-unique so the UI never sees duplicate keys.
func withGlobalVars(vars []db.PromptVar) []db.PromptVar {
	globalNames := make(map[string]bool, len(globalPromptVars))
	for _, g := range globalPromptVars {
		globalNames[g.Name] = true
	}
	out := make([]db.PromptVar, 0, len(vars)+len(globalPromptVars))
	for _, v := range vars {
		if globalNames[v.Name] {
			continue // shadowed by the authoritative global runtime var
		}
		out = append(out, v)
	}
	return append(out, globalPromptVars...)
}

// validateTemplate parses the template and rejects any {{.Var}} not in the catalog.
// Returns "" if valid, otherwise an error message.
func validateTemplate(tmpl string, catalog []db.PromptVar) string {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "模板语法错误: " + err.Error()
	}
	allowed := map[string]bool{}
	for _, v := range catalog {
		allowed[v.Name] = true
	}
	for _, name := range templateFields(t) {
		if !allowed[name] {
			return "变量 {{." + name + "}} 不在该 agent 允许列表"
		}
	}
	return ""
}

// renderPrompt renders with example values (catalog example overridden by sample).
func renderPrompt(tmpl string, catalog []db.PromptVar, sample map[string]string) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	data := map[string]any{}
	for _, v := range catalog {
		data[v.Name] = v.Example
	}
	for k, val := range sample {
		data[k] = val
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// templateFields returns the distinct top-level {{.X}} field names referenced by
// the template (used to validate against the catalog whitelist).
func templateFields(t *template.Template) []string {
	seen := map[string]bool{}
	var out []string
	collect := func(p *parse.PipeNode) {
		if p == nil {
			return
		}
		for _, cmd := range p.Cmds {
			for _, arg := range cmd.Args {
				if f, ok := arg.(*parse.FieldNode); ok && len(f.Ident) > 0 && !seen[f.Ident[0]] {
					seen[f.Ident[0]] = true
					out = append(out, f.Ident[0])
				}
			}
		}
	}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch x := n.(type) {
		case *parse.ListNode:
			if x == nil {
				return
			}
			for _, c := range x.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			collect(x.Pipe)
		case *parse.IfNode:
			collect(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.RangeNode:
			collect(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.WithNode:
			collect(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		}
	}
	if t.Tree != nil {
		walk(t.Tree.Root)
	}
	return out
}
