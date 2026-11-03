package db

import (
	"database/sql"
	"sort"
	"time"
)

// TokenDailyAll aggregates token consumption by calendar day (UTC) for the
// past `days` days across all explorations. Only kind='result' rows carry token
// counts (the terminal per-run summary), so there is no double-counting.
func (d *DB) TokenDailyAll(days int) ([]DailyTokenBucket, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.Query(`
		SELECT TO_CHAR(DATE_TRUNC('day', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0)
		FROM activity
		WHERE kind = 'result'
		  AND created_at >= NOW() - ($1 * INTERVAL '1 day')
		GROUP BY day
		ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyTokenBucket
	for rows.Next() {
		var b DailyTokenBucket
		if err := rows.Scan(&b.Day, &b.InputTokens, &b.OutputTokens, &b.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type tokenUsageBucket struct {
	Day   time.Time
	Usage TokenUsage
}

// intentTokenRollup returns exactly-once usage grouped by its original UTC day.
// Each result terminates one run. A result carrying usage is authoritative; an
// older error/cancel result without usage falls back to the latest cumulative
// usage frame since the previous result. A trailing frame represents a run that
// was interrupted before its terminal result could be persisted.
func intentTokenRollup(tx *sql.Tx, explorationID, intentID int64) ([]tokenUsageBucket, error) {
	rows, err := tx.Query(`SELECT kind, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at
		FROM activity
		WHERE exploration_id=$1 AND node_id=$2 AND kind IN ('usage','result')
		ORDER BY id`, explorationID, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[time.Time]TokenUsage)
	add := func(at time.Time, usage TokenUsage) {
		if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
			usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
			return
		}
		utc := at.UTC()
		day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		total := byDay[day]
		total.add(usage)
		byDay[day] = total
	}

	var pending TokenUsage
	var pendingAt time.Time
	hasPending := false
	for rows.Next() {
		var kind string
		var input, output, read, write sql.NullInt64
		var createdAt time.Time
		if err := rows.Scan(&kind, &input, &output, &read, &write, &createdAt); err != nil {
			return nil, err
		}
		current := TokenUsage{
			InputTokens:      int(input.Int64),
			OutputTokens:     int(output.Int64),
			CacheReadTokens:  int(read.Int64),
			CacheWriteTokens: int(write.Int64),
		}
		hasCurrent := input.Valid || output.Valid || read.Valid || write.Valid
		if kind == "usage" {
			if hasCurrent {
				pending, pendingAt, hasPending = current, createdAt, true
			}
			continue
		}

		if hasCurrent {
			// Preserve a partially populated legacy terminal row by filling only its
			// missing dimensions from the latest cumulative usage frame.
			if hasPending {
				if !input.Valid {
					current.InputTokens = pending.InputTokens
				}
				if !output.Valid {
					current.OutputTokens = pending.OutputTokens
				}
				if !read.Valid {
					current.CacheReadTokens = pending.CacheReadTokens
				}
				if !write.Valid {
					current.CacheWriteTokens = pending.CacheWriteTokens
				}
			}
			add(createdAt, current)
		} else if hasPending {
			add(createdAt, pending)
		}
		pending, pendingAt, hasPending = TokenUsage{}, time.Time{}, false
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if hasPending {
		add(pendingAt, pending)
	}

	buckets := make([]tokenUsageBucket, 0, len(byDay))
	for day, usage := range byDay {
		buckets = append(buckets, tokenUsageBucket{Day: day, Usage: usage})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Day.Before(buckets[j].Day) })
	return buckets, nil
}

func (u *TokenUsage) add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

// TokenTotal sums token usage across ALL workers for this exploration (whole-task
// total). Same source as TokenStatsByWorker (kind='result' rows), just ungrouped.
func (s *ExplorationStore) TokenTotal() (TokenUsage, error) {
	var u TokenUsage
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0)
	FROM activity WHERE exploration_id=$1 AND kind='result'`, s.expID).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens)
	return u, err
}

// TokenTotalsAll returns the whole-task token total for every exploration in one
// query (exploration_id → total), so the task list can show per-task consumption
// without a query per row.
func (d *DB) TokenTotalsAll() (map[int64]TokenUsage, error) {
	rows, err := d.Query(`SELECT exploration_id,
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0)
	FROM activity WHERE kind='result' GROUP BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]TokenUsage{}
	for rows.Next() {
		var eid int64
		var u TokenUsage
		if err := rows.Scan(&eid, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens); err != nil {
			return nil, err
		}
		out[eid] = u
	}
	return out, rows.Err()
}

// TokenStatsByWorker sums token usage per worker for this exploration (from the
// kind='result' records that carry usage). Used by the per-agent token display.
func (s *ExplorationStore) TokenStatsByWorker() ([]TokenUsage, error) {
	rows, err := s.db.Query(`SELECT COALESCE(worker,''),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0)
	FROM activity WHERE exploration_id=$1 AND kind='result'
	GROUP BY worker ORDER BY worker`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenUsage{}
	for rows.Next() {
		var u TokenUsage
		if err := rows.Scan(&u.Worker, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TokenStatsBySession aggregates every persisted completed run, independent of
// activity-history pagination. Main/planner use fixed keys; Worker executions use
// their intent node id, even when a different work#N process handled a retry.
func (s *ExplorationStore) TokenStatsBySession() ([]SessionTokenUsage, error) {
	rows, err := s.db.Query(`SELECT
		CASE
			WHEN COALESCE(a.worker,'')='mainagent' THEN 'main:' || COALESCE(a.main_seg,0)::text
			WHEN COALESCE(a.worker,'')='planner' THEN 'plan'
			WHEN n.id IS NOT NULL THEN 'intent:' || n.id::text
			ELSE ''
		END AS session_key,
		COALESCE(SUM(a.input_tokens),0), COALESCE(SUM(a.output_tokens),0),
		COALESCE(SUM(a.cache_read_tokens),0), COALESCE(SUM(a.cache_write_tokens),0)
	FROM activity a
	LEFT JOIN exploration_nodes n
	  ON n.id=a.node_id AND n.exploration_id=a.exploration_id AND n.kind='intent'
	WHERE a.exploration_id=$1 AND a.kind='result'
	  AND (COALESCE(a.worker,'') IN ('mainagent','planner') OR n.id IS NOT NULL)
	GROUP BY session_key
	ORDER BY session_key`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionTokenUsage{}
	for rows.Next() {
		var u SessionTokenUsage
		if err := rows.Scan(&u.Session, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
