package db

// ExplorationDiag probes, at the moment of an activity FK violation (23503), why the
// parent row is unreachable. It reports whether the exploration row still exists, how
// many task rows reference it (a live task should keep exactly 1 — and RESTRICT on
// tasks.exploration_id means the exploration CANNOT be deleted while that row lives),
// and the current MAX(explorations.id). Together these tell apart the failure modes:
//   - expExists=false, taskRefs=0 → the whole row set is gone (DB reset / wrong DB).
//   - expExists=false, taskRefs=1 → impossible under RESTRICT; would mean a broken FK.
//   - expExists=true              → the write's expID is NOT this exploration (stale/
//     wrong id in the in-memory store); compare against Store.ID().
func (d *DB) ExplorationDiag(expID int64) (expExists bool, taskRefs int, maxExpID int64, err error) {
	err = d.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM explorations WHERE id=$1),
		(SELECT COUNT(*) FROM tasks WHERE exploration_id=$1),
		COALESCE((SELECT MAX(id) FROM explorations),0)`, expID).Scan(&expExists, &taskRefs, &maxExpID)
	return
}

// LastActivityAll returns the unix time of the most recent activity per
// exploration (exploration_id → max created_at epoch), one query for all tasks.
// Persisted (unlike Engine.LastActivity's in-memory map), so it survives restarts
// and gives终态任务 a stable "ran until" time for computing run duration.
func (d *DB) LastActivityAll() (map[int64]int64, error) {
	rows, err := d.Query(`SELECT exploration_id, EXTRACT(EPOCH FROM MAX(created_at))::bigint FROM activity GROUP BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var eid, ts int64
		if err := rows.Scan(&eid, &ts); err != nil {
			return nil, err
		}
		out[eid] = ts
	}
	return out, rows.Err()
}

// GoalCounts is the goal summary for one exploration.
type GoalCounts struct{ Total, Met int }

// FindingSeverityCounts breaks a task's findings down by severity for the task
// list (severity 白名单外/为空的记录不计入任一档)。
type FindingSeverityCounts struct{ Critical, High, Medium, Low int }

// TaskListMetrics contains the aggregates rendered in task lists.
type TaskListMetrics struct {
	Tokens         TokenUsage
	LastActivity   int64
	Goals          GoalCounts
	RunningIntents int                   // kind='intent' 且 state='running' 的条数，即运行中 Worker 数
	Findings       FindingSeverityCounts // findings 表里该任务的漏洞数（按严重度分档）
}

// TaskListMetricsAll returns list aggregates for every live task in one query.
// The lateral lookups use the per-exploration indexes instead of grouping the
// complete activity history on every poll.
func (d *DB) TaskListMetricsAll() (map[int64]TaskListMetrics, error) {
	rows, err := d.Query(`
		SELECT task.exploration_id,
		       COALESCE(token_metrics.input_tokens,0),
		       COALESCE(token_metrics.output_tokens,0),
		       COALESCE(token_metrics.cache_read_tokens,0),
		       COALESCE(token_metrics.cache_write_tokens,0),
		       COALESCE(latest_activity.created_at,0),
		       COALESCE(goal_metrics.total,0),
		       COALESCE(goal_metrics.met,0),
		       COALESCE(intent_metrics.running,0),
		       COALESCE(finding_metrics.critical,0),
		       COALESCE(finding_metrics.high,0),
		       COALESCE(finding_metrics.medium,0),
		       COALESCE(finding_metrics.low,0)
		FROM tasks task
		LEFT JOIN LATERAL (
			SELECT SUM(input_tokens) AS input_tokens,
			       SUM(output_tokens) AS output_tokens,
			       SUM(cache_read_tokens) AS cache_read_tokens,
			       SUM(cache_write_tokens) AS cache_write_tokens
			FROM activity
			WHERE exploration_id=task.exploration_id AND kind='result'
		) token_metrics ON true
		LEFT JOIN LATERAL (
			SELECT EXTRACT(EPOCH FROM created_at)::bigint AS created_at
			FROM activity
			WHERE exploration_id=task.exploration_id
			ORDER BY created_at DESC
			LIMIT 1
		) latest_activity ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS total,
			       COUNT(*) FILTER (WHERE state='met') AS met
			FROM exploration_nodes
			WHERE exploration_id=task.exploration_id AND kind='goal'
		) goal_metrics ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS running
			FROM exploration_nodes
			WHERE exploration_id=task.exploration_id AND kind='intent' AND state='running'
		) intent_metrics ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) FILTER (WHERE severity='critical') AS critical,
			       COUNT(*) FILTER (WHERE severity='high')     AS high,
			       COUNT(*) FILTER (WHERE severity='medium')   AS medium,
			       COUNT(*) FILTER (WHERE severity='low')      AS low
			FROM findings
			WHERE task_id=task.id
		) finding_metrics ON true
		WHERE task.deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]TaskListMetrics{}
	for rows.Next() {
		var explorationID int64
		var metrics TaskListMetrics
		if err := rows.Scan(
			&explorationID,
			&metrics.Tokens.InputTokens,
			&metrics.Tokens.OutputTokens,
			&metrics.Tokens.CacheReadTokens,
			&metrics.Tokens.CacheWriteTokens,
			&metrics.LastActivity,
			&metrics.Goals.Total,
			&metrics.Goals.Met,
			&metrics.RunningIntents,
			&metrics.Findings.Critical,
			&metrics.Findings.High,
			&metrics.Findings.Medium,
			&metrics.Findings.Low,
		); err != nil {
			return nil, err
		}
		out[explorationID] = metrics
	}
	return out, rows.Err()
}

// GoalCountsAll returns goal totals (total / met) per exploration id, one query
// for all tasks — used by listTasks so the task list shows progress for every task.
func (d *DB) GoalCountsAll() (map[int64]GoalCounts, error) {
	rows, err := d.Query(`
		SELECT exploration_id,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE state = 'met') AS met
		FROM exploration_nodes
		WHERE kind = 'goal'
		GROUP BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]GoalCounts{}
	for rows.Next() {
		var eid int64
		var gc GoalCounts
		if err := rows.Scan(&eid, &gc.Total, &gc.Met); err != nil {
			return nil, err
		}
		out[eid] = gc
	}
	return out, rows.Err()
}
