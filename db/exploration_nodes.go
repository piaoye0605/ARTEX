package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// AddNode writes a typed reasoning node with optional anchors to asset ids.
func (s *ExplorationStore) AddNode(kind string, payload map[string]any, priority int, state, origin string, anchors []int64) (int64, error) {
	raw, _ := json.Marshal(payload)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		s.expID, kind, string(raw), priority, state, origin).Scan(&id); err != nil {
		return 0, err
	}
	for _, a := range anchors {
		if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, a); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// AddIntent is a convenience: an open intent.
func (s *ExplorationStore) AddIntent(payload map[string]any, priority int, anchors []int64, origin string) (int64, error) {
	if origin == "" {
		origin = "planner"
	}
	return s.AddNode("intent", payload, priority, "open", origin, anchors)
}

// AddGoal writes a goal node (state open).
func (s *ExplorationStore) AddGoal(payload map[string]any, origin string) (int64, error) {
	return s.AddNode("goal", payload, 0, "open", origin, nil)
}

// OriginFactID returns this exploration's root fact id — the KindFact node with
// state='origin' seeded at task creation (the task root). Every intent traces
// back to it. Returns 0 (no error) when none exists (legacy explorations created
// under the old 'begin' root, or none yet).
func (s *ExplorationStore) OriginFactID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND state='origin' ORDER BY id LIMIT 1`, s.expID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// Anchor records a node→asset reference edge (many-to-many): it captures which
// global assets an exploration node touched, as lineage/provenance. The asset
// graph itself is global and shared — anchoring no longer gates visibility (any
// task may read any asset via search); it only preserves the reasoning trail.
// Idempotent.
func (s *ExplorationStore) Anchor(nodeID, assetID int64) error {
	if nodeID <= 0 || assetID <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, nodeID, assetID)
	return err
}

// Link adds a typed exploration edge (idempotent).
func (s *ExplorationStore) Link(from int64, rel string, to int64) error {
	_, err := s.db.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id) VALUES ($1,$2,$3,$4)
ON CONFLICT (exploration_id, src_id, rel, dst_id) DO NOTHING`, s.expID, from, rel, to)
	return err
}

// SetNodeState updates any node's state (never deletes). content_version bumps
// so a folded member's state flip invalidates its digest's cached body (§5.3).
func (s *ExplorationStore) SetNodeState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE exploration_nodes SET state=$1, blocked_reason=NULL, content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, state, id, s.expID)
	return err
}

// UpdateGoalPayload rewrites a goal node's payload text (and optional vulnclass);
// scoped to kind='goal' so it can never mutate an intent/fact by id. Returns an
// error if no such goal exists in this exploration.
func (s *ExplorationStore) UpdateGoalPayload(id int64, text, vulnclass string) error {
	payload := map[string]any{"text": text}
	if vulnclass != "" {
		payload["vulnclass"] = vulnclass
	}
	raw, _ := json.Marshal(payload)
	res, err := s.db.Exec(`UPDATE exploration_nodes SET payload=$1 WHERE id=$2 AND exploration_id=$3 AND kind='goal'`, string(raw), id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("目标不存在")
	}
	return nil
}

// DeleteGoal hard-deletes a goal node; scoped to kind='goal' so it can never drop
// an intent/fact by id. The edges (spawns) and anchors reference it with ON DELETE
// CASCADE, so they go with it; activity.node_id is ON DELETE SET NULL. Returns an
// error if no such goal exists in this exploration.
func (s *ExplorationStore) DeleteGoal(id int64) error {
	res, err := s.db.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal'`, id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("目标不存在")
	}
	return nil
}

const nodeCols = `id, kind, payload, priority, state, COALESCE(origin,''), COALESCE(owner,''), COALESCE(blocked_reason,''), COALESCE(delete_reason,''), created_at`

func scanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var payload []byte
	if err := sc.Scan(&n.ID, &n.Kind, &payload, &n.Priority, &n.State, &n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason, &n.CreatedAt); err != nil {
		return nil, err
	}
	n.Payload = json.RawMessage(payload)
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]*Node, error) {
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListByKind lists nodes of a kind (newest first).
func (s *ExplorationStore) ListByKind(kind string, limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind=$2 ORDER BY id DESC LIMIT $3`, s.expID, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListByKindPage returns one newest-first page of nodes of a kind for reverse
// pagination: up to `limit` nodes with id < `before` (before<=0 = the newest page).
// hasMore reports whether still-older nodes exist, so a client (e.g. the worker
// session list) can page past the old fixed cap instead of losing older intents.
func (s *ExplorationStore) ListByKindPage(kind string, before int64, limit int) ([]*Node, bool, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind=$2 AND ($3 <= 0 OR id < $3)
ORDER BY id DESC LIMIT $4`, s.expID, kind, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(nodes) > limit
	if hasMore {
		nodes = nodes[:limit]
	}
	return nodes, hasMore, nil
}

// listByKindPageFiltered is ListByKindPage with an optional summary keyword
// filter (payload->>'summary' ILIKE %q%). Fetches one extra row so the caller can
// probe hasMore. Empty q = no filter. Newest-first by id.
func (s *ExplorationStore) listByKindPageFiltered(kind string, before int64, limit int, q string) ([]*Node, error) {
	where := `exploration_id=$1 AND kind=$2 AND ($3 <= 0 OR id < $3)`
	args := []any{s.expID, kind, before}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND payload->>'summary' ILIKE $%d`, len(args))
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE `+where+` ORDER BY id DESC LIMIT $`+fmt.Sprintf("%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// countByKindFiltered counts nodes of a kind under the same summary filter, for a
// page's total. Empty q = count all of that kind.
func (s *ExplorationStore) countByKindFiltered(kind, q string) (int, error) {
	where := `exploration_id=$1 AND kind=$2`
	args := []any{s.expID, kind}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND payload->>'summary' ILIKE $%d`, len(args))
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE `+where, args...).Scan(&n)
	return n, err
}

// CountFinishedIntents counts this exploration's intents in a terminal explored
// state (done/blocked/exhausted) — graph_overview's done_intents_total, so the planner
// knows recent_done_intents (capped at a recent window) is a truncated view and
// stays cautious about "already tried" dedup. Excludes 'stopped' (killed/deleted),
// matching exactly what recent_done_intents surfaces.
func (s *ExplorationStore) CountFinishedIntents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state IN ('done','blocked','exhausted')`, s.expID).Scan(&n)
	return n, err
}

// CountOpenIntents counts this exploration's open intents — graph_overview's
// frontier_open, so the planner knows open_intents (capped at the top-N by
// priority) is a truncated view and there may be more claimable work.
func (s *ExplorationStore) CountOpenIntents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open'`, s.expID).Scan(&n)
	return n, err
}

// GetNode returns one node of this exploration by id (nil, nil if not found).
func (s *ExplorationStore) GetNode(id int64) (*Node, error) {
	n, err := scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, id, s.expID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return n, err
}

// Nodes lists all nodes (for graph viz).
func (s *ExplorationStore) Nodes(limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 ORDER BY id LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// NodeFilter narrows a NodesPage query. Empty fields include every value.
type NodeFilter struct {
	Kinds  []string // exploration_nodes.kind
	States []string // exploration_nodes.state
	Query  string   // case-insensitive substring over payload text / origin
	Asc    bool     // true = oldest first (replay); default newest first (broadcast)
}

// NodesPage returns one 1-based page of this exploration's nodes plus the total
// matching count. The 播报板 reads the graph as a time series, so it pages in SQL
// rather than pulling the whole graph like Nodes does. Ordering is by id, which
// is BIGSERIAL and therefore creation order — stable when several nodes share a
// created_at second.
func (s *ExplorationStore) NodesPage(f NodeFilter, page, size int) ([]*Node, int, error) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	offset := (page - 1) * size

	conds := []string{"exploration_id=$1"}
	args := []any{s.expID}
	addIn := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		marks := make([]string, 0, len(values))
		for _, v := range values {
			args = append(args, v)
			marks = append(marks, "$"+fmt.Sprint(len(args)))
		}
		conds = append(conds, column+" IN ("+strings.Join(marks, ",")+")")
	}
	addIn("kind", f.Kinds)
	addIn("state", f.States)
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		mark := "$" + fmt.Sprint(len(args))
		ors := []string{"payload::text ILIKE " + mark, "COALESCE(origin,'') ILIKE " + mark}
		// 纯数字(或 UI 里带 # 前缀的形式,如「#41」)当作节点 id 精确匹配,方便直接定位某个节点。
		if idStr := strings.TrimPrefix(q, "#"); idStr != "" {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				args = append(args, id)
				ors = append(ors, "id = $"+fmt.Sprint(len(args)))
			}
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "DESC"
	if f.Asc {
		order = "ASC"
	}
	args = append(args, size, offset)
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes`+where+
		` ORDER BY id `+order+
		` LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, 0, err
	}
	return nodes, total, nil
}

// NodesByIDs loads the given nodes of this exploration in id order. Used to
// resolve the neighbours of a 播报板 page without fetching the whole graph.
func (s *ExplorationStore) NodesByIDs(ids []int64) ([]*Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, s.expID)
	marks := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		marks = append(marks, "$"+fmt.Sprint(len(args)))
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND id IN (`+strings.Join(marks, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// AddStandaloneFinding writes a finding to the standalone findings table, which
// persists across task deletion (task_id / node_id become NULL when the task or
// exploration node is deleted). taskID and nodeID may be 0 (stored as NULL).
func (s *ExplorationStore) AddStandaloneFinding(taskID, nodeID int64, vulnclass, name, severity, summary, evidence, worker string, assetIDs []int64) (int64, error) {
	return s.db.AddFinding(taskID, nodeID, vulnclass, name, severity, summary, evidence, worker, assetIDs)
}
