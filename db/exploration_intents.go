package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SetIntentState updates an intent node's state.
// ResetRunningIntents returns any intent left in 'running' back to 'open' so it
// is re-claimed. Called on startup: a 'running' intent with no live worker (a
// backend restart or crashed worker goroutine left it stuck) would otherwise spin
// forever in the UI. Workers resume from their transcript, so reopening is safe.
func (s *ExplorationStore) ResetRunningIntents() (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='running'`, s.expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReopenIntent flips ONE not-successfully-finished intent (blocked/exhausted/stopped)
// back to 'open' so a worker re-claims it (graph writes it already produced stay).
// The worker resumes from its prior LLM transcript instead of restarting from scratch.
// done/open/running are left untouched. Returns whether a row changed. Used by "重跑".
func (s *ExplorationStore) ReopenIntent(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE id=$1 AND exploration_id=$2 AND kind='intent'
  AND state IN ('blocked','exhausted','stopped')`, id, s.expID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReopenBlockedIntents flips EVERY 'blocked' intent in this exploration back to 'open'
// (batch rerun after e.g. an LLM/network outage that blocked many at once). Returns the
// number reopened. Workers resume from their transcripts; kept graph writes remain.
func (s *ExplorationStore) ReopenBlockedIntents() (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='blocked'`, s.expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *ExplorationStore) SetIntentState(id int64, state string) error {
	// terminal states stamp completed_at; reopening (back to open/running) clears it.
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	_, err := s.db.Exec(`UPDATE exploration_nodes
SET state=$1, blocked_reason=NULL, content_version=content_version+1, completed_at = CASE WHEN $4 THEN now() ELSE NULL END
WHERE id=$2 AND exploration_id=$3 AND kind='intent'`, state, id, s.expID, terminal)
	return err
}

// CompareAndSetIntentState transitions one local intent only when its current
// state still matches expected. It is the state boundary used by pause/resume and
// worker settlement, so a stale API read cannot overwrite a concurrent finish.
func (s *ExplorationStore) CompareAndSetIntentState(id int64, expected, state string) (bool, error) {
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state=$1, blocked_reason=NULL, content_version=content_version+1, completed_at = CASE WHEN $5 THEN now() ELSE NULL END
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state=$4`, state, id, s.expID, expected, terminal)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IntentCleanup summarizes the blackboard records removed when a user cancels
// one worker intent. Global assets and traffic are deliberately not part of this
// operation; exploration anchors disappear only because their owning nodes do.
type IntentCleanup struct {
	Intents    int64 `json:"intents"`
	Facts      int64 `json:"facts"`
	Findings   int64 `json:"findings"`
	Activities int64 `json:"activities"`
}

// SoftDeleteIntent 假删除一个待领/运行中/已暂停的意图:置 state='deleted' 并把用户填写的
// 删除原因记入 delete_reason 字段,保留意图节点及其全部产出/血缘(不再像旧实现那样在图上
// 另挂一条 fact)。返回删除前意图的 summary,供 planner 通知使用。副会话随删除态一并清理。
// 调用方须先停掉运行中的 worker,避免其后续写入。
func (s *ExplorationStore) SoftDeleteIntent(id int64, reason string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var state string
	var rawPayload []byte
	if err := tx.QueryRow(`SELECT state, payload FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(&state, &rawPayload); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("intent not found")
		}
		return "", err
	}
	if state != "running" && state != "paused" && state != "open" {
		return "", fmt.Errorf("%w: intent state %s cannot be deleted", ErrIntentStateConflict, state)
	}
	if _, err := tx.Exec(`UPDATE exploration_nodes
		SET state='deleted', delete_reason=$3, blocked_reason=NULL,
		    content_version=content_version+1, completed_at=now()
		WHERE id=$1 AND exploration_id=$2`, id, s.expID, reason); err != nil {
		return "", err
	}
	// 主意图审计轨迹保留,但副问答会话随删除态原子清理(延迟快照会拒绝它)。
	if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE intent_id=$1`, id); err != nil {
		return "", err
	}
	var summary string
	var p map[string]any
	if json.Unmarshal(rawPayload, &p) == nil {
		if sm, ok := p["summary"].(string); ok {
			summary = sm
		}
	}
	return summary, tx.Commit()
}

// CancelIntent 物理删除一个意图,以及"仅由该意图支撑"的全部独占子孙节点——从该意图沿
// yields/derived_from 向下可达、且所有父节点(所有指向它的边的源)都落在删除集内的节点。
// goal 与 origin fact 永不删除;还被删除集之外的意图/digest 引用的共享节点也保留,以免破坏
// 其它分支、产生断链。被删的每个 intent 先做 token rollup(保留不可逆计量)并清理其 activity
// 与副会话;被删的 finding 先删 findings 表行(node_id FK 为 ON DELETE SET NULL,否则留孤儿)。
// 边随节点 CASCADE 清理,整个清理保持在一个事务内。调用方须先停掉运行中的 worker 防止其后续写入。
func (s *ExplorationStore) CancelIntent(id int64) (IntentCleanup, error) {
	var out IntentCleanup
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// 锁定意图行确认存在(幂等:已删则 not found)。状态不校验——真删除对任何状态成立,
	// running 的 worker 由调用方先停。
	if err := tx.QueryRow(`SELECT 1 FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(new(int)); err != nil {
		if err == sql.ErrNoRows {
			return out, fmt.Errorf("intent not found")
		}
		return out, err
	}

	// 载入全图节点(判 protected)与边(算向下可达 + 父集)。图规模对真实任务很小。
	kind := map[int64]string{}
	protected := map[int64]bool{}
	nrows, err := tx.Query(`SELECT id, kind, state FROM exploration_nodes WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return out, err
	}
	for nrows.Next() {
		var nid int64
		var k, st string
		if err := nrows.Scan(&nid, &k, &st); err != nil {
			nrows.Close()
			return out, err
		}
		kind[nid] = k
		if k == KindGoal || (k == KindFact && st == StateOrigin) {
			protected[nid] = true // 目标与任务根事实永不随意图删除
		}
	}
	nrows.Close()
	if err := nrows.Err(); err != nil {
		return out, err
	}

	parentsOf := map[int64][]int64{} // dst -> 所有指向它的边的 src(任意 rel,含 covers:被 digest 覆盖的成员因此有 digest 父而被保留)
	downOf := map[int64][]int64{}    // src -> 沿 yields/derived_from 的向下邻居
	erows, err := tx.Query(`SELECT src_id, rel, dst_id FROM exploration_edges WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return out, err
	}
	for erows.Next() {
		var src, dst int64
		var rel string
		if err := erows.Scan(&src, &rel, &dst); err != nil {
			erows.Close()
			return out, err
		}
		parentsOf[dst] = append(parentsOf[dst], src)
		if rel == RelYields || rel == RelDerivedFrom {
			downOf[src] = append(downOf[src], dst)
		}
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return out, err
	}

	// 独占级联:从意图向下扩展,一个节点入删除集当且仅当它未被保护、且它的每个父都已在集内
	// (即除了经过被删节点外再无来路)。迭代到不动点。
	del := map[int64]bool{id: true}
	for changed := true; changed; {
		changed = false
		for src := range del {
			for _, dst := range downOf[src] {
				if del[dst] || protected[dst] {
					continue
				}
				exclusive := true
				for _, p := range parentsOf[dst] {
					if !del[p] {
						exclusive = false
						break
					}
				}
				if exclusive {
					del[dst] = true
					changed = true
				}
			}
		}
	}

	// 按类型分桶。
	var ids, intentIDs, findingIDs []int64
	for nid := range del {
		ids = append(ids, nid)
		switch kind[nid] {
		case KindIntent:
			intentIDs = append(intentIDs, nid)
			out.Intents++
		case KindFact:
			out.Facts++
		case KindFinding:
			findingIDs = append(findingIDs, nid)
			out.Findings++
		}
	}

	// 每个被删意图:token rollup(保留不可逆计量)后清 activity。rollup 行 node_id 为 NULL,
	// 不会被下面按 node_id 的删除命中。
	for _, iid := range intentIDs {
		tokenBuckets, err := intentTokenRollup(tx, s.expID, iid)
		if err != nil {
			return out, err
		}
		res, err := tx.Exec(`DELETE FROM activity WHERE exploration_id=$1 AND node_id=$2`, s.expID, iid)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out.Activities += n
		}
		for _, bucket := range tokenBuckets {
			metadata, _ := json.Marshal(map[string]any{
				"cancelled_intent_id": iid,
				"token_day":           bucket.Day.Format(time.DateOnly),
				"token_rollup":        true,
			})
			if _, err := tx.Exec(`INSERT INTO activity(
				exploration_id, worker, kind, summary, metadata,
				input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at)
				VALUES ($1,'token-ledger','result',$2,$3,$4,$5,$6,$7,$8)`,
				s.expID, fmt.Sprintf("已取消意图 #%d 的 Token 计量", iid), metadata,
				bucket.Usage.InputTokens, bucket.Usage.OutputTokens,
				bucket.Usage.CacheReadTokens, bucket.Usage.CacheWriteTokens, bucket.Day); err != nil {
				return out, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE intent_id=$1`, iid); err != nil {
			return out, err
		}
	}

	// finding 节点删除前先删 findings 表行(否则留 node_id=NULL 的孤儿)。
	for _, fid := range findingIDs {
		if _, err := tx.Exec(`DELETE FROM findings WHERE node_id=$1`, fid); err != nil {
			return out, err
		}
	}

	// 删节点(边随 CASCADE 清理)。逐个删并核对行数,防并发改动。
	var removed int64
	for _, nid := range ids {
		res, err := tx.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, nid, s.expID)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += n
		}
	}
	if removed != int64(len(ids)) {
		return out, fmt.Errorf("intent cleanup changed concurrently")
	}
	return out, tx.Commit()
}
