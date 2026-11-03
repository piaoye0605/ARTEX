package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func (s *Server) pgListProfiles(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ps, err := pg.ListProfiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"profiles": llmProfileDTOs(ps)})
}

func (s *Server) pgSaveProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	// APIKey has json:"-" on db.LLMProfile so it is never leaked to the UI on
	// read; accept it here via a sibling field for create/update. Streaming is a
	// *bool shadowing the embedded json:"streaming": an absent field must default
	// to streaming (true), which a plain bool zero value (false = non-streaming)
	// would get wrong — legacy/partial clients that never send it stay streaming.
	var body struct {
		db.LLMProfile
		APIKey    string `json:"api_key"`
		Streaming *bool  `json:"streaming"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := body.LLMProfile
	p.APIKey = body.APIKey
	p.Streaming = body.Streaming == nil || *body.Streaming
	// 输出上限:负数无意义,归零(= 不发送该字段)。字段名开关只有 Chat Completions
	// 用得上——anthropic 与 openai-responses 各自定死了字段名,存下来只会误导后续读者,
	// 故非 openai 格式一律清空。未知取值同样清空,避免把 DB CHECK 的报错甩给用户。
	if p.MaxTokens < 0 {
		p.MaxTokens = 0
	}
	if p.Format != "openai" || p.MaxTokensField != llm.MaxTokensFieldCompletion {
		p.MaxTokensField = ""
	}
	id, err := pg.SaveProfile(&p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Editing a profile rebuilds any task pinned to it on its next round. Reapply
	// the active profile too: the global fallback and explicit task chains must
	// repopulate the same provider-cache entry and therefore share one limiter.
	s.invalidateProfileAgents()
	// Hot-apply. Not just when the ACTIVE profile changed: with failover on, any
	// profile's key/model/priority/pool_exclude edit reshapes the chain, so the
	// engine's provider has to be rebuilt either way.
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"id": id})
}

// pgGetLLMRetryPolicy 返回全局重试策略(五层各自的次数+间隔)。未配置过 → 全零，
// 前端把零显示成「默认」。
func (s *Server) pgGetLLMRetryPolicy(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	writeJSON(w, 200, pg.LLMRetryPolicy())
}

// pgSaveLLMRetryPolicy 保存全局重试策略。三个「跟着端点走」的层(建连/空响应/同
// provider 安全窗口)是 provider 的构建参数或调用参数，改完必须让缓存里的 provider
// 重建；熔断参数则直接推给进程级 Registry。
func (s *Server) pgSaveLLMRetryPolicy(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var pol db.LLMRetryPolicy
	if err := decode(r, &pol); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetLLMRetryPolicy(pol); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.applyRetryPolicy()
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
	writeJSON(w, 200, pg.LLMRetryPolicy())
}

func (s *Server) pgDeleteProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	if err := pg.DeleteProfileContext(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrActiveLLMProfileDelete):
			writeErr(w, 409, "当前激活的 LLM 配置不能删除，请先激活其他配置")
		case errors.Is(err, db.ErrLLMProfileReferencesChanged):
			writeErr(w, 409, "LLM 配置正在被任务或会话修改，请重试")
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, 409, "等待 LLM 配置引用释放超时，请重试")
		case errors.Is(err, db.ErrLLMProfileNotFound):
			writeErr(w, 404, err.Error())
		default:
			writeErr(w, 500, err.Error())
		}
		return
	}
	s.invalidateProfileAgents() // drop cached agents for the removed profile
	s.llmHealth.Reset(id)       // its breaker state is meaningless now (row is FK-cascaded away)
	s.restoreTasksAfterProfileDelete(pg)
	// Cache invalidation also removed the active profile's shared provider entry.
	// Reapply after task-state sync so the wake-up observes the post-delete chain.
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) restoreTasksAfterProfileDelete(pg *db.DB) {
	for _, task := range s.m.List() {
		if taskID, err := strconv.ParseInt(task.ID, 10, 64); err == nil {
			if pt, err := pg.GetTask(taskID); err == nil && pt != nil {
				s.syncTaskLLMState(pt)
				// Deleting the active entry may advance the task to a ready successor,
				// or clear the explicit chain and restore its Agent/global fallback.
				// Resume only intents that were blocked by the exhausted chain; a
				// paused task remains paused and terminal tasks remain immutable.
				if !isTerminalStatus(task.lifecycleSnapshot().Status) && s.taskRuntimeAvailable(task, "planner", "worker") {
					if _, reopenErr := task.Store.ReopenIntentsByBlockedReason(db.IntentBlockedLLMQuota); reopenErr != nil {
						log.Printf("[llm-profile] task %s reopen quota-blocked intents after profile delete: %v", task.ID, reopenErr)
					}
				}
				task.Notify()
			}
		}
	}
}

func (s *Server) pgActivateProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetActiveProfile(body.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateProfileAgents() // active change may affect pinned-task fallbacks
	s.reapplyActiveProfile()    // switch the running engine to the newly activated profile
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgLLMPoolStatus reports the failover ("轮询") switches, the resolved chain order
// and every profile's circuit-breaker state — what the LLM page renders as the
// "轮询顺序" strip and the per-card health badges.
func (s *Server) pgLLMPoolStatus(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	writeJSON(w, 200, s.llmPoolStatus())
}

// pgLLMPoolReset clears a tripped profile's circuit breaker so the next call
// tries it again immediately ("立即恢复"). id=0 clears every profile.
func (s *Server) pgLLMPoolReset(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"` // 0 / omitted = all
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.ID > 0 {
		s.llmHealth.Reset(body.ID)
	} else {
		for id := range s.llmHealth.Snapshot() {
			s.llmHealth.Reset(id)
		}
	}
	writeJSON(w, 200, s.llmPoolStatus())
}

// pgListModels fetches available models from the provider's API endpoint.
// Supports OpenAI-format (GET /models) and Anthropic-format (GET /v1/models); for
// Anthropic-compatible third parties (e.g. DeepSeek) whose model list lives only on
// the OpenAI path, it falls back to the OpenAI endpoint at the stripped root.
func (s *Server) pgListModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider  string `json:"provider"` // "openai" | "anthropic"
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
		Proxy     string `json:"proxy"`
		ProfileID *int64 `json:"profile_id"` // fallback: use stored key from this profile
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Resolve API key: form input > profile stored key.
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" && req.ProfileID != nil {
		if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
			apiKey = p.APIKey
		}
	}
	if apiKey == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "未提供 API Key"})
		return
	}

	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	provider := strings.TrimSpace(req.Provider)

	// Candidate endpoints to try in order. Some Anthropic-compatible providers
	// (e.g. DeepSeek) implement /v1/messages under an /anthropic path but expose
	// the model list only on their OpenAI-format path — so for anthropic we fall
	// back to the OpenAI endpoint at the stripped root.
	type candidate struct {
		url string
		hdr http.Header
	}
	bearerHdr := func() http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+apiKey)
		return h
	}
	anthropicHdr := func() http.Header {
		h := http.Header{}
		h.Set("x-api-key", apiKey)
		h.Set("anthropic-version", "2023-06-01")
		return h
	}

	var candidates []candidate
	switch provider {
	case "openai", "openai-responses":
		// Responses API shares the OpenAI model list at /v1/models; tolerate a full
		// endpoint URL of either format.
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		b := strings.TrimRight(strings.TrimSuffix(strings.TrimSuffix(baseURL, "/chat/completions"), "/responses"), "/")
		candidates = append(candidates, candidate{b + "/models", bearerHdr()})
	default: // anthropic
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		b := strings.TrimRight(strings.TrimSuffix(baseURL, "/v1/messages"), "/")
		candidates = append(candidates, candidate{b + "/v1/models", anthropicHdr()})
		// Fallback for Anthropic-compatible third parties whose model list lives on
		// the OpenAI path: strip a trailing /anthropic and try the OpenAI endpoint.
		if root := strings.TrimRight(strings.TrimSuffix(b, "/anthropic"), "/"); root != b {
			candidates = append(candidates,
				candidate{root + "/models", bearerHdr()},
				candidate{root + "/v1/models", bearerHdr()},
			)
		}
	}

	// Build HTTP client with optional proxy.
	transport := &http.Transport{}
	if p := strings.TrimSpace(req.Proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	// Try each candidate; return the first that yields a non-empty model list.
	var lastErr string
	emptyOK := false
	for _, c := range candidates {
		httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, c.url, nil)
		if err != nil {
			lastErr = "构建请求失败: " + err.Error()
			continue
		}
		httpReq.Header = c.hdr
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = "请求失败: " + err.Error()
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("API 返回 %d: %s", resp.StatusCode, string(body[:min(len(body), 512)]))
			continue
		}
		// Both OpenAI and Anthropic return {"data": [{"id": "..."},...]}.
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			lastErr = "解析响应失败: " + err.Error()
			continue
		}
		models := make([]string, 0, len(parsed.Data))
		for _, m := range parsed.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
		if len(models) > 0 {
			writeJSON(w, 200, map[string]any{"ok": true, "models": models})
			return
		}
		emptyOK = true // 200 but no model ids — keep trying other candidates
	}
	if emptyOK {
		writeJSON(w, 200, map[string]any{"ok": true, "models": []string{}})
		return
	}
	if lastErr == "" {
		lastErr = "未获取到模型列表"
	}
	writeJSON(w, 200, map[string]any{"ok": false, "error": lastErr})
}

// --- prompt template helpers (Go text/template + catalog 白名单) ---
