package server

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/klauspost/compress/zstd"
)

// This file pins the recovery and failure paths of task_archive_package.go — the
// half of the cold-archive pipeline that only runs after an abrupt process death
// (`.staging` renames to replay, `.restore` installs to undo, `.deleting-*`
// packages to put back) plus the guard branches of the package writer/reader.
//
// The happy path lives in task_archive_package_test.go; here the assertions are
// mostly "the guard fires and nothing is lost", because the whole point of these
// paths is that the operator never has to hand-repair a data directory.

func writeArchiveFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readArchiveFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// newPackageRecoveryTask creates a live task (needed because task_archives has a
// FK to tasks) and removes both rows afterwards. A live row is exactly the state
// IsTaskArchiveRestored treats as "the restore committed".
func newPackageRecoveryTask(t *testing.T, m *Manager) int64 {
	t.Helper()
	task, err := m.CreateTask("archive recovery fixture "+strconv.FormatInt(time.Now().UnixNano(), 10), "recovery fixture", nil, 0, 0)
	if err != nil {
		t.Fatalf("create recovery task: %v", err)
	}
	id := taskIDOf(task)
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, id)
		_ = m.pg.DeleteTask(id)
	})
	return id
}

func insertRecoveryArchive(t *testing.T, m *Manager, taskID int64, state, archivePath string) int64 {
	t.Helper()
	var id int64
	if err := m.pg.QueryRow(
		`INSERT INTO task_archives (task_id,state,archive_path) VALUES ($1,$2,$3) RETURNING id`,
		taskID, state, archivePath,
	).Scan(&id); err != nil {
		t.Fatalf("insert archive row: %v", err)
	}
	return id
}

func TestWriteArchiveJournalRejectsBadValueAndTarget(t *testing.T) {
	dir := t.TempDir()
	// A channel cannot be marshalled: the journal is never written, so a caller
	// that aborted here still holds every file at its source path.
	if err := writeArchiveJournal(filepath.Join(dir, "chan.json"), make(chan int)); err == nil {
		t.Fatal("unmarshalable journal value was accepted")
	}
	// Parent path is a regular file: OpenFile must fail rather than silently
	// creating the journal somewhere unexpected.
	blocked := filepath.Join(dir, "blocked")
	writeArchiveFixture(t, blocked, "not a directory")
	if err := writeArchiveJournal(filepath.Join(blocked, "journal.json"), archiveStageJournal{ArchiveID: 1}); err == nil {
		t.Fatal("journal under a file path was accepted")
	}
	// Target itself is a directory.
	target := filepath.Join(dir, "as-dir")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveJournal(target, archiveStageJournal{ArchiveID: 1}); err == nil {
		t.Fatal("journal written over a directory")
	}
	ok := filepath.Join(dir, "journal.json")
	if err := writeArchiveJournal(ok, archiveStageJournal{ArchiveID: 7, TaskID: "42"}); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	var decoded archiveStageJournal
	if err := json.Unmarshal([]byte(readArchiveFixture(t, ok)), &decoded); err != nil {
		t.Fatalf("journal is not valid JSON: %v", err)
	}
	if decoded.ArchiveID != 7 || decoded.TaskID != "42" {
		t.Fatalf("journal round trip = %+v", decoded)
	}
}

func TestStageTaskArchiveFilesResumesJournaledMoves(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(taskArchiveRoot(dataDir), ".staging", "77")
	payload := filepath.Join(root, "payload")
	// Move 1 was already applied by the interrupted round.
	staged := filepath.Join(payload, "files", "tasks", "42", "evidence.txt")
	writeArchiveFixture(t, staged, "already staged")
	// Move 2 is still hot and must be replayed.
	pending := filepath.Join(dataDir, "transcripts", "exp9-worker.jsonl")
	writeArchiveFixture(t, pending, "pending transcript")

	journal := archiveStageJournal{ArchiveID: 77, TaskID: "42", Moves: []archiveFileMove{
		{Source: filepath.Join(dataDir, "tasks", "42", "evidence.txt"), Relative: filepath.Join("files", "tasks", "42", "evidence.txt")},
		{Source: pending, Relative: filepath.Join("files", "transcripts", "exp9-worker.jsonl")},
		// Move 3 vanished outside the archive flow: nothing to do, not an error.
		{Source: filepath.Join(dataDir, "transcripts", "exp9-gone.jsonl"), Relative: filepath.Join("files", "transcripts", "exp9-gone.jsonl")},
	}}
	if err := writeArchiveJournal(filepath.Join(root, "journal.json"), journal); err != nil {
		t.Fatal(err)
	}

	stage, err := stageTaskArchiveFiles(dataDir, 77, "42", 9)
	if err != nil {
		t.Fatalf("resume staging: %v", err)
	}
	if len(stage.journal.Moves) != 3 {
		t.Fatalf("resumed journal moves=%d, want the full plan of 3", len(stage.journal.Moves))
	}
	if got := readArchiveFixture(t, filepath.Join(payload, "files", "transcripts", "exp9-worker.jsonl")); got != "pending transcript" {
		t.Fatalf("pending transcript payload=%q", got)
	}
	if got := readArchiveFixture(t, staged); got != "already staged" {
		t.Fatalf("already staged payload was overwritten: %q", got)
	}
	assertPathMissing(t, pending)
	if _, err := os.Stat(filepath.Join(payload, "files", "transcripts", "exp9-gone.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("vanished source produced a payload entry: %v", err)
	}
}

func TestStageTaskArchiveFilesRejectsBadJournalAndBadTranscripts(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(taskArchiveRoot(dataDir), ".staging", "78")
	writeArchiveFixture(t, filepath.Join(root, "journal.json"), "{not json")
	if _, err := stageTaskArchiveFiles(dataDir, 78, "42", 9); err == nil || !strings.Contains(err.Error(), "read task archive staging journal") {
		t.Fatalf("corrupt staging journal err=%v", err)
	}

	// A journal path that is a directory is an I/O error, not "no journal yet":
	// treating it as absent would re-plan moves that may already be applied.
	dataDir2 := t.TempDir()
	root2 := filepath.Join(taskArchiveRoot(dataDir2), ".staging", "79")
	if err := os.MkdirAll(filepath.Join(root2, "journal.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := stageTaskArchiveFiles(dataDir2, 79, "42", 9); err == nil {
		t.Fatal("directory journal path was treated as a fresh staging round")
	}

	// A staging round with no transcripts directory at all is normal (fresh data
	// dir): the task workspace is still staged and the journal stays complete.
	dataDir3 := t.TempDir()
	writeArchiveFixture(t, filepath.Join(dataDir3, "tasks", "42", "evidence.txt"), "evidence")
	stage, err := stageTaskArchiveFiles(dataDir3, 80, "42", 9)
	if err != nil {
		t.Fatalf("staging without transcripts: %v", err)
	}
	if len(stage.journal.Moves) != 1 {
		t.Fatalf("journal moves=%d, want only the task workspace", len(stage.journal.Moves))
	}
	assertPathMissing(t, filepath.Join(dataDir3, "tasks", "42", "evidence.txt"))
}

func TestStageTaskArchiveFilesRollsBackWhenReplayFails(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(taskArchiveRoot(dataDir), ".staging", "81")
	payload := filepath.Join(root, "payload")
	// payload/files is a file, so MkdirAll("payload/files/tasks/42") fails while
	// replaying a journaled move — the round must roll back, not package half.
	writeArchiveFixture(t, filepath.Join(payload, "files"), "blocking file")
	journal := archiveStageJournal{ArchiveID: 81, TaskID: "42", Moves: []archiveFileMove{
		{Source: filepath.Join(dataDir, "tasks", "42", "evidence.txt"), Relative: filepath.Join("files", "tasks", "42", "evidence.txt")},
	}}
	writeArchiveFixture(t, filepath.Join(dataDir, "tasks", "42", "evidence.txt"), "evidence")
	if err := writeArchiveJournal(filepath.Join(root, "journal.json"), journal); err != nil {
		t.Fatal(err)
	}
	if _, err := stageTaskArchiveFiles(dataDir, 81, "42", 9); err == nil {
		t.Fatal("staging reported success with an unbuildable payload")
	}
	assertPathExists(t, filepath.Join(dataDir, "tasks", "42", "evidence.txt"))
}

func TestTaskArchiveStageRollbackConflictAndCommit(t *testing.T) {
	dataDir := t.TempDir()
	taskFile := filepath.Join(dataDir, "tasks", "42", "evidence.txt")
	writeArchiveFixture(t, taskFile, "evidence")
	stage, err := stageTaskArchiveFiles(dataDir, 82, "42", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Something recreated the source path after the rename: moving the payload
	// back would clobber it, so rollback must report instead of overwriting.
	writeArchiveFixture(t, taskFile, "recreated by operator")
	if err := stage.rollback(); err == nil || !strings.Contains(err.Error(), "archive rollback destination exists") {
		t.Fatalf("conflicting rollback err=%v", err)
	}
	if stage.done != true {
		t.Fatal("failed rollback left the stage reusable")
	}
	// A stage is single-shot: every later call is a no-op.
	if err := stage.rollback(); err != nil {
		t.Fatalf("rollback after done: %v", err)
	}
	if err := stage.commit(); err != nil {
		t.Fatalf("commit after done: %v", err)
	}
	assertPathExists(t, stage.root)
	assertPathExists(t, filepath.Join(stage.payload, "files", "tasks", "42", "evidence.txt"))

	// nil receivers are legal (defensive cleanup paths call them unconditionally).
	var nilStage *taskArchiveFileStage
	if err := nilStage.rollback(); err != nil {
		t.Fatalf("nil rollback: %v", err)
	}
	if err := nilStage.commit(); err != nil {
		t.Fatalf("nil commit: %v", err)
	}
}

func TestInstallTaskArchiveFilesGuards(t *testing.T) {
	dataDir := t.TempDir()
	for _, bad := range []string{"abc", "0", "-3"} {
		if _, err := installTaskArchiveFiles(dataDir, t.TempDir(), bad, 1); err == nil {
			t.Fatalf("restore accepted task id %q", bad)
		}
	}

	// Destination already hot: a restore must not overwrite live task files.
	extracted := filepath.Join(dataDir, "archives", "tasks", ".restore", "83")
	writeArchiveFixture(t, filepath.Join(extracted, "files", "tasks", "42", "evidence.txt"), "archived")
	writeArchiveFixture(t, filepath.Join(dataDir, "tasks", "42", "evidence.txt"), "live")
	if _, err := installTaskArchiveFiles(dataDir, extracted, "42", 83); err == nil || !strings.Contains(err.Error(), "restore destination already exists") {
		t.Fatalf("restore over live path err=%v", err)
	}

	// Journal path occupied by a directory: installation refuses before renaming.
	dataDir2 := t.TempDir()
	extracted2 := filepath.Join(dataDir2, "archives", "tasks", ".restore", "84")
	writeArchiveFixture(t, filepath.Join(extracted2, "files", "tasks", "42", "evidence.txt"), "archived")
	if err := os.MkdirAll(filepath.Join(extracted2, "restore-journal.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installTaskArchiveFiles(dataDir2, extracted2, "42", 84); err == nil {
		t.Fatal("restore proceeded without a writable journal")
	}
	assertPathExists(t, filepath.Join(extracted2, "files", "tasks", "42", "evidence.txt"))

	// An extraction tree without transcript entries only installs the workspace;
	// the journal is still written so recovery knows the install is in flight.
	dataDir3 := t.TempDir()
	extracted3 := filepath.Join(dataDir3, "archives", "tasks", ".restore", "85")
	writeArchiveFixture(t, filepath.Join(extracted3, "files", "tasks", "42", "evidence.txt"), "archived")
	installed, err := installTaskArchiveFiles(dataDir3, extracted3, "42", 85)
	if err != nil {
		t.Fatalf("restore without transcripts: %v", err)
	}
	if len(installed.moves) != 1 {
		t.Fatalf("restore moves=%d, want only the task workspace", len(installed.moves))
	}
	assertPathExists(t, filepath.Join(dataDir3, "tasks", "42", "evidence.txt"))
	assertPathExists(t, filepath.Join(extracted3, "restore-journal.json"))
	if err := installed.commit(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskArchiveRestoreRollbackConflictAndCommit(t *testing.T) {
	dataDir := t.TempDir()
	extracted := filepath.Join(dataDir, "archives", "tasks", ".restore", "86")
	source := filepath.Join(extracted, "files", "tasks", "42", "evidence.txt")
	writeArchiveFixture(t, source, "archived")
	stage, err := installTaskArchiveFiles(dataDir, extracted, "42", 86)
	if err != nil {
		t.Fatal(err)
	}
	// The extraction copy reappeared, so rolling back would clobber it.
	writeArchiveFixture(t, source, "recreated by operator")
	if err := stage.rollback(); err == nil || !strings.Contains(err.Error(), "restore rollback source exists") {
		t.Fatalf("conflicting restore rollback err=%v", err)
	}
	assertPathExists(t, filepath.Join(dataDir, "tasks", "42", "evidence.txt"))

	var nilStage *taskArchiveRestoreFiles
	if err := nilStage.rollback(); err != nil {
		t.Fatalf("nil restore rollback: %v", err)
	}
	if err := nilStage.commit(); err != nil {
		t.Fatalf("nil restore commit: %v", err)
	}

	// Committing an install drops the extraction tree but keeps the live files.
	dataDir2 := t.TempDir()
	extracted2 := filepath.Join(dataDir2, "archives", "tasks", ".restore", "87")
	writeArchiveFixture(t, filepath.Join(extracted2, "files", "tasks", "42", "evidence.txt"), "archived")
	installed, err := installTaskArchiveFiles(dataDir2, extracted2, "42", 87)
	if err != nil {
		t.Fatal(err)
	}
	if err := installed.commit(); err != nil {
		t.Fatal(err)
	}
	if err := installed.commit(); err != nil {
		t.Fatalf("second restore commit: %v", err)
	}
	assertPathMissing(t, extracted2)
	if got := readArchiveFixture(t, filepath.Join(dataDir2, "tasks", "42", "evidence.txt")); got != "archived" {
		t.Fatalf("installed payload=%q", got)
	}
}

func TestRecoverTaskArchiveRestoreStagesBranches(t *testing.T) {
	m, _ := newTestStack(t)

	// No .restore directory at all is the common case after a clean shutdown.
	if err := recoverTaskArchiveRestoreStages(t.TempDir(), m.pg); err != nil {
		t.Fatalf("missing restore dir: %v", err)
	}

	dataDir := t.TempDir()
	parent := filepath.Join(taskArchiveRoot(dataDir), ".restore")

	// A stray file in .restore is not a stage.
	writeArchiveFixture(t, filepath.Join(parent, "stray.txt"), "stray")
	// Extraction died before the journal: nothing had been renamed yet.
	writeArchiveFixture(t, filepath.Join(parent, "no-journal", "files", "tasks", "42", "a.txt"), "a")
	// Corrupt journal: reported, stage left in place for inspection.
	writeArchiveFixture(t, filepath.Join(parent, "bad-journal", "restore-journal.json"), "{")
	// Unknown archive id and an unknown task: the transaction never committed,
	// so the already-installed file must be moved back to the extraction tree.
	missingArchive := filepath.Join(parent, "missing-archive")
	missingDestination := filepath.Join(dataDir, "tasks", "9000000001", "evidence.txt")
	writeArchiveFixture(t, missingDestination, "installed")
	missingJournal := archiveRestoreJournal{
		ArchiveID: 9_000_000_001,
		TaskID:    9_000_000_001,
		Moves: []restoredArchivePath{{
			Source:      filepath.Join(missingArchive, "files", "tasks", "9000000001", "evidence.txt"),
			Destination: missingDestination,
		}},
	}
	raw, err := json.Marshal(missingJournal)
	if err != nil {
		t.Fatal(err)
	}
	writeArchiveFixture(t, filepath.Join(missingArchive, "restore-journal.json"), string(raw))
	// Same, but the journal carries no task id, so recovery cannot fall back to
	// the task row and must report the missing archive.
	noTaskID := filepath.Join(parent, "no-task-id")
	writeArchiveFixture(t, filepath.Join(noTaskID, "restore-journal.json"), `{"archive_id":9000000002,"task_id":0,"moves":[]}`)

	// Committed restore: the archive row exists and the task row is live, so the
	// extraction tree is pure garbage.
	taskID := newPackageRecoveryTask(t, m)
	archiveID := insertRecoveryArchive(t, m, taskID, pgdb.ArchiveReady, "")
	restored := filepath.Join(parent, "restored")
	committed := archiveRestoreJournal{ArchiveID: archiveID, TaskID: taskID}
	raw, err = json.Marshal(committed)
	if err != nil {
		t.Fatal(err)
	}
	writeArchiveFixture(t, filepath.Join(restored, "restore-journal.json"), string(raw))

	if err := recoverTaskArchiveRestoreStages(dataDir, m.pg); err == nil {
		t.Fatal("corrupt journal and unknown archive id were not reported")
	} else {
		t.Logf("restore recovery reported: %v", err)
	}

	assertPathExists(t, filepath.Join(parent, "stray.txt"))
	assertPathMissing(t, filepath.Join(parent, "no-journal"))
	assertPathExists(t, filepath.Join(parent, "bad-journal"))
	assertPathMissing(t, missingDestination)
	// The rollback moves installed files back into the extraction tree and then
	// discards that tree: the package on disk is authoritative and the persistent
	// restore job re-extracts from it on retry.
	assertPathMissing(t, missingArchive)
	assertPathExists(t, filepath.Join(noTaskID, "restore-journal.json"))
	assertPathMissing(t, restored)
}

func TestRecoverTaskArchiveStagesBranches(t *testing.T) {
	m, _ := newTestStack(t)

	if err := recoverTaskArchiveStages(t.TempDir(), m.pg); err != nil {
		t.Fatalf("missing staging dir: %v", err)
	}

	dataDir := t.TempDir()
	parent := filepath.Join(taskArchiveRoot(dataDir), ".staging")
	writeArchiveFixture(t, filepath.Join(parent, "stray.txt"), "stray")
	// Died between mkdir and journal: no rename had run.
	writeArchiveFixture(t, filepath.Join(parent, "no-journal", "payload", "files", "tasks", "42", "a.txt"), "a")
	// Corrupt journal.
	writeArchiveFixture(t, filepath.Join(parent, "bad-journal", "journal.json"), "{")

	// Unknown archive id: the archive row never committed, so staged files go back.
	unknown := filepath.Join(parent, "unknown")
	unknownSource := filepath.Join(dataDir, "transcripts", "exp9-orphan.jsonl")
	stagedFile := filepath.Join(unknown, "payload", "files", "transcripts", "exp9-orphan.jsonl")
	writeArchiveFixture(t, stagedFile, "orphan")
	writeArchiveFixture(t, filepath.Join(unknown, "journal.json"),
		`{"archive_id":9000000003,"task_id":"42","moves":[{"source":`+strconv.Quote(unknownSource)+`,"relative":"files/transcripts/exp9-orphan.jsonl"}]}`)

	// A ready archive owns a verified package: stale staging is discarded.
	readyTaskID := newPackageRecoveryTask(t, m)
	readyArchiveID := insertRecoveryArchive(t, m, readyTaskID, pgdb.ArchiveReady, "")
	ready := filepath.Join(parent, "ready")
	writeArchiveFixture(t, filepath.Join(ready, "payload", "files", "tasks", "x.txt"), "stale")
	writeArchiveFixture(t, filepath.Join(ready, "journal.json"),
		`{"archive_id":`+strconv.FormatInt(readyArchiveID, 10)+`,"task_id":"42","moves":[]}`)

	// A failed archive keeps a half-written package that must be deleted with the
	// rollback, otherwise the retry would resume from a partial archive.
	failedTaskID := newPackageRecoveryTask(t, m)
	failedPackage := filepath.Join(dataDir, "archives", "tasks", "task-"+strconv.FormatInt(failedTaskID, 10)+"-99.tar.zst")
	writeArchiveFixture(t, failedPackage, "partial package")
	failedArchiveID := insertRecoveryArchive(t, m, failedTaskID, pgdb.ArchiveFailed, failedPackage)
	failed := filepath.Join(parent, "failed")
	failedSource := filepath.Join(dataDir, "transcripts", "exp9-failed.jsonl")
	writeArchiveFixture(t, filepath.Join(failed, "payload", "files", "transcripts", "exp9-failed.jsonl"), "failed transcript")
	writeArchiveFixture(t, filepath.Join(failed, "journal.json"),
		`{"archive_id":`+strconv.FormatInt(failedArchiveID, 10)+`,"task_id":"`+strconv.FormatInt(failedTaskID, 10)+`","moves":[{"source":`+strconv.Quote(failedSource)+`,"relative":"files/transcripts/exp9-failed.jsonl"}]}`)

	if err := recoverTaskArchiveStages(dataDir, m.pg); err == nil {
		t.Fatal("corrupt journal was not reported")
	} else {
		t.Logf("staging recovery reported: %v", err)
	}

	assertPathExists(t, filepath.Join(parent, "stray.txt"))
	assertPathMissing(t, filepath.Join(parent, "no-journal"))
	assertPathExists(t, filepath.Join(parent, "bad-journal"))
	assertPathMissing(t, unknown)
	if got := readArchiveFixture(t, unknownSource); got != "orphan" {
		t.Fatalf("rolled back staged transcript=%q", got)
	}
	assertPathMissing(t, ready)
	assertPathMissing(t, failedPackage)
	if got := readArchiveFixture(t, failedSource); got != "failed transcript" {
		t.Fatalf("rolled back failed archive transcript=%q", got)
	}
}

func TestRecoverTaskArchiveDeletePackagesBranches(t *testing.T) {
	m, _ := newTestStack(t)

	// Missing archives root is the normal state on a fresh install.
	if err := recoverTaskArchiveDeletePackages(t.TempDir(), m.pg); err != nil {
		t.Fatalf("missing archives root: %v", err)
	}

	dataDir := t.TempDir()
	root := taskArchiveRoot(dataDir)
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeArchiveFixture(t, filepath.Join(root, "plain.tar.zst"), "no marker")
	writeArchiveFixture(t, filepath.Join(root, "a.tar.zst.deleting-abc"), "bad id")
	writeArchiveFixture(t, filepath.Join(root, "b.tar.zst.deleting-0"), "zero id")
	// Archive row gone: the delete already committed, so the staged file is trash.
	writeArchiveFixture(t, filepath.Join(root, "c.tar.zst.deleting-9000000004"), "orphan")

	// Ready archive: the delete never committed, so put the package back.
	readyTaskID := newPackageRecoveryTask(t, m)
	restorePath := filepath.Join(root, "restore-me.tar.zst")
	readyArchiveID := insertRecoveryArchive(t, m, readyTaskID, pgdb.ArchiveReady, restorePath)
	writeArchiveFixture(t, filepath.Join(root, "restore-me.tar.zst.deleting-"+strconv.FormatInt(readyArchiveID, 10)), "package")

	// Archive already queued for deletion: the idempotent worker consumes the
	// staged path itself, recovery must not race it.
	queuedTaskID := newPackageRecoveryTask(t, m)
	queuedArchiveID := insertRecoveryArchive(t, m, queuedTaskID, pgdb.DeleteQueued, "")
	queuedStaged := filepath.Join(root, "queued.tar.zst.deleting-"+strconv.FormatInt(queuedArchiveID, 10))
	writeArchiveFixture(t, queuedStaged, "queued package")

	// Restoring the package would clobber a different file: report instead.
	clashTaskID := newPackageRecoveryTask(t, m)
	clashArchiveID := insertRecoveryArchive(t, m, clashTaskID, pgdb.ArchiveReady, "")
	writeArchiveFixture(t, filepath.Join(root, "clash.tar.zst"), "replacement")
	writeArchiveFixture(t, filepath.Join(root, "clash.tar.zst.deleting-"+strconv.FormatInt(clashArchiveID, 10)), "package")

	if err := recoverTaskArchiveDeletePackages(dataDir, m.pg); err == nil || !strings.Contains(err.Error(), "archive delete recovery destination exists") {
		t.Fatalf("clashing delete recovery err=%v", err)
	}

	assertPathExists(t, filepath.Join(root, "plain.tar.zst"))
	assertPathExists(t, filepath.Join(root, "a.tar.zst.deleting-abc"))
	assertPathExists(t, filepath.Join(root, "b.tar.zst.deleting-0"))
	assertPathMissing(t, filepath.Join(root, "c.tar.zst.deleting-9000000004"))
	if got := readArchiveFixture(t, restorePath); got != "package" {
		t.Fatalf("restored package=%q", got)
	}
	assertPathExists(t, queuedStaged)
	assertPathExists(t, filepath.Join(root, "clash.tar.zst.deleting-"+strconv.FormatInt(clashArchiveID, 10)))
}

func TestStageTaskArchivePackageDeleteBranches(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := taskArchivePath(dataDir, 31, "42")
	// Neither the package nor its staged copy exists: nothing to move, not an error.
	got, moved, err := stageTaskArchivePackageDelete(archivePath, 31)
	if err != nil || moved || got != archivePath+".deleting-31" {
		t.Fatalf("missing package stage=%q moved=%v err=%v", got, moved, err)
	}

	writeArchiveFixture(t, archivePath, "package")
	if got, moved, err := stageTaskArchivePackageDelete(archivePath, 31); err != nil || !moved || got != archivePath+".deleting-31" {
		t.Fatalf("rename stage=%q moved=%v err=%v", got, moved, err)
	}
	assertPathMissing(t, archivePath)
	assertPathExists(t, archivePath+".deleting-31")

	// Both copies present is ambiguous: the package may be newer than the staged
	// delete, so refuse rather than pick one.
	writeArchiveFixture(t, archivePath, "package")
	if _, _, err := stageTaskArchivePackageDelete(archivePath, 31); err == nil || !strings.Contains(err.Error(), "归档包原文件和删除暂存文件同时存在") {
		t.Fatalf("ambiguous package err=%v", err)
	}
}

// craftedArchiveEntry is one entry for writeCraftedArchive.
type craftedArchiveEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func writeCraftedArchive(t *testing.T, path string, entries []craftedArchiveEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(file)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(encoder)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o600, Typeflag: entry.typeflag, Size: int64(len(entry.body)), Linkname: entry.linkname}
		if entry.typeflag != tar.TypeReg && entry.typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTaskArchivePackageGuards(t *testing.T) {
	if _, _, _, err := writeTaskArchivePackage(filepath.Join(t.TempDir(), "nil.tar.zst"), t.TempDir(), nil); err == nil {
		t.Fatal("nil snapshot was packaged")
	}
	// Parent path is a file: the package directory cannot be created.
	dir := t.TempDir()
	writeArchiveFixture(t, filepath.Join(dir, "blocked"), "file")
	payload := filepath.Join(dir, "payload")
	if err := os.MkdirAll(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := writeTaskArchivePackage(filepath.Join(dir, "blocked", "pkg.tar.zst"), payload, &pgdb.TaskArchiveSnapshot{}); err == nil {
		t.Fatal("package written under a file path")
	}
	// Payload directory missing: the manifest cannot be created.
	if _, _, _, err := writeTaskArchivePackage(filepath.Join(dir, "pkg.tar.zst"), filepath.Join(dir, "absent"), &pgdb.TaskArchiveSnapshot{}); err == nil {
		t.Fatal("package written from a missing payload directory")
	}
}

func TestExtractTaskArchivePackageGuards(t *testing.T) {
	dir := t.TempDir()
	if err := extractTaskArchivePackage(filepath.Join(dir, "absent.tar.zst"), "", filepath.Join(dir, "out")); err == nil {
		t.Fatal("missing package was extracted")
	}
	garbage := filepath.Join(dir, "garbage.tar.zst")
	writeArchiveFixture(t, garbage, "not zstd at all")
	if err := extractTaskArchivePackage(garbage, "", filepath.Join(dir, "out-garbage")); err == nil {
		t.Fatal("non-zstd payload was extracted")
	}

	// An absolute entry name must be rejected before anything is written. The
	// spelling depends on the platform: Windows filepath.IsAbs only accepts a
	// volume-qualified path, POSIX accepts a leading separator.
	absoluteName := "/abs.txt"
	if runtime.GOOS == "windows" {
		absoluteName = "C:/abs.txt"
	}
	cases := []struct {
		name    string
		entries []craftedArchiveEntry
		want    string
	}{
		{
			name:    "symlink entry",
			entries: []craftedArchiveEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "target"}},
			want:    "unsupported task archive entry type",
		},
		{
			name:    "absolute path",
			entries: []craftedArchiveEntry{{name: absoluteName, body: "x", typeflag: tar.TypeReg}},
			want:    "unsafe task archive path",
		},
		{
			name:    "parent traversal",
			entries: []craftedArchiveEntry{{name: "a/../../b.txt", body: "x", typeflag: tar.TypeReg}},
			want:    "unsafe task archive path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".tar.zst")
			writeCraftedArchive(t, path, tc.entries)
			err := extractTaskArchivePackage(path, "", filepath.Join(dir, "out-"+strings.ReplaceAll(tc.name, " ", "-")))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("extract err=%v, want substring %q", err, tc.want)
			}
		})
	}

	// A duplicate entry cannot overwrite the first one (O_EXCL).
	dup := filepath.Join(dir, "duplicate.tar.zst")
	writeCraftedArchive(t, dup, []craftedArchiveEntry{
		{name: "same.txt", body: "first", typeflag: tar.TypeReg},
		{name: "same.txt", body: "second", typeflag: tar.TypeReg},
	})
	if err := extractTaskArchivePackage(dup, "", filepath.Join(dir, "out-dup")); err == nil {
		t.Fatal("duplicate archive entry overwrote an extracted file")
	}

	// Directory + file without a checksum: the hashing branch is skipped.
	ok := filepath.Join(dir, "ok.tar.zst")
	writeCraftedArchive(t, ok, []craftedArchiveEntry{
		{name: "sub", typeflag: tar.TypeDir},
		{name: "sub/file.txt", body: "payload", typeflag: tar.TypeReg},
	})
	out := filepath.Join(dir, "out-ok")
	if err := extractTaskArchivePackage(ok, "", out); err != nil {
		t.Fatalf("extract clean package: %v", err)
	}
	if got := readArchiveFixture(t, filepath.Join(out, "sub", "file.txt")); got != "payload" {
		t.Fatalf("extracted payload=%q", got)
	}
}
