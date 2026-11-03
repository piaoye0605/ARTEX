package server

import (
	"context"
	"testing"
	"time"
)

// newTestStack builds a Manager + Server on temp dirs with a cancelable engine
// context, and wires teardown in the only order that is race-free:
//
//	t.Cleanup is LIFO, and this cleanup is registered AFTER every t.TempDir()
//	call above, so it runs BEFORE the TempDir RemoveAll. It cancels the engine
//	ctx, drains in-flight operations (draining gate also stops new task
//	operations from creating directories), then closes the store.
//
// Without this, engine goroutines outlive the test, keep writing into the data
// dir, and TempDir cleanup fails with "directory is not empty" — while leaked
// engines from finished tests keep logging "sql: database is closed" for the
// rest of the package run.
func newTestStack(t *testing.T) (*Manager, *Server) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	if m.pg == nil {
		t.Skip("postgres unavailable — skipping")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(func() {
		cancel()
		drainCtx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = s.WaitIdle(drainCtx)
		m.Close()
	})
	return m, s
}
