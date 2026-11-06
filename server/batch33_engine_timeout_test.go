package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file pins the task-timeout primitives in engine_timeout.go that do not
// need a live planner/worker: the settling + inflight bookkeeping, WaitIdle's
// drainage contract, the deadline cache with its DB fallback, and the
// coordinator's early-outs (ctx canceled / already terminal / unstamped).
func TestEngineTimeoutPrimitives(t *testing.T) {
	e := NewEngine(nil)

	// settling: only the first caller wins the transition.
	if e.isSettling("7") {
		t.Fatal("fresh task must not be settling")
	}
	if !e.markSettling("7") || !e.isSettling("7") {
		t.Fatal("first markSettling must win")
	}
	if e.markSettling("7") {
		t.Fatal("second markSettling must lose")
	}

	// inflight accounting admits operations, and refuses them behind a delete barrier.
	if !e.beginTaskOperation("7") {
		t.Fatal("clean task must admit operations")
	}
	if e.inflightCount("7") != 1 {
		t.Fatalf("inflight after begin = %d", e.inflightCount("7"))
	}
	e.decInflight("7")
	if e.inflightCount("7") != 0 {
		t.Fatalf("inflight after dec = %d", e.inflightCount("7"))
	}
	e.deleting.Store("gone", true)
	if e.beginTaskOperation("gone") {
		t.Fatal("task behind a delete barrier must not admit operations")
	}
	e.deleting.Delete("gone")

	// WaitIdle: nothing in flight returns immediately.
	if err := e.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle with no inflight = %v", err)
	}
	// Once draining, new operations are refused (one-way latch).
	if e.beginTaskOperation("7") {
		t.Fatal("draining engine must refuse new operations")
	}
	// A leftover operation surfaces as a timeout error naming the count.
	counter := e.inflightCounter("slow")
	atomic.StoreInt64(counter, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	err := e.WaitIdle(ctx)
	if err == nil || !strings.Contains(err.Error(), "剩余 1 个在飞操作") {
		t.Fatalf("WaitIdle with leftover = %v", err)
	}
	atomic.StoreInt64(counter, 0)
	if err := e.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after drain = %v", err)
	}

	// taskDeadline: process map first, then the DB-loaded snapshot, then 0.
	future := time.Now().Add(time.Hour).Unix()
	if got := e.taskDeadline(&Task{ID: "no-deadline"}); got != 0 {
		t.Fatalf("unstamped deadline = %d", got)
	}
	task := &Task{ID: "from-snapshot", DeadlineAt: future}
	if got := e.taskDeadline(task); got != future {
		t.Fatalf("snapshot deadline = %d, want %d", got, future)
	}
	if v, ok := e.deadline.Load("from-snapshot"); !ok || v.(int64) != future {
		t.Fatalf("snapshot deadline not cached: %v %v", v, ok)
	}
	cached := &Task{ID: "from-snapshot", DeadlineAt: future + 999}
	if got := e.taskDeadline(cached); got != future {
		t.Fatalf("cached deadline must win: %d", got)
	}

	// resetTimeoutRevival clears only the per-run state.
	e.settling.Store("reset", true)
	e.stamped.Store("reset", true)
	e.deadline.Store("reset", future)
	e.coordStarted.Store("reset", true)
	e.resetTimeoutRevival("reset")
	if _, ok := e.settling.Load("reset"); ok {
		t.Fatal("settling survived resetTimeoutRevival")
	}
	if _, ok := e.stamped.Load("reset"); ok {
		t.Fatal("stamped survived resetTimeoutRevival")
	}
	if _, ok := e.deadline.Load("reset"); ok {
		t.Fatal("deadline survived resetTimeoutRevival")
	}
	if _, ok := e.coordStarted.Load("reset"); ok {
		t.Fatal("coordStarted survived resetTimeoutRevival")
	}

	// clockCtx: no timeout + non-final is a pass-through; final always tags the ctx.
	base := context.Background()
	if got := e.clockCtx(base, &Task{ID: "no-deadline"}, false); got != base {
		t.Fatal("clockCtx without deadline must return the base ctx unchanged")
	}
	if got := e.clockCtx(base, &Task{ID: "no-deadline"}, true); got == base {
		t.Fatal("final round must carry a task clock even without a deadline")
	}
	if got := e.clockCtx(base, &Task{ID: "from-snapshot", DeadlineAt: future}, false); got == base {
		t.Fatal("stamped deadline must carry a task clock")
	}

	// startDeadlineCoordinator early-outs: nil task, no timeout, already started.
	e.startDeadlineCoordinator(base, nil)
	e.startDeadlineCoordinator(base, &Task{ID: "no-timeout"})
	e.coordStarted.Store("already", true)
	e.startDeadlineCoordinator(base, &Task{ID: "already", TimeoutSeconds: 60})
	if _, ok := e.runtimes["already"]; ok {
		t.Fatal("already-started coordinator must not register routines again")
	}
	// A real launch is idempotent too, and stops with its ctx.
	ctx2, cancel2 := context.WithCancel(base)
	started := &Task{ID: "launch", TimeoutSeconds: 60, DeadlineAt: time.Now().Add(time.Hour).Unix()}
	e.startDeadlineCoordinator(ctx2, started)
	e.startDeadlineCoordinator(ctx2, started)
	cancel2()
}

func TestEngineDeadlineCoordinatorAndStamp(t *testing.T) {
	m, s := newTestStack(t)
	e := s.engine
	tag := fmt.Sprintf("batch33-%d", time.Now().UnixNano())
	task, err := m.CreateTask(tag, "deadline probe", nil, 60, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	numericID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatalf("task id %q: %v", task.ID, err)
	}
	defer func() { _ = m.pg.DeleteTask(numericID) }()

	// stampFirstRun: no timeout is a no-op; the first stamp records and caches it.
	e.stampFirstRun(&Task{ID: "no-timeout"})
	e.stampFirstRun(task)
	dl, ok := e.deadline.Load(task.ID)
	if !ok || dl.(int64) <= 0 {
		t.Fatalf("stampFirstRun did not cache a deadline: %v %v", dl, ok)
	}
	before := dl.(int64)
	e.stampFirstRun(task) // already stamped this process → no second DB write
	if again, _ := e.deadline.Load(task.ID); again.(int64) != before {
		t.Fatalf("second stamp changed the deadline: %d → %v", before, again)
	}
	// A failed stamp must not stick, so a later run can retry.
	e.stampFirstRun(&Task{ID: "999999999", TimeoutSeconds: 60})
	if _, stuck := e.stamped.Load("999999999"); stuck {
		t.Fatal("failed stamp must be retryable")
	}

	// deadlineCoordinator: a terminal task returns without entering settling.
	e.resetTimeoutRevival(task.ID)
	if _, err := m.SetTaskStatusGuarded(task.ID, "done"); err != nil {
		t.Fatalf("set terminal status: %v", err)
	}
	done := make(chan struct{})
	go func() {
		e.deadlineCoordinator(context.Background(), task)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit on a terminal task")
	}
	if e.isSettling(task.ID) {
		t.Fatal("terminal task must not enter settling")
	}

	// deadlineCoordinator: a canceled ctx exits from the unstamped poll loop.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	deadlineDone := make(chan struct{})
	go func() {
		e.deadlineCoordinator(cctx, &Task{ID: "unstamped", TimeoutSeconds: 60})
		close(deadlineDone)
	}()
	select {
	case <-deadlineDone:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit on a canceled ctx")
	}

	// settleTask is one-shot: whoever marked settling first owns the sequence.
	e.resetTimeoutRevival(task.ID)
	if !e.markSettling(task.ID) {
		t.Fatal("markSettling precondition failed")
	}
	e.settleTask(context.Background(), task) // 已进入收尾 → 直接返回，不改终态
	if status := m.TaskStatus(task.ID); status != "done" {
		t.Fatalf("double settle changed status to %q", status)
	}
}
