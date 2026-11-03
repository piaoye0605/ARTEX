package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// scanTrace reads the summary-only column set shared by the worker-trace tools
// (ActivityTrace / ActivityTraceSearch). Detail is never selected here — it is
// pulled separately, on demand, via ActivityByIDs.
func scanTrace(rows *sql.Rows) ([]Activity, error) {
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.IsError, &a.Summary); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const traceCols = `id, node_id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), is_error, COALESCE(summary,'')`

// ActivityTrace lists one work's steps (by intent/node id) for the trace tools:
// summaries only (detail is lazy — fetch via ActivityByIDs). thinking/usage rows
// are dropped so the caller sees the work's actions + observations, not the
// model's internal reasoning or token-accounting noise.
func (s *ExplorationStore) ActivityTrace(nodeID int64, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')
ORDER BY id LIMIT $3`, s.expID, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceForTerminalIntent returns one inherited work trace only while its
// source intent is still terminal at query time.
func (s *ExplorationStore) ActivityTraceForTerminalIntent(nodeID int64, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	ORDER BY a.id LIMIT $3`, s.expID, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceSearch finds steps whose summary OR detail matches q
// (case-insensitive), returning summaries only. nodeID != nil scopes to one work;
// nil searches across ALL works (node_id IS NOT NULL — planner/main steps have no
// node_id, so this cleanly means "worker traces"). thinking/usage rows dropped.
func (s *ExplorationStore) ActivityTraceSearch(nodeID *int64, q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	var rows *sql.Rows
	var err error
	if nodeID != nil {
		rows, err = s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')
AND (summary ILIKE $3 OR detail ILIKE $3) ORDER BY id LIMIT $4`, s.expID, *nodeID, like, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id IS NOT NULL AND kind NOT IN ('thinking','usage')
AND (summary ILIKE $2 OR detail ILIKE $2) ORDER BY id LIMIT $3`, s.expID, like, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceSearchForTerminalIntent is the scoped inherited search variant;
// terminal eligibility is evaluated by the same statement that reads the rows.
func (s *ExplorationStore) ActivityTraceSearchForTerminalIntent(nodeID int64, q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	  AND (a.summary ILIKE $3 OR a.detail ILIKE $3)
	ORDER BY a.id LIMIT $4`, s.expID, nodeID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceSearchTerminalIntents searches inherited worker history without
// exposing planner/main rows or work that is still open/running in the source.
func (s *ExplorationStore) ActivityTraceSearchTerminalIntents(q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	  AND (a.summary ILIKE $2 OR a.detail ILIKE $2)
	ORDER BY a.id LIMIT $3`, s.expID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// AssetRef is a compact exploration node (intent/fact/finding) that references an
// asset via an anchor — for the coverage graph's node drawer.
type AssetRef struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Summary      string `json:"summary"`
	SourceTaskID int64  `json:"source_task_id,omitempty"`
	Inherited    bool   `json:"inherited,omitempty"`
}

// AssetRefs returns the intents / facts / findings in THIS exploration anchored to
// the given asset id (newest first) — what this task tested / concluded about it.
func (s *ExplorationStore) AssetRefs(assetID int64) ([]AssetRef, error) {
	if assetID <= 0 {
		return []AssetRef{}, nil
	}
	rows, err := s.db.Query(`
SELECT en.id, en.kind, en.state, en.payload
FROM exploration_anchors ea
JOIN exploration_nodes en ON en.id = ea.node_id
WHERE ea.asset_id = $1 AND en.exploration_id = $2 AND en.kind IN ('intent','fact','finding')
ORDER BY en.id DESC`, assetID, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetRef{}
	for rows.Next() {
		var r AssetRef
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Kind, &r.State, &payload); err != nil {
			return nil, err
		}
		var p map[string]any
		_ = json.Unmarshal(payload, &p)
		for _, k := range []string{"summary", "text"} {
			if v, ok := p[k].(string); ok && strings.TrimSpace(v) != "" {
				r.Summary = v
				break
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActivityTraceSearchExcluding keyword-searches every node's steps in this
// exploration EXCEPT one node's own (excludeNodeID) — so a worker's
// search_all_worker_traces doesn't return its own in-progress trace (which is
// already in its context). excludeNodeID<=0 → no exclusion (searches all nodes).
func (s *ExplorationStore) ActivityTraceSearchExcluding(excludeNodeID int64, q string, limit int) ([]Activity, error) {
	if excludeNodeID <= 0 {
		return s.ActivityTraceSearch(nil, q, limit)
	}
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id IS NOT NULL AND node_id <> $2
AND kind NOT IN ('thinking','usage')
AND (summary ILIKE $3 OR detail ILIKE $3) ORDER BY id LIMIT $4`, s.expID, excludeNodeID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityByIDs loads full detail for specific step ids (the trace drill-down),
// scoped to this exploration. thinking rows are skipped (never returned, even if
// their id is asked for). Order is ascending id, not the input order.
func (s *ExplorationStore) ActivityByIDs(ids []int64) ([]Activity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids)+1)
	args[0] = s.expID
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args[i+1] = id
	}
	rows, err := s.db.Query(`SELECT id, node_id, COALESCE(kind,''), COALESCE(tool,''), is_error, COALESCE(detail,'')
FROM activity WHERE exploration_id=$1 AND kind<>'thinking' AND id IN (`+strings.Join(ph, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Kind, &a.Tool, &a.IsError, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActivityByIDsForTerminalIntents is the inherited-detail read boundary. It
// deliberately excludes node-less planner/main rows, non-intent activities, live
// source work, and thinking. Local callers that need legacy behavior use
// ActivityByIDs instead.
func (s *ExplorationStore) ActivityByIDsForTerminalIntents(ids []int64) ([]Activity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids)+1)
	args[0] = s.expID
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args[i+1] = id
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.detail,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
		WHERE a.exploration_id=$1 AND a.kind NOT IN ('thinking','usage') AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.id IN (`+strings.Join(ph, ",")+`) ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Kind, &a.Tool, &a.IsError, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
