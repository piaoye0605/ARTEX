package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	active := ""
	if t := s.m.ActiveTask(); t != nil {
		active = t.ID
	}
	list := s.m.List()
	metrics, _ := s.m.PG().TaskListMetricsAll()
	archiveBlockers, _ := s.m.PG().TaskArchiveBlockers()
	dtos := make([]TaskDTO, 0, len(list))
	for _, t := range list {
		dto := taskDTO(t, s.resolvedTaskStatus(t))
		applyTaskArchiveBlocker(&dto, archiveBlockers)
		metric := metrics[t.ExpID]
		dto.Tokens = tokenTotalDTO(metric.Tokens)
		// prefer the live in-memory heartbeat (fresher) and fall back to the
		// persisted max activity time (survives restarts) for run-duration display.
		dto.LastActivity = metric.LastActivity
		if live := s.engine.LastActivity(t.ID); live > dto.LastActivity {
			dto.LastActivity = live
		}
		dto.GoalsTotal = metric.Goals.Total
		dto.GoalsMet = metric.Goals.Met
		dto.InFlight = metric.RunningIntents
		dto.Findings = FindingSeverityDTO{
			Critical: metric.Findings.Critical,
			High:     metric.Findings.High,
			Medium:   metric.Findings.Medium,
			Low:      metric.Findings.Low,
		}
		dtos = append(dtos, dto)
	}
	writeJSON(w, 200, map[string]any{"tasks": dtos, "active": active})
}

func (s *Server) resolvedTaskStatus(t *Task) string {
	if t == nil {
		return "created"
	}
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Queued:
		return "queued"
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case t.llmStateSnapshot().FailoverState == "chain_exhausted" && s.engine.Started(t.ID):
		// LLM readiness is an execution dependency, not lifecycle state. Keeping
		// an exhausted task running lets the user edit/reset its chain directly.
		return "running"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	default:
		return "created"
	}
}

func (s *Server) setActive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.m.SetActive(req.ID) {
		writeErr(w, 404, "task not found")
		return
	}
	// resume the engine for the opened task (idempotent — no-op if already running).
	// 排队中的任务:仅设为活跃可查看,不启动引擎(维持并发上限,由 reconcile 补位)。
	if t, ok := s.m.Task(req.ID); ok && !t.lifecycleSnapshot().Queued {
		s.engine.Run(s.ctx, t)
	}
	writeJSON(w, 200, map[string]any{"active": req.ID})
}

// control pauses/resumes a task's autonomous execution (planner + workers).
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" {
		writeErr(w, 400, "action must be pause|resume")
		return
	}
	result, err := s.applyTaskControl(t, req.Action)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// controlIntent pauses, resumes, or cancels one local worker intent. A running
// worker is always stopped before state mutation/cleanup, preventing tool output
// that arrives after the user action from recreating deleted blackboard records.
func (s *Server) controlIntent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除，无法控制意图")
		return
	}
	defer s.engine.decInflight(t.ID)

	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil || iid <= 0 {
		writeErr(w, 400, "bad intent id")
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"` // cancel(删除)时必填:删除原因
		Mode   string `json:"mode"`   // cancel 专用:soft(默认,假删除)| hard(真删除,级联移除独占子孙)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" && req.Action != "cancel" {
		writeErr(w, 400, "action must be pause|resume|cancel")
		return
	}

	result, err := s.applyIntentControl(r.Context(), t, iid, req.Action, req.Reason, req.Mode)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// rerunIntent 重跑一条没跑成功的意图(blocked/exhausted/stopped):把它置回 open,worker
// 会重新认领、从头再跑(已写回图谱的 fact/finding/asset 保留);若任务已终态/暂停则顺带复活。
// 用于「出错的 work 点击继续运行」——网络/LLM 抖动导致 blocked 后可一键重试。
func restoreRerunIntent(t *Task, before *db.Node) error {
	if t == nil || before == nil {
		return fmt.Errorf("missing intent rollback snapshot")
	}
	if before.State == "blocked" && before.BlockedReason != "" {
		return t.Store.SetIntentBlockedReason(before.ID, before.BlockedReason)
	}
	return t.Store.SetIntentState(before.ID, before.State)
}

func (s *Server) rerunIntent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad intent id")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除，无法重跑意图")
		return
	}
	defer s.engine.decInflight(t.ID)
	before, err := t.Store.GetNode(iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	reopened, err := t.Store.ReopenIntent(iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !reopened {
		writeErr(w, 409, "该意图不是可重跑状态(仅 blocked/exhausted/stopped 可重跑)")
		return
	}
	queued, err := s.admitTask(t, "resume")
	if err != nil {
		if rollbackErr := restoreRerunIntent(t, before); rollbackErr != nil {
			err = fmt.Errorf("%w; restore intent %d after admission failure: %v", err, iid, rollbackErr)
		}
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[task] #%s 意图 #%d 已重开(重跑)", t.ID, iid)
	writeJSON(w, 200, map[string]any{"id": t.ID, "reopened": iid, "queued": queued})
}

// rerunBlocked 批量重跑本任务全部 blocked 意图(适合一次网络/LLM 断连导致多条 blocked 后
// 一键全部重试),置回 open 并复活任务;返回重开的条数。
func (s *Server) rerunBlocked(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除，无法重跑意图")
		return
	}
	defer s.engine.decInflight(t.ID)
	intents, err := t.Store.ListByKind(db.KindIntent, 1000000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	before := make([]*db.Node, 0)
	for _, intent := range intents {
		if intent.State == "blocked" {
			copy := *intent
			before = append(before, &copy)
		}
	}
	n, err := t.Store.ReopenBlockedIntents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	queued := false
	if n > 0 {
		queued, err = s.admitTask(t, "resume")
		if err != nil {
			rollbackErrors := make([]string, 0)
			for _, intent := range before {
				if rollbackErr := restoreRerunIntent(t, intent); rollbackErr != nil {
					rollbackErrors = append(rollbackErrors, fmt.Sprintf("intent %d: %v", intent.ID, rollbackErr))
				}
			}
			if len(rollbackErrors) > 0 {
				err = fmt.Errorf("%w; restore blocked intents after admission failure: %s", err, strings.Join(rollbackErrors, "; "))
			}
			writeErr(w, 500, err.Error())
			return
		}
		log.Printf("[task] #%s 批量重开 %d 条 blocked 意图", t.ID, n)
	}
	writeJSON(w, 200, map[string]any{"id": t.ID, "reopened": n, "queued": queued})
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		req.Description = "未命名任务"
	}
	if len(req.LLMProfileIDs) == 0 && req.LLMProfileID != nil {
		req.LLMProfileIDs = []int64{*req.LLMProfileID}
	}
	if err := s.validateTaskProfileIDs(req.LLMProfileIDs); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.TimeoutSeconds < 0 {
		req.TimeoutSeconds = 0
	}
	if len(req.SourceTaskIDs) > db.MaxTaskSourceCount {
		writeErr(w, 400, fmt.Sprintf("关联任务最多选择 %d 个", db.MaxTaskSourceCount))
		return
	}
	sourceIDs := make([]int64, 0, len(req.SourceTaskIDs))
	seenSources := map[int64]bool{}
	for _, raw := range req.SourceTaskIDs {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 || seenSources[id] {
			writeErr(w, 400, "关联任务 id 无效或重复")
			return
		}
		if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
			writeErr(w, 400, fmt.Sprintf("关联任务 #%d 不存在", id))
			return
		}
		seenSources[id] = true
		sourceIDs = append(sourceIDs, id)
	}
	companyIDs, err := db.NormalizeTaskCompanyIDs(req.CompanyIDs)
	if err != nil {
		writeErr(w, 400, fmt.Sprintf("关联企业无效：最多选择 %d 个有效企业", db.MaxTaskCompanyCount))
		return
	}
	req.CompanyIDs = companyIDs
	interceptRules, err := buildTaskInterceptRules(req.InterceptRules)
	if err != nil {
		writeErr(w, 400, "任务级拦截规则无效："+err.Error())
		return
	}
	t, err := s.m.CreateTaskWithOptions(req.Description, req.Goal, db.TaskCreateOptions{
		Name: strings.TrimSpace(req.Name), CategoryID: req.CategoryID,
		SourceTaskIDs: sourceIDs, CompanyIDs: req.CompanyIDs, LLMProfileIDs: req.LLMProfileIDs,
		TimeoutSeconds: req.TimeoutSeconds, PlanHeartbeatSeconds: req.PlanHeartbeatSeconds,
		CoverageEnabled: req.CoverageEnabled,
		InterceptRules:  interceptRules,
	})
	if err != nil {
		if errors.Is(err, db.ErrTaskCategoryInvalid) || errors.Is(err, db.ErrTaskCategoryNotFound) {
			writeErr(w, 400, "任务分类不存在或无效")
			return
		}
		if errors.Is(err, db.ErrTaskCompanyIDsInvalid) || errors.Is(err, db.ErrTaskCompanyNotFound) {
			writeErr(w, 400, "关联企业不存在或无效")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[task] 新建任务 #%s «%s» 目标: %s", t.ID, req.Description, req.Goal)
	// 共享的建后流程(seed + 种子意图 + 后台目标分解 + engine.Run),与 spawn_task 复用同一段。
	// launchTask 内部异步,不阻塞 UI —— 目标分解在后台可见地进行。
	s.launchTask(t, req.Description+" "+req.Goal, req.SeedFirstIntent != nil && *req.SeedFirstIntent)
	writeJSON(w, 201, taskDTO(t, s.resolvedTaskStatus(t)))
}

func (s *Server) validateTaskProfileIDs(ids []int64) error {
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return fmt.Errorf("LLM 配置 id 无效或重复")
		}
		seen[id] = true
		if _, ok := s.loadProfileConfig(id); !ok {
			return fmt.Errorf("LLM 配置 #%d 不存在或未设置 API Key", id)
		}
	}
	return nil
}

func (s *Server) updateTaskLLMProfiles(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	// 任何生命周期状态(含终态)都可以改链:任务结束后主 Agent 对话仍走这条链,
	// 模型不可用时不换链就等于把已完成任务的交互一起锁死。
	before := t.llmStateSnapshot()
	var req struct {
		LLMProfileIDs      []int64 `json:"llm_profile_ids"`
		ActiveLLMProfileID *int64  `json:"active_llm_profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if err := s.validateTaskProfileIDs(req.LLMProfileIDs); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	active := int64(0)
	if req.ActiveLLMProfileID != nil {
		active = *req.ActiveLLMProfileID
	}
	reopened, err := s.m.ReplaceTaskLLMProfiles(t.ID, req.LLMProfileIDs, active)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Profile edits affect only subsequent LLM calls. Existing in-flight calls
	// retain their provider. Concurrency reconciliation treats ActiveLLMCalls as a
	// live slot and postpones an unavailable task's pause/queue transition until
	// the call returns; a nudge lets an idle running task resume promptly.
	t.Notify()
	go s.reconcileConcurrency()
	llmState := t.llmStateSnapshot()
	result := map[string]any{
		"id": t.ID, "llm_profile_ids": llmState.ProfileIDs,
		"active_llm_profile_id": llmState.ActiveID,
		"llm_failover_state":    llmState.FailoverState, "reopened_intents": reopened,
	}
	if !sameOptionalID(before.ActiveID, llmState.ActiveID) {
		event := s.emitManualTaskLLMSwitch(t, before.ActiveID, llmState.ActiveID)
		result["switch_event"] = activityDTO(event)
	}
	writeJSON(w, 200, result)
}

var (
	reURL    = regexp.MustCompile(`https?://[^\s'"]+`)
	reIPPort = regexp.MustCompile(`\b((?:\d{1,3}\.){3}\d{1,3})(?::(\d{1,5}))?`)
	reDomain = regexp.MustCompile(`\b((?:[a-zA-Z0-9-]+\.)+[a-zA-Z]{2,})(?::(\d{1,5}))?`)
)

// parseTarget extracts a target (scheme, host, port) from free text, supporting
// full URLs, IP[:port] and domain[:port]. ok=false when nothing parseable.
func parseTarget(text string) (scheme, host string, port int, ok bool) {
	text = strings.TrimSpace(text)
	if m := reURL.FindString(text); m != "" {
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			scheme = strings.ToLower(u.Scheme)
			host = strings.ToLower(u.Hostname())
			port = portOr(u.Port(), defaultPort(scheme))
			return scheme, host, port, true
		}
	}
	if m := reIPPort.FindStringSubmatch(text); m != nil {
		host = m[1]
		port = portOr(m[2], 80)
		return schemeForPort(port), host, port, true
	}
	if m := reDomain.FindStringSubmatch(text); m != nil {
		host = strings.ToLower(m[1])
		if m[2] == "" {
			return "https", host, 443, true
		}
		port = portOr(m[2], 443)
		return schemeForPort(port), host, port, true
	}
	return "", "", 0, false
}

func portOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return d
}

func defaultPort(scheme string) int {
	if scheme == "http" {
		return 80
	}
	return 443
}

func schemeForPort(p int) string {
	if p == 443 || p == 8443 {
		return "https"
	}
	return "http"
}

// llmHost returns the host of the configured LLM endpoint (to keep it out of scope).
func (s *Server) llmHost() string {
	s.cfgMu.Lock()
	base := s.llmCfg.BaseURL
	s.cfgMu.Unlock()
	if base == "" {
		return ""
	}
	if u, err := url.Parse(base); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

func (s *Server) seed(t *Task, text string) {
	scheme, host, port, ok := parseTarget(text)
	if !ok {
		log.Printf("[seed] task %s: 未能从 %q 解析出目标 host/IP，不创建站点（请手动配置 scope）", t.ID, text)
		return
	}
	// P0-1 guard: never treat the configured LLM gateway as a target.
	if gw := s.llmHost(); gw != "" && host == gw {
		log.Printf("[seed] task %s: 目标 %q 是 LLM 网关，拒绝作为渗透目标", t.ID, host)
		return
	}

	u := scheme + "://" + host
	if !(scheme == "https" && port == 443) && !(scheme == "http" && port == 80) {
		u += ":" + strconv.Itoa(port)
	}
	var rootID int64
	if as := s.m.Assets(); as != nil {
		taskID, _ := strconv.ParseInt(t.ID, 10, 64)
		if net.ParseIP(host) != nil {
			rootID, _ = as.UpsertIP(db.UpsertIPReq{IP: host, TaskID: taskID})
		} else if scheme == "https" || scheme == "http" {
			rootID, _ = as.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: u, TaskID: taskID})
		} else {
			rootID, _ = as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: host, TaskID: taskID})
		}
		if rootID > 0 {
			_ = as.SetTaskAssetSource(taskID, rootID, "task", "由任务描述或目标初始化", nil)
		}
	}
	// anchor the seeded assets to this task's begin root as lineage/provenance
	// (the asset graph is global and shared; anchoring no longer gates reads).
	if rootID > 0 {
		if begin, _ := t.Store.OriginFactID(); begin > 0 {
			_ = t.Store.Anchor(begin, rootID)
		}
	}
	log.Printf("[seed] task %s: 目标站点 %s", t.ID, u)
	// 不在这里 Notify:首轮是否触发统一由 engine.Run 的 HasActiveIntent 决定(种子意图任务
	// 跳过首轮)。seed 早于 Run 执行,若在此 Notify 会 buffered 到通道、被 plannerLoop 启动时
	// 消费掉而绕过 Run 的门控 → 种子任务仍误触发首轮。
}

// seedFirstIntent writes ONE open intent (summary = 描述+目标) into the task's
// frontier at creation, so a worker can claim and run it immediately without first
// waiting a planner round. Mirrors a planner top-level intent: it links from the
// origin fact (RelDerivedFrom) so it still traces back to a fact node. Best-effort —
// a failure just falls back to the normal planner-driven flow.
func (s *Server) seedFirstIntent(t *Task) {
	summary := fmt.Sprintf("完成任务目标：%s（任务：%s）", t.Goal, t.Description)
	id, err := t.Store.AddIntent(map[string]any{"summary": summary}, 8, nil, "seed")
	if err != nil {
		log.Printf("[seed] task %s: 下发种子意图失败: %v", t.ID, err)
		return
	}
	if origin, _ := t.Store.OriginFactID(); origin > 0 {
		_ = t.Store.Link(origin, db.RelDerivedFrom, id)
	}
	log.Printf("[seed] task %s: 已下发种子意图 #%d", t.ID, id)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	dto := taskDTO(t, s.resolvedTaskStatus(t))
	archiveBlockers, _ := s.m.PG().TaskArchiveBlockers()
	applyTaskArchiveBlocker(&dto, archiveBlockers)
	writeJSON(w, 200, dto)
}

// taskCoverage returns a task's rough asset test coverage (denominator/tested/backlog).
func (s *Server) taskCoverage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store 未启用")
		return
	}
	// 资产覆盖度功能关闭 → 短路返回 {enabled:false}，前端据此隐藏覆盖度卡片/进度。
	if !t.CoverageEnabled {
		writeJSON(w, 200, &db.Coverage{Enabled: false, ByType: []db.CoverageByType{}})
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	cov, err := as.TaskCoverageWithSources(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	cov.Enabled = true
	writeJSON(w, 200, cov)
}

// taskCoverageGraph returns the force-directed asset coverage graph for a task:
// all in-scope assets (每种类型) + 连接用的根域名/公司节点, each carrying tested/in_scope.
func (s *Server) taskCoverageGraph(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store 未启用")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	g, err := as.BuildCoverageGraph(taskID, t.ExpID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, g)
}

// taskAssetRefs returns the intents / facts / findings in this task anchored to a
// given asset id — powers the coverage-graph node drawer's「关联意图 / 关联事实」。
func (s *Server) taskAssetRefs(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	assetID, _ := strconv.ParseInt(r.URL.Query().Get("asset_id"), 10, 64)
	if assetID <= 0 {
		writeErr(w, 400, "需要 asset_id")
		return
	}
	refs, err := t.Store.AssetRefsWithSources(assetID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	intents := []CoverageAssetRefDTO{}
	facts := []CoverageAssetRefDTO{}
	findings := []CoverageAssetRefDTO{}
	for _, ref := range refs {
		dto := coverageAssetRefDTO(ref)
		switch ref.Kind {
		case "intent":
			intents = append(intents, dto)
		case "fact":
			facts = append(facts, dto)
		case "finding":
			findings = append(findings, dto)
		}
	}
	writeJSON(w, 200, map[string]any{"intents": intents, "facts": facts, "findings": findings})
}

// taskScopeList returns a task's scope rows (coverage denominator sources).
func (s *Server) taskScopeList(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store 未启用")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	rows, err := as.ListTaskScopeWithSources(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"scope": rows})
}

func (s *Server) taskScopeAdd(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store 未启用")
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Value  string `json:"value"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	ts, err := as.AddAgentScope(taskID, body.Kind, body.Value, body.Reason, "manual")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, ts)
}

func (s *Server) taskScopeDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "asset store 未启用")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	scopeID, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid scope id")
		return
	}
	deleted, err := as.DeleteTaskScope(taskID, scopeID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !deleted {
		writeErr(w, 404, "scope row not found")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) frontier(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	fr, _ := t.Store.Frontier(atoiDefault(r.URL.Query().Get("limit"), 100))
	writeJSON(w, 200, taskNodeDTOs(fr))
}
