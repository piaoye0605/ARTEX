package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// pgReady returns the PG handle, or writes 503 and returns nil if unavailable.
func (s *Server) pg(w http.ResponseWriter) *db.DB {
	if s.m.pg == nil {
		writeErr(w, 503, "管理后台数据源(PostgreSQL)未连接")
		return nil
	}
	return s.m.pg
}

func pathInt(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil
}

func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// ---------- tasks (delete) ----------

const taskDeleteDrainTimeout = 10 * time.Second

func canonicalTaskID(raw string) (string, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

// beginTaskDelete serializes the delete barrier with pause, resume, admission,
// and FIFO reconciliation. Every lifecycle path rechecks deleting after taking
// concMu, so none can commit a contradictory state once this returns.
func (s *Server) beginTaskDelete(taskID string) bool {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	if s.engine.IsDeleting(taskID) {
		return false
	}
	return s.engine.BeginDelete(taskID)
}

// abortTaskDelete restores the execution barrier from the committed task row,
// not from the in-memory state observed before deletion began. Keeping the task
// paused on an unreadable row is conservative: a later explicit resume can
// safely recover it without allowing work to escape an uncertain delete.
func (s *Server) abortTaskDelete(taskID string) {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	keepPaused := true
	if id, err := strconv.ParseInt(taskID, 10, 64); err == nil && s.m != nil && s.m.pg != nil {
		if persisted, getErr := s.m.pg.GetTask(id); getErr == nil && persisted != nil {
			keepPaused = persisted.Paused || persisted.Queued
		} else if task, ok := s.m.Task(taskID); ok {
			state := task.lifecycleSnapshot()
			keepPaused = state.Paused || state.Queued
			if getErr != nil {
				log.Printf("[task-delete] task %s 读取持久状态失败，使用内存状态恢复屏障: %v", taskID, getErr)
			}
		} else if getErr == nil {
			// The request targeted a task that does not exist. Do not retain a
			// synthetic pause entry after releasing its temporary delete barrier.
			keepPaused = false
		}
	}
	s.engine.AbortDelete(taskID, keepPaused)
}

func (s *Server) pgDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalTaskID(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "任务 id 无效")
		return
	}
	var opts DeleteTaskOptions
	if err := decode(r, &opts); err != nil && err != io.EOF {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if !s.beginTaskDelete(id) {
		writeErr(w, http.StatusConflict, "任务正在删除")
		return
	}
	deleted := false
	defer func() {
		if !deleted {
			s.abortTaskDelete(id)
		}
	}()

	// Main-agent runs use a separate context from planner/workers. Cancel it, then
	// wait until both execution domains have returned before removing transcripts.
	s.cancelTaskChat(id, agent.AbortTaskDeleted)
	drainCtx, cancelDrain := context.WithTimeout(r.Context(), taskDeleteDrainTimeout)
	defer cancelDrain()
	if err := s.waitTaskQuiescent(drainCtx, id); err != nil {
		writeErr(w, http.StatusConflict, "任务仍有运行中的 Agent，删除已取消")
		return
	}

	if err := s.drainTaskSideQuestions(drainCtx, id); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	result, err := s.m.DeleteTask(id, opts)
	if err != nil {
		var committed *taskDeleteCommittedError
		if errors.As(err, &committed) {
			// PostgreSQL is already gone. Complete runtime teardown and return the
			// auditable counts together with the post-commit cleanup warning.
			s.engine.StopTask(id)
			s.taskAgentMu.Lock()
			delete(s.taskAgents, id)
			s.taskAgentMu.Unlock()
			deleted = true
			writeCommittedTaskDelete(w, result, err)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	// Manager has removed the task from the registry, so no new API operation can
	// resolve it. Now stop and join every task-owned Engine goroutine and clear its
	// lifecycle maps before releasing the delete barrier.
	s.engine.StopTask(id)
	s.taskAgentMu.Lock()
	delete(s.taskAgents, id)
	s.taskAgentMu.Unlock()
	deleted = true
	writeJSON(w, 200, result)
}

func writeCommittedTaskDelete(w http.ResponseWriter, result DeleteTaskResult, err error) {
	result.CleanupWarning = err.Error()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) waitTaskQuiescent(ctx context.Context, taskID string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.chatMu.Lock()
		chatBusy := s.chatBusy[taskID]
		s.chatMu.Unlock()
		if s.engine.inflightCount(taskID) == 0 && !chatBusy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ---------- agents ----------

func (s *Server) pgResourceVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	agents, err := pg.ResourceAgents(r.PathValue("kind"), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"agents": idStrings(agents)})
}

func (s *Server) pgToggleVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		AgentID    int64  `json:"agent_id,string"`
		Kind       string `json:"kind"`
		ResourceID int64  `json:"resource_id"`
		Visible    bool   `json:"visible"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleVisibility(body.AgentID, body.Kind, body.ResourceID, body.Visible); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- llm profiles ----------
