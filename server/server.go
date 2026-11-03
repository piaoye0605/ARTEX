package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
)

// BuildVersion is the backend application version, injected from cmd/artex at
// startup (which in turn gets it from -ldflags "-X main.version=<tag>").
// Defaults to "dev" for local builds. Exposed to the frontend via GET /api/health.
var BuildVersion = "dev"

// Server exposes the ARTEX backend over a JSON HTTP API for the shadcn/ui
// frontend.
type Server struct {
	m      *Manager
	engine *Engine
	ctx    context.Context

	skillDir string // root directory for skill subdirectories
	jwtKey   []byte // HS256 signing key loaded from / generated into dataDir/jwt.key

	// concMu serializes concurrency-cap decisions (admission + reconcile) so a
	// scheduler tick and an HTTP settings change / task creation can't both count
	// free slots off the same snapshot and over-promote past the limit.
	concMu sync.Mutex

	cfgMu     sync.Mutex
	chatAgent *agent.ChatAgent // conversational runner for the chat page; nil w/o LLM
	// llmProv is the fully-decorated global provider (recorder + failover chain)
	// installed by applyLLM. Task routers use it after an explicit chain is cleared.
	llmProv   llm.Provider
	llmDirect llm.Provider // concrete global provider, before any failover pool
	llmCfg    agent.Config // current LLM config (key not exposed)
	llmOn     bool
	llmProf   string // active LLM profile name (for llmrec tagging)

	// chatBusy guards the per-task main-agent run: the chat handler launches the
	// agent on the server's background ctx (not the request ctx) and returns
	// immediately, so a page reload / proxy timeout can't abort a live run. This
	// map serializes turns per task — one main-agent run at a time per task, so
	// concurrent messages don't corrupt the shared exp<id>-main transcript.
	chatMu   sync.Mutex
	chatBusy map[string]bool
	// chatCancel holds the cancel func for each in-flight conversation run (keyed by
	// convBusyKey), so a manual stop can abort JUST that session's agent run. Set/
	// cleared alongside chatBusy under chatMu. Aborting a run does NOT touch the P3
	// trigger queue — the drain goroutine simply proceeds to the next queued fire.
	chatCancel map[string]context.CancelCauseFunc

	// triggerQ buffers P3 trigger fires PER AGENT. A per-agent "pump" launches runs up
	// to a concurrency limit derived from the agent's策略: serial → limit 1 (+ optional
	// merge); parallel → limit = trigger_max_parallel (0=∞), no merge. triggerActive
	// counts in-flight runs per agent (replaces a boolean drain flag); a run's
	// completion decrements it and re-pumps to fill the freed slot. triggerCfg caches
	// the agent's last-read策略 so the pump never queries the DB while holding queueMu.
	// Distinct agents always run concurrently. Queue is in-memory (matches chatBusy); a
	// restart drops pending fires — the scheduler re-fires from watermarks next tick.
	queueMu       sync.Mutex
	triggerQ      map[string][]triggeredRun
	triggerActive map[string]int
	triggerCfg    map[string]triggerBehavior

	// profChatAgents caches a per-profile ChatAgent (chat page), keyed by profile id.
	// Built lazily on first use; invalidated when any profile is saved/activated/deleted
	// so edits take effect.
	profMu         sync.Mutex
	profChatAgents map[int64]*agent.ChatAgent // per-profile ChatAgent cache (chat page)

	// provByProfile caches ONE provider per LLM profile id so every agent bound or
	// pinned to the same profile shares a single provider instance — hence one rate
	// limiter. applyLLM also resolves the persisted global-active profile through this
	// cache, so global fallback and task chains do not accidentally double the
	// configured request rate. Cleared on profile edits, then repopulated by active reapply.
	provCacheMu   sync.Mutex
	provByProfile map[int64]*provEntry
	provCacheGen  uint64

	// llmHealth is the process-wide circuit-breaker state for LLM failover (轮询).
	// It deliberately lives OUTSIDE the provider caches: rebuilding the chain
	// (saving an unrelated profile, flipping a setting) must not erase what we
	// learned about which backends are out of credit / rate-limited.
	llmHealth *llmpool.Registry

	// Explicit task chains use one task-scoped dynamic provider shared by goals,
	// planner, workers, and the main agent. Bundles are stable; their runtime reads
	// the persisted current profile at every new LLM call.
	taskAgentMu sync.Mutex
	taskAgents  map[string]*taskAgentBundle

	// Cold task archives run through one persistent FIFO worker. The buffered wake
	// channel coalesces enqueue bursts; the database remains the source of truth.
	archiveWake chan struct{}
	archiveWG   sync.WaitGroup
	side        *sideQuestionState
}

// provEntry is a cached provider + its config for one LLM profile id.
type provEntry struct {
	prov llm.Provider
	cfg  agent.Config
}

// triggeredRun is one queued P3 trigger fire awaiting its turn for an agent.
// taskID + mergeable let the drainer coalesce several event triggers (finding/goal)
// from the SAME task into one conversation before it starts (interval fires don't merge).
type triggeredRun struct {
	agentKey  string
	title     string
	message   string // 事件正文(触发语 + 工具/入参/返回等);不含任务描述/目标头
	taskID    int64  // source task for finding/goal triggers; 0 for interval/none
	taskDesc  string // 任务描述(任务级,同任务相同);合并时只渲染一次
	taskGoal  string // 任务目标(任务级,同任务相同);合并时只渲染一次
	mergeable bool   // true for finding/goal event triggers (merge by taskID)
}

// WaitIdle waits for the engine's in-flight task operations to drain (process
// shutdown path — call between httpSrv.Shutdown and Manager.Close). See
// Engine.WaitIdle for semantics; once called the engine rejects new operations.
func (s *Server) WaitIdle(ctx context.Context) error {
	return s.engine.WaitIdle(ctx)
}

func New(ctx context.Context, m *Manager, skillDir string, dataDir string, keyDir string) *Server {
	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil {
		log.Fatalf("[auth] JWT key: %v", err)
	}
	s := &Server{m: m, engine: NewEngine(m), ctx: ctx, skillDir: skillDir, jwtKey: key, chatBusy: map[string]bool{},
		chatCancel: map[string]context.CancelCauseFunc{}, triggerQ: map[string][]triggeredRun{},
		triggerActive: map[string]int{}, triggerCfg: map[string]triggerBehavior{},
		profChatAgents: map[int64]*agent.ChatAgent{},
		provByProfile:  map[int64]*provEntry{}, llmHealth: newLLMHealthRegistry(m.pg),
		taskAgents: map[string]*taskAgentBundle{}, archiveWake: make(chan struct{}, 1)}
	s.initSideQuestions()
	// 熔断阈值/冷却是失败路径上的热参数，启动时把全局重试策略推给 Registry 一次；
	// 之后每次保存策略再推一次（saveLLMRetryPolicy）。
	s.applyRetryPolicy()
	// Every task uses a stable task router. An empty explicit chain is resolved by
	// that router through Agent bindings and then the global provider, so adding a
	// first chain to a running task takes effect on its very next LLM call.
	s.engine.SetAuthoritativeAgentResolver(func(t *Task) (*agent.Planner, *agent.Worker) {
		if !s.taskRuntimeAvailable(t, "planner", "worker") {
			return nil, nil
		}
		b := s.agentsForTask(t)
		return b.pl, b.wk
	})
	// Global readiness (Ready()/llm_configured): a global active LLM provider is
	// installed. Task-level runnability is separate (ReadyFor → the resolver above).
	s.engine.SetReadiness(func() bool {
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		return s.llmOn
	})
	// Wire DB-stored prompt templates into the agents (新版方案 §3.3 / §5a). With no
	// override row, agents keep their built-in defaults — behavior is unchanged.
	if m.pg != nil {
		agent.PromptOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil {
				return "", false
			}
			t, err := m.pg.CurrentPrompt(a.ID)
			if err != nil || t == "" {
				return "", false
			}
			return t, true
		}
		// Wire DB-stored wrap-up (settlement) prompts. Empty column → built-in default.
		agent.WrapupOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupPrompt == "" {
				return "", false
			}
			return a.WrapupPrompt, true
		}
		// Wire DB-stored wrap-up turn budgets. 0 / missing → built-in per-agent default.
		agent.WrapupMaxTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.WrapupMaxTurns, true
		}
		// Wire DB-stored TASK-TIMEOUT wrap-up prompt/turns (worker/planner). Empty/0 → default.
		agent.WrapupTaskTimeoutOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupPrompt == "" {
				return "", false
			}
			return a.TaskTimeoutWrapupPrompt, true
		}
		agent.WrapupTaskTimeoutTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.TaskTimeoutWrapupMaxTurns, true
		}
		wireAgentAugment(m.pg, s.skillDir, s.hostTools) // 可见 skills/MCP + 流量/编排 host 工具装配进 agent 工具集
		domainReg := buildDomainReg(m.Assets())
		wireTools(m.pg, domainReg) // 内置工具表：按 agent 过滤 + 覆盖描述/schema + 注入默认值
		seedPrompts(m.pg)          // 内置 agent 默认提示词正文播种进 agent_prompts(仅空时)
		s.seedOrchestrationTools() // P2 跨任务编排工具 seed 进 tools 表(可按 agent 绑定)
		if err := s.seedFindingRetester(); err != nil {
			log.Printf("[retester] seed: %v", err)
		}
		go s.evidenceStore().RunGC(s.ctx)
		s.seedPythonInterpreter()     // 自定义脚本工具:开机检测 python 解释器入库(仅空时)
		go newScheduler(s).Run(s.ctx) // P3 触发器调度(定时/finding/目标事件),仅自定义 agent
		// 漏洞 IM 推送投递引擎。与 Scheduler 并列但独立：推送的实时性要求(3s)
		// 与触发器的业务节奏不同，且两者失败互不牵连——推送卡住不该影响 agent 触发。
		go newNotifier(s).Run(s.ctx)
		// Fill the tool cache for any enabled MCP that has none yet (notably the
		// seeded browser MCP on first run). Async so it never blocks startup.
		go s.discoverEmptyMCPsOnStartup()
		logSink.SetDB(ctx, m.pg) // restore last 100 log rows and enable async persistence
	}
	// precedence: persisted DB config > env.
	if cfg, ok := s.loadLLMConfig(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] saved LLM config init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from DB: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else if cfg, ok := agent.FromEnv(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] env provider init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from env: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else {
		log.Printf("[engine] no LLM provider configured — engine idle until set via /api/llm or env")
	}
	s.restoreTaskRuntimes()
	go s.reconcileConcurrency()
	s.startTaskArchiveWorker()
	s.wireInterceptReviewer() // LLM 兜底审批:未命中拦截规则的命令交给模型判定
	return s
}

// Restored deadline and worker loops must inherit the same context as new tasks,
// including the side-question checkpoint publisher installed during startup.
func (s *Server) restoreTaskRuntimes() {
	m := s.m
	// reload tasks persisted on disk so the task list survives a restart, and
	// restore persisted paused state (so a task paused before restart stays paused).
	for _, t := range m.LoadExisting() {
		lifecycle := t.lifecycleSnapshot()
		// clear stale 'running' intents from a prior crash/restart (no live worker
		// owns them) so they re-claim instead of spinning forever in the UI.
		if n, _ := t.Store.ResetRunningIntents(); n > 0 {
			log.Printf("[engine] task %s 重置 %d 个残留 running 意图为 open", t.ID, n)
		}
		if lifecycle.Paused {
			s.engine.Pause(t.ID, agent.AbortPausedOnReload)
		}
		// 任务级超时:为每个未终态、带 timeout 的任务起 deadline 协调器,独立于 planner/worker
		// loop——非活跃任务重启后也能在到点后被收尾(deadline 已过则立即走收尾时序)。
		if !isTerminalStatus(lifecycle.Status) {
			s.engine.startDeadlineCoordinator(s.ctx, t)
		}
	}
	// Restore every task that had already been admitted before shutdown. Starting
	// only the active UI task left other non-queued tasks counted as concurrency
	// occupants without live loops, which could permanently block the persistent
	// FIFO. Paused loops remain idle; queued tasks are admitted below as slots allow.
	for _, t := range m.List() {
		lifecycle := t.lifecycleSnapshot()
		if !lifecycle.Queued && !isTerminalStatus(lifecycle.Status) {
			s.engine.Run(s.ctx, t)
		}
	}
}

// agentMaxTurns returns the configured max_turns for an agent key (0 = unlimited,
// also the fallback when no DB or no row).
func (s *Server) agentMaxTurns(key string) int {
	if s.m.pg == nil {
		return 0
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil {
		return 0
	}
	return a.MaxTurns
}

// agentRunSeconds returns the configured wall-clock run budget (seconds) for an
// agent key (0 = unlimited; 1200 fallback when no DB or no row, matching schema).
func (s *Server) agentRunSeconds(key string) int {
	if s.m.pg == nil {
		return 1200
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil {
		return 1200
	}
	return a.RunSecs
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerSideRoutes(mux)

	// Auth routes — exempt from JWT check (handled in requireAuth)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/init", s.authInit)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/change-password", s.authChangePassword)

	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/logs", s.getLogs)
	mux.HandleFunc("GET /api/logs/history", s.getLogsHistory)
	mux.HandleFunc("GET /api/logs/stream", s.streamLogs)

	// 页面一键更新。走的是默认的 JWT 鉴权（auth.go 只放行 /api/auth/* 和
	// /api/health），所以这几个改动程序自身的接口天然需要登录。
	mux.HandleFunc("GET /api/update/check", s.updateCheck)
	mux.HandleFunc("POST /api/update/apply", s.updateApply)
	mux.HandleFunc("POST /api/update/rollback", s.updateRollback)
	mux.HandleFunc("GET /api/update/stream", s.updateStream)

	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("GET /api/task-categories", s.pgListTaskCategories)
	mux.HandleFunc("POST /api/task-categories", s.pgCreateTaskCategory)
	mux.HandleFunc("PATCH /api/task-categories/{id}", s.pgRenameTaskCategory)
	mux.HandleFunc("DELETE /api/task-categories/{id}", s.pgDeleteTaskCategory)
	mux.HandleFunc("POST /api/tasks/category/batch", s.updateTasksCategoryBatch)
	mux.HandleFunc("GET /api/task-templates", s.pgListTaskTemplates)
	mux.HandleFunc("POST /api/task-templates", s.pgCreateTaskTemplate)
	mux.HandleFunc("PATCH /api/task-templates/{id}", s.pgUpdateTaskTemplate)
	mux.HandleFunc("DELETE /api/task-templates/{id}", s.pgDeleteTaskTemplate)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.updateTaskMetadata)
	mux.HandleFunc("PATCH /api/tasks/{id}/category", s.updateTaskCategory)
	// 任务级资产拦截/允许规则
	mux.HandleFunc("GET /api/tasks/{id}/intercept-rules", s.taskInterceptListRules)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules", s.taskInterceptCreateRule)
	mux.HandleFunc("PUT /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptDeleteRule)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules/{rid}/toggle", s.taskInterceptToggleRule)
	mux.HandleFunc("POST /api/tasks/control/batch", s.controlTasksBatch)
	mux.HandleFunc("GET /api/task-archives", s.listTaskArchives)
	mux.HandleFunc("GET /api/task-archives/{id}", s.getTaskArchive)
	mux.HandleFunc("POST /api/tasks/{id}/archive", s.queueTaskArchive)
	mux.HandleFunc("POST /api/tasks/archive/batch", s.queueTaskArchivesBatch)
	mux.HandleFunc("POST /api/task-archives/{id}/restore", s.queueTaskArchiveRestore)
	mux.HandleFunc("POST /api/task-archives/restore/batch", s.restoreTaskArchivesBatch)
	mux.HandleFunc("DELETE /api/task-archives/{id}", s.queueTaskArchiveDelete)
	mux.HandleFunc("POST /api/task-archives/delete/batch", s.deleteTaskArchivesBatch)
	mux.HandleFunc("GET /api/tasks/{id}/coverage", s.taskCoverage)
	mux.HandleFunc("GET /api/tasks/{id}/coverage-graph", s.taskCoverageGraph)
	mux.HandleFunc("GET /api/tasks/{id}/asset-refs", s.taskAssetRefs)
	mux.HandleFunc("POST /api/tasks/{id}/assets", s.attachTaskAssets)
	mux.HandleFunc("DELETE /api/tasks/{id}/assets/{assetID}", s.detachTaskAsset)
	mux.HandleFunc("GET /api/tasks/{id}/intent-assets", s.taskIntentAssets)

	// 工作空间文件管理器（针对 workDir）
	mux.HandleFunc("GET /api/workspace/list", s.wsList)
	mux.HandleFunc("GET /api/workspace/read", s.wsRead)
	mux.HandleFunc("POST /api/workspace/write", s.wsWrite)
	mux.HandleFunc("POST /api/workspace/mkdir", s.wsMkdir)
	mux.HandleFunc("DELETE /api/workspace/delete", s.wsDelete)
	mux.HandleFunc("GET /api/workspace/download", s.wsDownload)
	mux.HandleFunc("POST /api/workspace/upload", s.wsUpload)
	mux.HandleFunc("GET /api/tasks/{id}/scope", s.taskScopeList)
	mux.HandleFunc("POST /api/tasks/{id}/scope", s.taskScopeAdd)
	mux.HandleFunc("DELETE /api/tasks/{id}/scope/{sid}", s.taskScopeDelete)
	mux.HandleFunc("GET /api/tasks/{id}/goals", s.listGoals)                       // 目标管理:列出本任务全部目标
	mux.HandleFunc("POST /api/tasks/{id}/goals", s.addGoal)                        // 目标管理:人工新增目标(复活任务)
	mux.HandleFunc("PATCH /api/tasks/{id}/goals/{gid}", s.editGoal)                // 目标管理:修改目标(复活任务)
	mux.HandleFunc("DELETE /api/tasks/{id}/goals/{gid}", s.deleteGoal)             // 目标管理:硬删除目标(不复活)
	mux.HandleFunc("GET /api/tasks/{id}/constraints", s.listConstraints)           // 约束管理:列出本任务操作约束
	mux.HandleFunc("POST /api/tasks/{id}/constraints", s.addConstraint)            // 约束管理:新增约束(不通知 planner)
	mux.HandleFunc("PATCH /api/tasks/{id}/constraints/{cid}", s.editConstraint)    // 约束管理:修改约束
	mux.HandleFunc("DELETE /api/tasks/{id}/constraints/{cid}", s.deleteConstraint) // 约束管理:删除约束
	mux.HandleFunc("POST /api/tasks/{id}/control", s.control)
	mux.HandleFunc("PUT /api/tasks/{id}/llm", s.updateTaskLLMProfiles)
	mux.HandleFunc("GET /api/tasks/{id}/llm/resolution", s.taskLLMResolutionHandler)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/control", s.controlIntent)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/messages", s.sendWorkerMessage)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/rerun", s.rerunIntent)    // 重跑单条 blocked/exhausted/stopped 意图
	mux.HandleFunc("POST /api/tasks/{id}/intents/rerun-blocked", s.rerunBlocked) // 批量重跑本任务全部 blocked 意图
	mux.HandleFunc("POST /api/active", s.setActive)

	mux.HandleFunc("GET /api/llm", s.getLLM)
	mux.HandleFunc("POST /api/llm", s.setLLM)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)

	// asset system
	mux.HandleFunc("GET /api/assets", s.listAssets)
	mux.HandleFunc("GET /api/assets/counts", s.assetCounts)
	mux.HandleFunc("POST /api/assets", s.insertAssets)
	mux.HandleFunc("DELETE /api/assets", s.deleteAssets)

	// company system
	mux.HandleFunc("GET /api/companies", s.listCompanies)
	mux.HandleFunc("POST /api/companies", s.createCompany)
	mux.HandleFunc("GET /api/companies/{id}", s.getCompany)
	mux.HandleFunc("DELETE /api/companies/{id}", s.deleteCompany)
	mux.HandleFunc("POST /api/companies/{id}/scope", s.addCompanyScope)
	mux.HandleFunc("POST /api/companies/reattribute", s.reattribute)

	mux.HandleFunc("GET /api/exploration/frontier", s.frontier)
	mux.HandleFunc("GET /api/exploration/findings", s.findings)
	mux.HandleFunc("GET /api/exploration/findings/groups", s.findingGroups)
	mux.HandleFunc("GET /api/exploration/findings/asset-tree", s.findingAssetTree)
	mux.HandleFunc("GET /api/exploration/findings/stats", s.findingStats)
	mux.HandleFunc("GET /api/exploration/findings/export", s.findingsExport)
	s.registerFindingTraffic(mux)
	mux.HandleFunc("GET /api/exploration/findings/{id}", s.getFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/lineage", s.findingLineage)
	mux.HandleFunc("POST /api/exploration/findings/{id}/deepen", s.deepenFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/retests", s.listFindingRetests)
	mux.HandleFunc("GET /api/exploration/findings/retests/active", s.listActiveFindingRetests)
	mux.HandleFunc("POST /api/exploration/findings/{id}/retests", s.startFindingRetest)
	mux.HandleFunc("PATCH /api/exploration/findings/{id}", s.patchFinding)
	mux.HandleFunc("DELETE /api/exploration/findings/{id}", s.deleteFinding)
	mux.HandleFunc("GET /api/exploration/intents", s.intents)
	mux.HandleFunc("GET /api/exploration/graph", s.explorationGraph)
	mux.HandleFunc("GET /api/exploration/nodes", s.explorationNodes)
	mux.HandleFunc("GET /api/exploration/activity", s.activity)
	mux.HandleFunc("GET /api/exploration/activity/history", s.activityHistory)
	mux.HandleFunc("GET /api/exploration/main-sessions", s.mainSessions)
	mux.HandleFunc("POST /api/exploration/main-session/new", s.newMainSession)
	mux.HandleFunc("GET /api/exploration/activity/stream", s.streamActivity)
	mux.HandleFunc("GET /api/exploration/activity/{seq}", s.activityDetail)
	mux.HandleFunc("GET /api/exploration/tokens", s.tokenStats)
	mux.HandleFunc("GET /api/tokens/daily", s.tokenDailyStats)
	mux.HandleFunc("GET /api/tokens/conversations", s.conversationTokens)
	mux.HandleFunc("GET /api/tokens/usage", s.pgUsageStats) // 全局 llm_usage 聚合（仪表盘新版视图）

	mux.HandleFunc("GET /api/audit", s.getAudit)
	mux.HandleFunc("POST /api/gc", s.gc)
	mux.HandleFunc("GET /api/traffic", s.getTraffic)
	mux.HandleFunc("GET /api/traffic/hosts", s.getTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic", s.deleteTraffic)
	mux.HandleFunc("DELETE /api/traffic/hosts", s.deleteTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic/all", s.deleteAllTraffic)
	mux.HandleFunc("GET /api/traffic/exchange", s.getTrafficExchange)
	mux.HandleFunc("GET /api/traffic/blob", s.getTrafficBlob)
	mux.HandleFunc("GET /api/commands", s.pgListCommands)
	mux.HandleFunc("GET /api/commands/stats", s.pgToolStats) // 按工具聚合调用次数
	mux.HandleFunc("GET /api/llm/records", s.pgListLLMRecords)
	mux.HandleFunc("DELETE /api/llm/records", s.pgDeleteLLMRecords)
	mux.HandleFunc("GET /api/llm/records/tasks", s.pgLLMTasks)
	mux.HandleFunc("GET /api/llm/records/by-model", s.pgTokenByModel) // 按模型聚合本任务 token 用量
	mux.HandleFunc("GET /api/llm/records/{id}", s.pgGetLLMRecord)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	// 漏洞 IM 推送。渠道是「多实例 + 各自过滤规则」的资源，因此独立成一组
	// REST 接口，而不是塞进扁平的 /api/settings 键值里。
	mux.HandleFunc("GET /api/notify/meta", s.notifyMeta)
	mux.HandleFunc("GET /api/notify/channels", s.notifyListChannels)
	mux.HandleFunc("POST /api/notify/channels", s.notifyCreateChannel)
	mux.HandleFunc("PATCH /api/notify/channels/{id}", s.notifyUpdateChannel)
	mux.HandleFunc("DELETE /api/notify/channels/{id}", s.notifyDeleteChannel)
	mux.HandleFunc("POST /api/notify/channels/{id}/test", s.notifyTestChannel)
	mux.HandleFunc("GET /api/notify/deliveries", s.notifyListDeliveries)
	mux.HandleFunc("POST /api/notify/deliveries/{id}/retry", s.notifyRetryDelivery)
	mux.HandleFunc("POST /api/settings/web-search/test", s.testWebSearch)
	mux.HandleFunc("GET /api/report", s.getReport)
	mux.HandleFunc("GET /api/chat/mentions", s.searchChatMentions)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/chat/upload", s.chatUpload) // 方式1 文件上传:落到会话/任务工作目录 uploads/
	mux.HandleFunc("GET /api/tasks/{id}/chat/status", s.taskChatStatus)
	mux.HandleFunc("POST /api/tasks/{id}/chat/stop", s.stopChat)

	// --- 管理后台 API (PostgreSQL 数据源; 新版数据库与管理后台方案) ---
	mux.HandleFunc("DELETE /api/tasks/{id}", s.pgDeleteTask)
	// Agents
	// conversations (chat page)
	mux.HandleFunc("GET /api/conversations", s.pgListConversations)
	mux.HandleFunc("POST /api/conversations", s.pgCreateConversation)
	mux.HandleFunc("POST /api/conversations/delete/batch", s.pgDeleteConversationsBatch)
	mux.HandleFunc("PATCH /api/conversations/{id}", s.pgRenameConversation)
	mux.HandleFunc("PATCH /api/conversations/{id}/profile", s.pgUpdateConversation)
	mux.HandleFunc("DELETE /api/conversations/{id}", s.pgDeleteConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.pgConversationMessages)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.pgSendConversationMessage)
	mux.HandleFunc("POST /api/conversations/{id}/stop", s.pgStopConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages/{seq}", s.pgConversationMsgDetail)

	mux.HandleFunc("GET /api/agents", s.pgListAgents)
	mux.HandleFunc("POST /api/agents", s.pgCreateAgent)
	mux.HandleFunc("GET /api/agents/{key}", s.pgGetAgent)
	mux.HandleFunc("PATCH /api/agents/{key}", s.pgUpdateAgent)
	mux.HandleFunc("DELETE /api/agents/{key}", s.pgDeleteAgent)
	mux.HandleFunc("PUT /api/agents/{key}/config", s.pgSaveAgentConfig)
	mux.HandleFunc("PUT /api/agents/{key}/prompt", s.pgSavePrompt)
	mux.HandleFunc("POST /api/agents/{key}/prompt/reset", s.pgResetPrompt)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup", s.pgSaveWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/reset", s.pgResetWrapup)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup/task-timeout", s.pgSaveTaskTimeoutWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/task-timeout/reset", s.pgResetTaskTimeoutWrapup)
	mux.HandleFunc("GET /api/agents/{key}/triggers", s.pgListTriggers)
	mux.HandleFunc("POST /api/agents/{key}/triggers", s.pgCreateTrigger)
	mux.HandleFunc("PATCH /api/triggers/{id}", s.pgUpdateTrigger)
	mux.HandleFunc("DELETE /api/triggers/{id}", s.pgDeleteTrigger)
	mux.HandleFunc("GET /api/agents/{key}/prompts", s.pgListPromptVersions)
	mux.HandleFunc("GET /api/agents/{key}/variables", s.pgPromptVars)
	mux.HandleFunc("POST /api/agents/{key}/prompt/preview", s.pgPreviewPrompt)
	mux.HandleFunc("GET /api/agents/{key}/visibility", s.pgGetAgentVisibility)
	mux.HandleFunc("PUT /api/agents/{key}/visibility", s.pgSetAgentVisibility)
	// 内置工具目录（描述/参数默认值可改、按 agent 绑定；key 与 handler 在代码层）
	mux.HandleFunc("GET /api/tools", s.pgListTools)
	mux.HandleFunc("PUT /api/tools/{key}", s.pgUpdateTool)
	mux.HandleFunc("POST /api/tools/custom", s.pgCreateCustomTool)
	mux.HandleFunc("POST /api/tools/custom/test", s.pgTestCustomTool)
	mux.HandleFunc("PUT /api/tools/custom/{key}", s.pgUpdateCustomTool)
	mux.HandleFunc("DELETE /api/tools/custom/{key}", s.pgDeleteCustomTool)
	mux.HandleFunc("POST /api/settings/python/detect", s.pgDetectPython)
	mux.HandleFunc("POST /api/tools/{key}/reset", s.pgResetTool)
	// MCP CRUD
	mux.HandleFunc("GET /api/mcp", s.pgListMCP)
	mux.HandleFunc("POST /api/mcp", s.pgSaveMCP)
	mux.HandleFunc("DELETE /api/mcp/{id}", s.pgDeleteMCP)
	mux.HandleFunc("GET /api/mcp/{id}/tools", s.pgMCPTools)
	mux.HandleFunc("POST /api/mcp/{id}/refresh", s.pgRefreshMCP)
	// 资产同步 — ScopeSentry 数据源
	mux.HandleFunc("GET /api/sync/scopesentry/status", s.syncSSStatus)
	mux.HandleFunc("POST /api/sync/scopesentry/datasource", s.syncSSDatasource)
	mux.HandleFunc("GET /api/sync/scopesentry/projects", s.syncSSProjects)
	mux.HandleFunc("GET /api/sync/scopesentry/tasks", s.syncSSTasks)
	mux.HandleFunc("POST /api/sync/scopesentry/sync", s.syncSSRun)
	// Skill CRUD (文件系统)
	mux.HandleFunc("GET /api/skills", s.fsListSkills)
	mux.HandleFunc("POST /api/skills", s.fsCreateSkill)
	mux.HandleFunc("POST /api/skills/upload", s.fsUploadSkill)
	mux.HandleFunc("DELETE /api/skills/{name}", s.fsDeleteSkill)
	mux.HandleFunc("GET /api/skills/missing", s.fsMissingSkills)   // 未命中(想调但不存在)的 skill 名
	mux.HandleFunc("GET /api/skills/{name}/usage", s.fsSkillUsage) // 单个 skill 的最近调用
	mux.HandleFunc("PUT /api/skills/{name}/meta", s.fsUpdateSkillMeta)
	mux.HandleFunc("POST /api/skills/{name}/dirs", s.fsCreateDir)
	mux.HandleFunc("GET /api/skills/{name}/files", s.fsListFiles)
	// {file...} captures path segments including slashes (e.g. scripts/extract.py)
	mux.HandleFunc("GET /api/skills/{name}/files/{file...}", s.fsReadFile)
	mux.HandleFunc("PUT /api/skills/{name}/files/{file...}", s.fsWriteFile)
	mux.HandleFunc("DELETE /api/skills/{name}/files/{file...}", s.fsDeletePath)
	// MCP 资源侧可见性（更具体的 skill 路由会优先匹配）
	mux.HandleFunc("GET /api/visibility/{kind}/{id}", s.pgResourceVisibility)
	mux.HandleFunc("POST /api/visibility/toggle", s.pgToggleVisibility)
	// Skill 可见性（按名称，更具体，优先于上面的通配路由）
	mux.HandleFunc("GET /api/visibility/skill/{name}", s.pgSkillVisibility)
	mux.HandleFunc("POST /api/visibility/skill/toggle", s.pgToggleSkillVisibility)
	// LLM 多 profile
	mux.HandleFunc("GET /api/llm/profiles", s.pgListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{id}", s.pgDeleteProfile)
	mux.HandleFunc("POST /api/llm/profiles/active", s.pgActivateProfile)
	mux.HandleFunc("GET /api/llm/retry-policy", s.pgGetLLMRetryPolicy)
	mux.HandleFunc("POST /api/llm/retry-policy", s.pgSaveLLMRetryPolicy)
	mux.HandleFunc("GET /api/llm/pool", s.pgLLMPoolStatus)
	mux.HandleFunc("POST /api/llm/pool/reset", s.pgLLMPoolReset)
	mux.HandleFunc("POST /api/llm/models", s.pgListModels)

	// 拦截规则管理
	mux.HandleFunc("GET /api/intercept/rules", s.interceptListRules)
	mux.HandleFunc("POST /api/intercept/rules", s.interceptCreateRule)
	mux.HandleFunc("PUT /api/intercept/rules/{id}", s.interceptUpdateRule)
	mux.HandleFunc("DELETE /api/intercept/rules/{id}", s.interceptDeleteRule)
	mux.HandleFunc("POST /api/intercept/rules/{id}/toggle", s.interceptToggleRule)

	// 资产拦截规则管理（全局黑名单：域名/IP/URL/CIDR）
	mux.HandleFunc("GET /api/asset-intercept/rules", s.assetInterceptListRules)
	mux.HandleFunc("POST /api/asset-intercept/rules", s.assetInterceptCreateRule)
	mux.HandleFunc("PUT /api/asset-intercept/rules/{id}", s.assetInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/asset-intercept/rules/{id}", s.assetInterceptDeleteRule)
	mux.HandleFunc("POST /api/asset-intercept/rules/{id}/toggle", s.assetInterceptToggleRule)

	mux.HandleFunc("GET /api/intercept/pending", s.interceptListPending)
	mux.HandleFunc("GET /api/intercept/pending/{id}", s.interceptGetOne)
	mux.HandleFunc("POST /api/intercept/pending/{id}/decide", s.interceptDecide)
	mux.HandleFunc("GET /api/intercept/history", s.interceptHistory)
	mux.HandleFunc("GET /api/intercept/history/{id}", s.interceptDetail)
	mux.HandleFunc("GET /api/intercept/history/{id}/execution", s.interceptExecution)
	mux.HandleFunc("GET /api/intercept/task/{taskID}", s.interceptListTaskItems)
	mux.HandleFunc("GET /api/intercept/tool-config", s.interceptGetToolConfig)
	mux.HandleFunc("PUT /api/intercept/tool-config", s.interceptSetToolConfig)
	mux.HandleFunc("GET /api/intercept/judge", s.interceptGetJudgeConfig)
	mux.HandleFunc("PUT /api/intercept/judge", s.interceptSetJudgeConfig)
	mux.HandleFunc("GET /api/intercept/judge/usage", s.interceptJudgeUsage) // 兜底审批累计 token 用量

	// /api/* goes through CORS + JWT; everything else is served by the embedded
	// frontend (public — auth is enforced client-side and on the API). With the
	// no-embed build the webui handler just 404s (run `next dev` separately).
	api := cors(s.requireAuth(mux))
	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/", s.webuiHandler())
	return root
}

// --- handlers ---

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func atoiDefault(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
