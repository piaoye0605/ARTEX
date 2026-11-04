package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	pgdb "github.com/Autumn-27/artex/db"
)

// This file pins the cold-archive control plane in task_archives.go — the FIFO
// worker routes that previously had no HTTP-layer coverage at all (getTaskArchive,
// all queue* handlers, normalizeArchiveIDs, the topo ordering helpers). The
// packaging primitives themselves are covered by task_archive_package_test.go;
// here the same lifecycle runs end-to-end through the real background worker so
// the assertions describe queue → archive → restore → delete as users see it.
//
// Lifecycle contracts pinned here:
//   - archive done  → state "ready", package on disk, task row marked archived
//   - restore done  → archive row DELETED, package consumed, task live again (paused)
//   - delete done   → archive stub row + package both gone
//   - queue guards  → running task 409 (must be paused/terminal), queued task 409,
//                     unknown task 404, malformed id 400

// waitForArchiveState polls the archive row until it reaches want, or fails the
// test. The background worker wakes on notify and every 2s, so a few seconds is
// normally plenty; the generous bound only shows up on slow CI machines.
func waitForArchiveState(t *testing.T, s *Server, id int64, want string, timeout time.Duration) pgdb.TaskArchive {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		item, err := s.m.PG().GetTaskArchive(id)
		if err != nil {
			t.Fatalf("get archive %d: %v", id, err)
		}
		if item == nil {
			t.Fatalf("archive %d vanished while waiting for %s", id, want)
		}
		if item.State == want {
			return *item
		}
		if item.State == pgdb.ArchiveFailed || item.State == pgdb.RestoreFailed || item.State == pgdb.DeleteFailed {
			t.Fatalf("archive %d landed in %s while waiting for %s: %s", id, item.State, want, item.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive %d still %s after %s (want %s)", id, item.State, timeout, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForArchiveGone polls until the archive row is deleted (restore/delete both
// end that way) and then asserts the on-disk package is consumed too.
func waitForArchiveGone(t *testing.T, s *Server, id int64, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		item, err := s.m.PG().GetTaskArchive(id)
		if err != nil {
			t.Fatalf("get archive %d: %v", id, err)
		}
		if item == nil {
			if _, err := os.Stat(taskArchivePath(s.m.dir, id, taskID)); !os.IsNotExist(err) {
				t.Fatalf("archive %d row gone but package remains: %v", id, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive %d still %s after %s (want deleted)", id, item.State, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// cleanupArchiveRows removes any leftover archive row/package for a task so a
// failing assertion cannot leak state into the shared database.
func cleanupArchiveRows(t *testing.T, s *Server, taskID int64) {
	t.Helper()
	pg := s.m.PG()
	rows, err := pg.Query(`SELECT id FROM task_archives WHERE task_id=$1`, taskID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_, _ = pg.Exec(`DELETE FROM task_archives WHERE id=$1`, id)
		_ = os.Remove(taskArchivePath(s.m.dir, id, strconv.FormatInt(taskID, 10)))
	}
}

// pausedTask creates a task and flags it paused, the minimum precondition for
// archiving. The archive row cleanup is registered immediately.
func pausedTask(t *testing.T, m *Manager, s *Server, name string) (*Task, int64) {
	t.Helper()
	task, err := m.CreateTask(name, "archive fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	numeric, _ := strconv.ParseInt(task.ID, 10, 64)
	if err := m.PG().SetPaused(numeric, true); err != nil {
		t.Fatalf("pause task: %v", err)
	}
	t.Cleanup(func() { cleanupArchiveRows(t, s, numeric) })
	return task, numeric
}

func TestTaskArchiveLifecycleRoute(t *testing.T) {
	m, s := newTestStack(t)
	task, numeric := pausedTask(t, m, s, "archive roundtrip")
	archiveBase := "/api/task-archives"

	// Queue through the HTTP route: 202 with the archive_queued row echoed.
	var queued pgdb.TaskArchive
	rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+task.ID+"/archive", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queue archive: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &queued)
	if queued.ID <= 0 || queued.State != pgdb.ArchiveQueued || queued.TaskID != numeric {
		t.Fatalf("queued row=%+v", queued)
	}
	// The snapshot columns are captured at queue time, before the task goes cold.
	// CreateTask's first argument is the description, not the name.
	if queued.OriginalStatus == "" || queued.TaskDescription != "archive roundtrip" ||
		queued.TaskGoal != "archive fixture" {
		t.Fatalf("queue snapshot incomplete: %+v", queued)
	}

	ready := waitForArchiveState(t, s, queued.ID, pgdb.ArchiveReady, 30*time.Second)
	if ready.SHA256 == "" || ready.OriginalSize <= 0 || ready.CompressedSize <= 0 {
		t.Fatalf("ready archive metrics incomplete: %+v", ready)
	}
	if ready.ArchivedAt == nil || ready.FormatVersion != pgdb.TaskArchiveFormatVersion {
		t.Fatalf("ready archive metadata incomplete: %+v", ready)
	}
	// The package is real bytes on disk inside the managed root.
	if _, err := os.Stat(taskArchivePath(s.m.dir, queued.ID, task.ID)); err != nil {
		t.Fatalf("archive package missing after ready: %v", err)
	}

	// The list route finds it by task id and by state filter; page echo works.
	var page struct {
		pgdb.TaskArchivePage
	}
	rec = crudDo(t, s, http.MethodGet, archiveBase+"?q="+task.ID+"&state=ready&size=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list archives: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &page)
	if page.Total < 1 || page.Size != 1 {
		t.Fatalf("list page=%+v", page.TaskArchivePage)
	}
	found := false
	for _, item := range page.Items {
		if item.ID == queued.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("archive %d missing from filtered list (total=%d)", queued.ID, page.Total)
	}
	// A state that matches nothing yields a zero-total page; with no rows the
	// encoder emits "items":null (no omitempty on the slice) — pinned as-is.
	rec = crudDo(t, s, http.MethodGet, archiveBase+"?state=no-such-state", "")
	crudJSON(t, rec, &page)
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("bogus state filter page=%+v", page.TaskArchivePage)
	}

	// Single read returns the ready row with the same metrics.
	var one pgdb.TaskArchive
	rec = crudDo(t, s, http.MethodGet, archiveBase+"/"+strconv.FormatInt(queued.ID, 10), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get archive: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &one)
	if one.ID != queued.ID || one.SHA256 != ready.SHA256 || one.State != pgdb.ArchiveReady {
		t.Fatalf("single read mismatch: %+v vs %+v", one, ready)
	}

	// Restore consumes the package and the row, and brings the task back paused.
	rec = crudDo(t, s, http.MethodPost, archiveBase+"/"+strconv.FormatInt(queued.ID, 10)+"/restore", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queue restore: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var restoreQueued pgdb.TaskArchive
	crudJSON(t, rec, &restoreQueued)
	if restoreQueued.State != pgdb.RestoreQueued || restoreQueued.ID != queued.ID {
		t.Fatalf("restore queued row=%+v", restoreQueued)
	}
	waitForArchiveGone(t, s, queued.ID, task.ID, 30*time.Second)

	// loadRestoredTask runs in the worker right AFTER the row is deleted, so the
	// registration window is racy — poll instead of asserting immediately.
	var restored *Task
	deadline := time.Now().Add(15 * time.Second)
	for {
		if task2, ok := m.Task(task.ID); ok {
			restored = task2
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s not re-registered after restore", task.ID)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !restored.lifecycleSnapshot().Paused {
		t.Fatal("restored task lost its paused flag")
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/tasks/"+task.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("restored task not visible via API: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskArchiveQueueGuards(t *testing.T) {
	m, s := newTestStack(t)

	// Malformed ids never reach the store.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/abc/archive", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad task id: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/task-archives/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad archive id: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Unknown task / archive.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/999999999/archive", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/task-archives/999999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown archive: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A live (running, unpaused) task may not be archived.
	live, err := m.CreateTask("archive guard live", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create live task: %v", err)
	}
	t.Cleanup(func() { cleanupArchiveRows(t, s, mustTaskID(t, live.ID)) })
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+live.ID+"/archive", ""); rec.Code != http.StatusConflict {
		t.Fatalf("live task archive: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A queued task is refused even though it is paused — the queue is a
	// commitment the admission loop owns.
	queuedTask, _ := pausedTask(t, m, s, "archive guard queued")
	if err := m.EnqueueTask(queuedTask.ID, "bootstrap"); err != nil {
		t.Fatalf("enqueue task: %v", err)
	}
	rec := crudDo(t, s, http.MethodPost, "/api/tasks/"+queuedTask.ID+"/archive", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("queued task archive: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Restore/delete against a missing archive row are state errors (409), not
	// 404 — the UI distinguishes "no such archive" from "archive not in a state
	// that allows this".
	if rec := crudDo(t, s, http.MethodPost, "/api/task-archives/999999999/restore", ""); rec.Code != http.StatusConflict {
		t.Fatalf("restore unknown archive: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/task-archives/999999999", ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete unknown archive: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/task-archives/abc/restore", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("restore bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/task-archives/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete bad id: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskArchiveBatchRoutes(t *testing.T) {
	m, s := newTestStack(t)
	task, _ := pausedTask(t, m, s, "archive batch")

	// Empty and oversized batches are rejected before any row is created.
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/archive/batch", `{"task_ids":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var oversized []string
	for i := 0; i < 101; i++ {
		oversized = append(oversized, strconv.Itoa(900000+i))
	}
	body, _ := json.Marshal(map[string]any{"task_ids": oversized})
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/archive/batch", string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/tasks/archive/batch", `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Mixed batch: invalid id reported per-item, unknown task reported per-item,
	// the paused task queued for real. 202 = "batch accepted", not "all ok".
	mixed := fmt.Sprintf(`{"task_ids":["abc","999999999","%s","%s"]}`, task.ID, task.ID)
	rec := crudDo(t, s, http.MethodPost, "/api/tasks/archive/batch", mixed)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("mixed batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch struct {
		Items []archiveBatchItem `json:"items"`
	}
	crudJSON(t, rec, &batch)
	// Duplicates collapse; invalid entries are never dropped silently.
	var invalid, unknown, okItems int
	var archiveID int64
	for _, item := range batch.Items {
		switch {
		case item.ID == "abc":
			invalid++
			if item.OK || item.Error != "任务 id 无效" {
				t.Fatalf("invalid item=%+v", item)
			}
		case item.ID == "999999999":
			unknown++
			if item.OK || item.Error == "" {
				t.Fatalf("unknown item=%+v", item)
			}
		case item.ID == task.ID:
			okItems++
			if !item.OK || !item.Queued || item.Archive <= 0 {
				t.Fatalf("queued item=%+v", item)
			}
			archiveID = item.Archive
		}
	}
	if invalid != 1 || unknown != 1 || okItems != 1 || archiveID <= 0 {
		t.Fatalf("batch items=%+v", batch.Items)
	}
	waitForArchiveState(t, s, archiveID, pgdb.ArchiveReady, 30*time.Second)

	// Archive-id batch guards mirror the task batch.
	if rec := crudDo(t, s, http.MethodPost, "/api/task-archives/delete/batch", `{"archive_ids":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty archive batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := crudDo(t, s, http.MethodPost, "/api/task-archives/delete/batch", `{"archive_ids":[0]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("zero archive id: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var tooMany []int64
	for i := 0; i < 101; i++ {
		tooMany = append(tooMany, int64(900000+i))
	}
	body, _ = json.Marshal(map[string]any{"archive_ids": tooMany})
	if rec := crudDo(t, s, http.MethodPost, "/api/task-archives/delete/batch", string(body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized archive batch: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// An unknown archive id inside a batch is a per-item error, not a 4xx.
	rec = crudDo(t, s, http.MethodPost, "/api/task-archives/restore/batch", `{"archive_ids":[999999999]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore batch unknown: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &batch)
	if len(batch.Items) != 1 || batch.Items[0].OK || batch.Items[0].Error == "" {
		t.Fatalf("restore batch items=%+v", batch.Items)
	}

	// Delete the ready archive through the batch route: stub row and package
	// both disappear, exactly like the single-item route.
	rec = crudDo(t, s, http.MethodPost, "/api/task-archives/delete/batch",
		`{"archive_ids":[`+strconv.FormatInt(archiveID, 10)+`]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	crudJSON(t, rec, &batch)
	if len(batch.Items) != 1 || !batch.Items[0].OK || batch.Items[0].Archive != archiveID {
		t.Fatalf("delete batch items=%+v", batch.Items)
	}
	waitForArchiveGone(t, s, archiveID, task.ID, 30*time.Second)
}

func TestTaskArchiveOrderingHelpers(t *testing.T) {
	// normalizeArchiveIDs dedupes, preserves first-seen order, rejects empties,
	// non-positive ids and oversized batches.
	got, err := normalizeArchiveIDs([]int64{7, 5, 7, 5, 9})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(got) != 3 || got[0] != 7 || got[1] != 5 || got[2] != 9 {
		t.Fatalf("normalize=%v", got)
	}
	for _, tc := range []struct {
		name string
		in   []int64
		want string
	}{
		{"empty", nil, "archive_ids 不能为空"},
		{"zero id", []int64{1, 0}, "归档 id 0 无效"},
		{"negative id", []int64{-3}, "归档 id -3 无效"},
	} {
		if _, err := normalizeArchiveIDs(tc.in); err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err=%v want %q", tc.name, err, tc.want)
		}
	}
	oversized := make([]int64, 101)
	if _, err := normalizeArchiveIDs(oversized); err == nil || err.Error() != "一次最多处理 100 个归档" {
		t.Fatalf("oversized: err=%v", err)
	}

	// topoTaskIDs orders dependent→source for archive/delete and source→dependent
	// for restore. sources[a] lists a's source tasks.
	ids := []string{"child", "parent"}
	sources := map[string][]string{"child": {"parent"}}
	archiveOrder := topoTaskIDs(ids, sources, false)
	if archiveOrder[0] != "child" || archiveOrder[1] != "parent" {
		t.Fatalf("archive order=%v (dependent must go first)", archiveOrder)
	}
	restoreOrder := topoTaskIDs(ids, sources, true)
	if restoreOrder[0] != "parent" || restoreOrder[1] != "child" {
		t.Fatalf("restore order=%v (source must go first)", restoreOrder)
	}
	// Relations pointing outside the batch are ignored, not treated as cycles.
	partial := topoTaskIDs([]string{"solo"}, map[string][]string{"solo": {"ghost"}}, false)
	if len(partial) != 1 || partial[0] != "solo" {
		t.Fatalf("external relation=%v", partial)
	}
	// A damaged graph (cycle) must not drop ids — the stable fallback keeps the
	// batch operable.
	cycle := topoTaskIDs([]string{"a", "b"}, map[string][]string{"a": {"b"}, "b": {"a"}}, false)
	if len(cycle) != 2 {
		t.Fatalf("cycle fallback dropped ids: %v", cycle)
	}

	// validateArchivePath keeps every archive operation inside the managed root.
	dataDir := t.TempDir()
	root := taskArchiveRoot(dataDir)
	if err := validateArchivePath(dataDir, ""); err == nil {
		t.Fatal("empty path accepted")
	}
	if err := validateArchivePath(dataDir, filepath.Join(root, "..", "escape.tar.zst")); err == nil {
		t.Fatal("path outside managed root accepted")
	}
	if err := validateArchivePath(dataDir, filepath.Join(root, "task-1-2.tar.zst")); err != nil {
		t.Fatalf("path inside managed root rejected: %v", err)
	}
}
