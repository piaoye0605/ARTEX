package server

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// WaitIdle must block until every admitted task operation drains, then return nil.
// While draining, beginTaskOperation must reject NEW operations (one-way latch) so
// nothing can start a fresh write between WaitIdle returning and the DB closing.
func TestWaitIdleDrainsInflightOperations(t *testing.T) {
	e := NewEngine(nil)

	c1 := e.inflightCounter("t1")
	c2 := e.inflightCounter("t2")
	atomic.AddInt64(c1, 1)
	atomic.AddInt64(c2, 2)

	done := make(chan error, 1)
	go func() { done <- e.WaitIdle(context.Background()) }()

	// Give WaitIdle a moment to enter the drain loop, then assert the latch.
	time.Sleep(60 * time.Millisecond)
	if e.beginTaskOperation("t3") {
		t.Fatal("draining 期间 beginTaskOperation 不应接入新任务操作")
	}

	// Simulate the two operations finishing asynchronously (deferred DB writes).
	go func() {
		time.Sleep(120 * time.Millisecond)
		atomic.AddInt64(c1, -1)
		time.Sleep(120 * time.Millisecond)
		atomic.AddInt64(c2, -2)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitIdle 应在排空后返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitIdle 未在 3s 内排空在飞操作")
	}
}

// WaitIdle must time out with the leftover operation count in the error message,
// so the shutdown log says exactly how many writes are about to be dropped.
func TestWaitIdleTimesOutWithRemainingCount(t *testing.T) {
	e := NewEngine(nil)

	c := e.inflightCounter("t1")
	atomic.AddInt64(c, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := e.WaitIdle(ctx)
	if err == nil {
		t.Fatal("在飞未清零且 ctx 超时应返回错误")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("错误信息应包含剩余在飞计数 3: %v", err)
	}
}
