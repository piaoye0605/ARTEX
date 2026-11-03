package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// AppendActivity records one worker step and returns its id.
func (s *ExplorationStore) AppendActivity(a Activity) (int64, error) {
	metadata := a.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO activity(exploration_id, node_id, worker, kind, tool, tool_use_id, is_error, summary, detail, metadata, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15)
RETURNING id`, s.expID, a.NodeID, utf8Clean(a.Worker), utf8Clean(a.Kind), utf8Clean(a.Tool), utf8Clean(a.ToolUseID), a.IsError, utf8Clean(a.Summary), utf8Clean(a.Detail),
		metadata, a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.MainSeg).Scan(&id)
	return id, err
}

// ActivityList returns steps after sinceID (exclusive), optionally filtered by node.
// Returns items and the new cursor (max id seen).
func (s *ExplorationStore) ActivityList(nodeID *int64, sinceID int64, limit int) ([]Activity, int64, error) {
	if limit <= 0 {
		limit = 300
	}
	var rows *sql.Rows
	var err error
	const cols = `id, node_id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), metadata, created_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg`
	if nodeID != nil {
		rows, err = s.db.Query(`SELECT `+cols+`
FROM activity WHERE exploration_id=$1 AND node_id=$2 AND id>$3 ORDER BY id LIMIT $4`, s.expID, *nodeID, sinceID, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+cols+`
FROM activity WHERE exploration_id=$1 AND id>$2 ORDER BY id LIMIT $3`, s.expID, sinceID, limit)
	}
	if err != nil {
		return nil, sinceID, err
	}
	defer rows.Close()
	out := []Activity{}
	cursor := sinceID
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.Metadata, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens, &a.MainSeg); err != nil {
			return nil, sinceID, err
		}
		if a.ID > cursor {
			cursor = a.ID
		}
		out = append(out, a)
	}
	return out, cursor, rows.Err()
}

// ActivityListForTerminalIntent is the inherited, incremental-read boundary.
// The intent state check and activity read happen in one statement so a source
// intent reopened between API-level checks cannot expose its new in-flight trace.
// Model reasoning and accounting rows are never inherited.
func (s *ExplorationStore) ActivityListForTerminalIntent(nodeID, sinceID int64, limit int) ([]Activity, int64, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''),
		COALESCE(a.tool,''), COALESCE(a.tool_use_id,''), a.is_error, COALESCE(a.summary,''),
		a.created_at, a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cache_write_tokens
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2 AND a.id>$3
	  AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	ORDER BY a.id LIMIT $4`, s.expID, nodeID, sinceID, limit)
	if err != nil {
		return nil, sinceID, err
	}
	defer rows.Close()
	out := []Activity{}
	cursor := sinceID
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens); err != nil {
			return nil, sinceID, err
		}
		if a.ID > cursor {
			cursor = a.ID
		}
		out = append(out, a)
	}
	return out, cursor, rows.Err()
}

// ActivitySessionFilter selects one UI "session" within a task's activity stream.
// Exactly one field is meaningful:
//   - Main == true  → the main-agent session (worker="mainagent") for one segment
//     (MainSeg; nil/0 = the original segment, which also matches legacy NULL rows).
//   - Worker != ""  → filter by worker name (Plan = "planner").
//     Goal Agent 的第 0 轮拆解也以 worker="planner" 落库，故 Plan 会话完整覆盖 Goal+Planner。
//   - NodeID != nil → a Worker session, filtered by node_id (= intent id).
//
// A zero value (all empty) matches the whole task (no session filter).
type ActivitySessionFilter struct {
	Worker  string
	NodeID  *int64
	Main    bool // main-agent session
	MainSeg *int // segment for the main session (nil == current, resolved by caller; 0 == original)
}

func (f ActivitySessionFilter) cond(argStart int) (string, []any) {
	switch {
	case f.NodeID != nil:
		return fmt.Sprintf(" AND node_id=$%d", argStart), []any{*f.NodeID}
	case f.Main:
		seg := 0
		if f.MainSeg != nil {
			seg = *f.MainSeg
		}
		if seg == 0 { // original segment also owns legacy rows with NULL main_seg
			return " AND worker='mainagent' AND COALESCE(main_seg,0)=0", nil
		}
		return fmt.Sprintf(" AND worker='mainagent' AND main_seg=$%d", argStart), []any{seg}
	case f.Worker != "":
		return fmt.Sprintf(" AND worker=$%d", argStart), []any{f.Worker}
	default:
		return "", nil
	}
}

// ActivityPage returns one page for reverse (newest-first) pagination of a session:
// up to `limit` steps ending before id `before` (exclusive; before<=0 = the latest
// page), returned in ASCENDING id order for direct display. hasMore reports whether
// still-older steps exist before the returned window (so the client can stop loading
// on scroll-up). Summary-only columns — detail is lazy-loaded via ActivityDetail.
func (s *ExplorationStore) ActivityPage(f ActivitySessionFilter, before int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	const cols = `id, node_id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), metadata, created_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg`
	args := []any{s.expID}
	cond, cargs := f.cond(len(args) + 1)
	args = append(args, cargs...)
	beforeArg := len(args) + 1
	args = append(args, before)
	limitArg := len(args) + 1
	args = append(args, limit+1) // one extra row to detect older history
	q := fmt.Sprintf(`SELECT `+cols+`
FROM activity WHERE exploration_id=$1%s AND ($%d <= 0 OR id < $%d)
ORDER BY id DESC LIMIT $%d`, cond, beforeArg, beforeArg, limitArg)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	desc := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.Metadata, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens, &a.MainSeg); err != nil {
			return nil, false, err
		}
		desc = append(desc, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	// reverse the newest-first window into ascending id order for display.
	out := make([]Activity, len(desc))
	for i, a := range desc {
		out[len(desc)-1-i] = a
	}
	return out, hasMore, nil
}

// ActivityPageForTerminalIntent is the inherited session-history boundary. It
// combines ownership, terminal-state and row-kind checks in one query, closing
// the race where a previously completed source intent is reopened before paging.
func (s *ExplorationStore) ActivityPageForTerminalIntent(nodeID, before int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''),
		COALESCE(a.tool,''), COALESCE(a.tool_use_id,''), a.is_error, COALESCE(a.summary,''),
		a.created_at, a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cache_write_tokens
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2
	  AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	  AND ($3 <= 0 OR a.id < $3)
	ORDER BY a.id DESC LIMIT $4`, s.expID, nodeID, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	desc := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens); err != nil {
			return nil, false, err
		}
		desc = append(desc, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	out := make([]Activity, len(desc))
	for i, a := range desc {
		out[len(desc)-1-i] = a
	}
	return out, hasMore, nil
}

// ActivityMaxID returns the task's current maximum activity id (0 when empty). It is
// the task-level snapshot cursor handed to the SSE stream so history (id<=cursor) and
// the live tail (id>cursor) meet with no gap — deliberately task-level, not per
// session, since one SSE covers the whole task.
func (s *ExplorationStore) ActivityMaxID() (int64, error) {
	var max sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(id) FROM activity WHERE exploration_id=$1`, s.expID).Scan(&max)
	if err != nil {
		return 0, err
	}
	return max.Int64, nil
}

// MainSession is one resettable main-agent conversation segment of a task. Segment 0
// is the original session (implicit, never stored); further segments are created by
// "新建会话" to start the main agent on a clean transcript while the task's graph,
// assets and goal stay shared.
type MainSession struct {
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
}

// CurrentMainSeg returns the highest main-session segment (0 when none created yet).
func (s *ExplorationStore) CurrentMainSeg() (int, error) {
	var seg int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM main_sessions WHERE exploration_id=$1`, s.expID).Scan(&seg)
	return seg, err
}

// ListMainSessions returns every main-session segment newest-first, always including
// the implicit original segment 0. Segment 0 carries no stored timestamp.
func (s *ExplorationStore) ListMainSessions() ([]MainSession, error) {
	rows, err := s.db.Query(`SELECT seq, created_at FROM main_sessions WHERE exploration_id=$1 ORDER BY seq DESC`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MainSession{}
	for rows.Next() {
		var m MainSession
		if err := rows.Scan(&m.Seq, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out = append(out, MainSession{Seq: 0}) // implicit original session (oldest)
	return out, nil
}

// NewMainSession creates the next main-session segment (seq = current+1) and returns
// it. It touches only main_sessions — the task's exploration graph/assets/goal are
// untouched, so the new session starts with a clean transcript over the same task.
func (s *ExplorationStore) NewMainSession() (MainSession, error) {
	var m MainSession
	err := s.db.QueryRow(`
INSERT INTO main_sessions(exploration_id, seq)
VALUES ($1, COALESCE((SELECT MAX(seq) FROM main_sessions WHERE exploration_id=$1),0)+1)
RETURNING seq, created_at`, s.expID).Scan(&m.Seq, &m.CreatedAt)
	return m, err
}

// ActivityDetail lazily returns the full detail blob for one step.
func (s *ExplorationStore) ActivityDetail(id int64) (string, error) {
	var d sql.NullString
	err := s.db.QueryRow(`SELECT detail FROM activity WHERE id=$1 AND exploration_id=$2`, id, s.expID).Scan(&d)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return d.String, err
}
