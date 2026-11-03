package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Run starts the planner loop + N worker loops for a task. The loops always run
// but no-op until an LLM is configured (so a task created while idle picks up
// automatically once LLM is set from the UI).
func (e *Engine) Run(ctx context.Context, t *Task) {
	workers := e.m.Workers()
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		t.Notify() // already running — just nudge a planning round
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1+workers)
	e.deleteMu.RUnlock()
	e.touch(t.ID)
	runTaskRoutine(rt, func(loopCtx context.Context) { e.plannerLoop(loopCtx, t) })
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("work#%d", i+1)
		runTaskRoutine(rt, func(loopCtx context.Context) { e.workerLoop(loopCtx, t, name) })
	}
	e.startDeadlineCoordinator(ctx, t) // 任务级超时定时器(仅 timeout>0;去重)
	// 仅在「完全没有活动意图(open+running)」时才 kick 首轮规划。带种子意图的任务:种子已
	// 是 open,或已被上面刚起的 worker 抢先 claim 成 running——两种都算「有活干」,一律跳过
	// 首轮 planner,worker 直接领种子意图开跑,跑完由 NotifyDone/心跳唤醒 planner。
	// ⚠️ 不能用 Frontier(只数 open):worker 领取(open→running)与本检查存在竞态,会误 kick。
	// 重启自动恢复时也可能只剩 running 意图,同样应跳过。
	if has, _ := t.Store.HasActiveIntent(); !has {
		t.Notify() // kick the first planning round (acted on once LLM is ready)
	}
}

// plannerHeartbeatInterval 解析任务的 planner 心跳间隔。db.CreateTask 已归一
// (低于 600 一律抬到 600);这里再兜一次底,防内存态异常值。
func plannerHeartbeatInterval(t *Task) time.Duration {
	sec := t.PlanHeartbeatSeconds
	if sec < db.MinPlanHeartbeatSeconds { // 下限=默认=600(10min)
		sec = db.MinPlanHeartbeatSeconds
	}
	return time.Duration(sec) * time.Second
}

// resetPlannerTimer 安全重臂一个可能已触发的 Timer(标准 Stop→drain→Reset 模式)。
func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := plannerHeartbeatInterval(t)
	// 心跳定时器在 loop 入口臂 = 从任务 start 计时:即使是跳过首轮 planner 的 seed 任务
	// (Run 里 frontier 非空不 kick 首轮)、这里一直阻塞,心跳也会在「任务 start + interval」
	// 触发第一轮规划。之后每次唤醒(边沿/心跳)都重臂 = 距上次任意规划触发的时长。
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()

	// runRound 跑一轮规划(含 debounce 合并 + 各 guard)。src 仅用于日志区分触发来源。
	runRound := func(src string) {
		// debounce: coalesce a burst of changes into one planning round
		timer := time.NewTimer(e.debounce)
	drain:
		for {
			select {
			case <-t.notify:
			case <-timer.C:
				break drain
			}
		}
		planner, _ := e.snapshotFor(t)
		if planner == nil {
			return // idle until LLM configured
		}
		if e.IsPaused(t.ID) {
			return // user-paused: don't plan
		}
		if e.IsDeleting(t.ID) {
			return
		}
		// terminal task (goals all met → done, or failed): the run is over. A
		// resume/nudge — e.g. auto-resume of the active task on restart — must NOT
		// re-plan (it would burn an LLM round and re-confirm a settled result).
		if isTerminalStatus(t.lifecycleSnapshot().Status) {
			return
		}
		// 任务级超时收尾中:丢弃普通唤醒——worker 收尾写回、Resume 的 Notify 都不再
		// 触发常规规划轮;终局那一轮由协调器(settleTask)直接驱动,不走这里。
		if e.isSettling(t.ID) {
			return
		}
		// goalless（人工直投）分支：任务已无 open 目标时 planner 不跑——跑了会重判
		// met→cancelExec 杀掉用户经主 agent 直投的意图。是否结束改由 frontier 决定：
		// 还有 open/running 意图 → 保持 running、静默等待；意图已全部跑干 → 落 done。
		// 整段纯 Go、不触发任何 LLM 调用，也不打规划轮 marker。
		if open, err := t.Store.HasOpenGoal(); err == nil && !open {
			t.drainTriggers() // 丢弃累积的 done/finding 触发，避免 goalless 长会话里无界增长
			if active, err := t.Store.HasActiveIntent(); err == nil && !active {
				// frontier 抽干且无在跑意图 → 收尾。用 Guarded 版做 CAS，避免踩到并发的
				// pause/delete/超时收尾的状态转换。
				if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil {
					log.Printf("[goalless] task %s 收尾落 done 失败: %v", t.ID, err)
				} else if won {
					e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
						Summary: "目标已全部达成，直投意图已执行完毕，任务结束"})
				}
			}
			return // goalless 分支永不进入 planner.Plan
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		defer e.decInflight(t.ID)
		e.stampFirstRun(t) // 首次真正规划 → 盖 first_run_at + 算 deadline(仅带 timeout 的任务)
		e.touch(t.ID)
		emit := func(r db.Activity) { e.emitActivity(t, r) }
		ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; 带任务 deadline
		if ectx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		log.Printf("[planner] task %s 规划中…(%s 触发)", t.ID, src)
		// round marker: each Plan() is one planner round; emit a boundary so the
		// UI can separate rounds in the transcript (kind='round').
		e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
			Summary: fmt.Sprintf("第 %d 轮规划", e.nextPlannerRound(t.ID))})
		// what fired this round (worker done / finding; may be several — debounce
		// coalesces a burst; empty for time/heartbeat wakes).
		triggers := t.drainTriggers()
		taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
		e.BeginLLMCall(t.ID)
		met, reason, err := planner.Plan(ectx, taskIDInt, e.m.assets, t.Store, t.Goal, triggers, emit)
		e.EndLLMCall(t.ID)
		switch {
		case err != nil && ectx.Err() == nil:
			log.Printf("[planner] task %s 规划出错: %v", t.ID, err)
		case met:
			log.Printf("[planner] task %s 判定目标达成: %s", t.ID, reason)
			// 所有目标达成 → 持久化任务状态为 done（前端 DTO 会优先展示该终态）。
			if err := e.m.SetTaskStatus(t.ID, "done"); err != nil {
				log.Printf("[planner] task %s 标记完成落库失败: %v", t.ID, err)
			}
			// 任务已判完成 → 立刻取消在跑的 worker：它们手头的意图跑出来也没意义了。
			// 下一轮 worker 循环撞终态门就不再领新意图;被取消的这批走下方"任务已完成"分支
			// 归为 stopped(而非 blocked)。
			e.cancelExec(t.ID, agent.AbortGoalMet)
		default:
			log.Printf("[planner] task %s 规划完成", t.ID)
		}
		e.touch(t.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
			runRound("edge") // worker 结束 / finding / kill / resume / seed 首轮
		case <-heartbeat.C:
			// 周期兜底:死锁兜底 + 唤醒去监督飞行中的 worker(steer/kill) + 周期复查。
			runRound("heartbeat")
		}
		// 每次唤醒(边沿或心跳)后重臂心跳:任意规划触发都重算这段静置计时。
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, worker := e.snapshotFor(t)
		if worker == nil {
			if sleepCtx(ctx, 1500*time.Millisecond) {
				return
			}
			continue
		}
		if e.IsPaused(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // user-paused: don't claim/execute intents
		}
		if e.IsDeleting(t.ID) {
			return
		}
		if e.isSettling(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 任务超时收尾中:不再领新意图(在跑的自行收尾,协调器等其 drain)
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 任务已终态(done/failed/timeout):停止领取遗留意图,别在完成后空跑 frontier
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		claimed := e.runWorkerStep(ctx, t, name, worker)
		e.decInflight(t.ID)
		if !claimed && sleepCtx(ctx, 800*time.Millisecond) {
			return
		}
	}
}

// runWorkerStep claims one intent from the frontier and fully settles it via
// runIntent. Returns false when nothing was claimable. The pool worker loop is its
// only caller.
func (e *Engine) runWorkerStep(ctx context.Context, t *Task, name string, worker *agent.Worker) bool {
	intent := e.claimNext(t, name)
	if intent == nil {
		return false
	}
	log.Printf("[worker %s] task %s 领取意图 #%d", name, t.ID, intent.ID)
	return e.runIntent(ctx, t, name, worker, intent, "", "")
}
