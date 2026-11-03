package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) intents(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		// Back-compat: bare list shape when the task can't be resolved.
		writeJSON(w, 200, []any{})
		return
	}
	q := r.URL.Query()
	limit := min(atoiDefault(q.Get("limit"), 300), 500)
	// No paging params → preserve the legacy bare-array response so existing callers
	// (and the poll) keep working unchanged.
	if q.Get("before") == "" && q.Get("page") == "" {
		in, err := t.Store.ListByKind(db.KindIntent, limit)
		if err != nil {
			log.Printf("[intents] task=%s limit=%d: %v", t.ID, limit, err)
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, taskNodeDTOs(in))
		return
	}
	// Paged form: ?before=<id> (or ?page as a marker) → {items, has_more} so the
	// worker session list can reach past the old fixed 300 boundary on scroll. It
	// also includes immutable intent results from directly related tasks; those
	// rows never participate in this task's frontier or claim path.
	before := int64(atoiDefault(q.Get("before"), 0))
	in, hasMore, err := taskIntentHistoryPage(t.Store, before, limit, false, 0)
	if err != nil {
		log.Printf("[intents] task=%s before=%d limit=%d: %v", t.ID, before, limit, err)
		writeErr(w, 500, err.Error())
		return
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		items, sourceMore, sourceErr := taskIntentHistoryPage(source.Store, before, limit, true, source.Task.TaskID)
		if sourceErr != nil {
			writeErr(w, 500, sourceErr.Error())
			return
		}
		in = append(in, items...)
		hasMore = hasMore || sourceMore
	}
	sort.Slice(in, func(i, j int) bool { return in[i].ID > in[j].ID })
	if len(in) > limit {
		in = in[:limit]
		hasMore = true
	}
	writeJSON(w, 200, map[string]any{"items": taskNodeDTOs(in), "has_more": hasMore})
}

func inheritedIntentResult(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// inheritedGraphSnapshot exposes immutable source-task history without leaking
// work that is still open or running. Edges are filtered with the nodes so the
// response cannot contain dangling ids that reveal hidden live intent topology.
func inheritedGraphSnapshot(nodes []*db.Node, edges []db.Edge, sourceTaskID int64) ([]*db.Node, []db.Edge) {
	visible := make(map[int64]struct{}, len(nodes))
	filteredNodes := make([]*db.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || (node.Kind == db.KindIntent && !inheritedIntentResult(node.State)) {
			continue
		}
		node.SourceTaskID = sourceTaskID
		node.Inherited = true
		visible[node.ID] = struct{}{}
		filteredNodes = append(filteredNodes, node)
	}
	filteredEdges := make([]db.Edge, 0, len(edges))
	for _, edge := range edges {
		if _, ok := visible[edge.From]; !ok {
			continue
		}
		if _, ok := visible[edge.To]; !ok {
			continue
		}
		filteredEdges = append(filteredEdges, edge)
	}
	return filteredNodes, filteredEdges
}

// taskIntentHistoryPage returns one newest-first page for an exploration.
// A source may have many open/running intents, so keep paging until enough
// historical results are collected instead of leaking its frontier into the UI.
func taskIntentHistoryPage(store *db.ExplorationStore, before int64, limit int, inherited bool, sourceTaskID int64) ([]*db.Node, bool, error) {
	if !inherited {
		return store.ListByKindPage(db.KindIntent, before, limit)
	}
	batch := max(limit, 300)
	cursor := before
	out := make([]*db.Node, 0, limit+1)
	for {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil {
			return nil, false, err
		}
		for _, node := range page {
			if !inheritedIntentResult(node.State) {
				continue
			}
			node.SourceTaskID = sourceTaskID
			node.Inherited = true
			out = append(out, node)
			if len(out) > limit {
				return out[:limit], true, nil
			}
		}
		if !more || len(page) == 0 {
			return out, false, nil
		}
		cursor = page[len(page)-1].ID
	}
}

// explorationGraph returns the whole exploration chain (task graph) as nodes+edges.
func (s *Server) explorationGraph(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{"nodes": []any{}, "edges": []any{}})
		return
	}
	nodes, err := t.Store.Nodes(2000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	edges, err := t.Store.Edges(5000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		sourceNodes, nodeErr := source.Store.Nodes(2000)
		if nodeErr != nil {
			writeErr(w, 500, nodeErr.Error())
			return
		}
		sourceEdges, edgeErr := source.Store.Edges(5000)
		if edgeErr != nil {
			writeErr(w, 500, edgeErr.Error())
			return
		}
		sourceNodes, sourceEdges = inheritedGraphSnapshot(sourceNodes, sourceEdges, source.Task.TaskID)
		nodes = append(nodes, sourceNodes...)
		edges = append(edges, sourceEdges...)
	}
	writeJSON(w, 200, map[string]any{"nodes": taskNodeDTOs(nodes), "edges": edgeDTOs(edges)})
}

// explorationNodes serves the 播报板: this task's own exploration nodes as a
// paged time series (newest first unless ?order=asc), filterable by kind/state
// and a payload substring. Inherited nodes are deliberately out of scope — the
// board reports what this task is doing right now, and paging across the source
// tasks' stores would make the cursor meaningless.
// Response also carries the edges touching the page plus the neighbour nodes
// they point at, so each row can state where it came from and what it produced.
func (s *Server) explorationNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 1)
	size := atoiDefault(q.Get("size"), 20)
	t := s.m.ResolveTask(q.Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{
			"items": []any{}, "total": 0, "page": page, "size": size,
			"edges": []any{}, "refs": map[string]any{},
		})
		return
	}
	filter := db.NodeFilter{
		Kinds:  csvValues(q.Get("kind")),
		States: csvValues(q.Get("state")),
		Query:  q.Get("q"),
		Asc:    q.Get("order") == "asc",
	}
	nodes, total, err := t.Store.NodesPage(filter, page, size)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := make([]int64, 0, len(nodes))
	onPage := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
		onPage[n.ID] = true
	}
	edges, err := t.Store.EdgesTouching(ids)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	neighbourSet := map[int64]bool{}
	for _, e := range edges {
		if !onPage[e.From] {
			neighbourSet[e.From] = true
		}
		if !onPage[e.To] {
			neighbourSet[e.To] = true
		}
	}
	neighbourIDs := make([]int64, 0, len(neighbourSet))
	for id := range neighbourSet {
		neighbourIDs = append(neighbourIDs, id)
	}
	neighbours, err := t.Store.NodesByIDs(neighbourIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	refs := make(map[string]TaskNodeDTO, len(neighbours))
	for _, n := range neighbours {
		refs[i64s(n.ID)] = taskNodeDTO(n)
	}
	writeJSON(w, 200, map[string]any{
		"items":  taskNodeDTOs(nodes),
		"total":  total,
		"page":   page,
		"size":   size,
		"edges":  edgeDTOs(edges),
		"refs":   refs,
		"assets": s.nodeAnchoredAssets(t, append(ids, neighbourIDs...)),
	})
}

// nodeAnchoredAssets resolves the exploration_anchors of the given nodes into
// display-ready asset labels, keyed by node id. Anchors are provenance decoration
// for the 播报板 — a failure here must not cost the caller its page, so errors are
// logged and degrade to "no assets".
func (s *Server) nodeAnchoredAssets(t *Task, nodeIDs []int64) map[string][]FindingAssetDTO {
	out := map[string][]FindingAssetDTO{}
	anchors, err := t.Store.NodeAssets(nodeIDs)
	if err != nil {
		log.Printf("[broadcast] node assets: %v", err)
		return out
	}
	var flat []int64
	for _, ids := range anchors {
		flat = append(flat, ids...)
	}
	assets := s.resolveAssetIDs(flat)
	for nodeID, ids := range anchors {
		if dtos := findingAssetDTOs(ids, assets); len(dtos) > 0 {
			out[i64s(nodeID)] = dtos
		}
	}
	return out
}

// csvValues splits a comma-separated query parameter, dropping empty entries.
func csvValues(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// activity returns the worker execution step log (incremental via ?since=seq).
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "cursor": 0})
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 300)
	var intentPtr *int64
	if iv := r.URL.Query().Get("intent"); iv != "" {
		if n, err := strconv.ParseInt(iv, 10, 64); err == nil {
			intentPtr = &n
		}
	}
	var (
		items  []db.Activity
		cursor int64
		err    error
	)
	if intentPtr != nil {
		items, cursor, err = t.Store.ActivityListWithSources(*intentPtr, since, limit)
	} else {
		items, cursor, err = t.Store.ActivityList(nil, since, limit)
	}
	if err != nil {
		log.Printf("[activity] task=%s since=%d limit=%d intent=%v: %v", t.ID, since, limit, intentPtr, err)
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": activityDTOs(items), "cursor": cursor})
}

// parseActivitySession maps a stable session key (main | plan | intent:<ID>) to a
// DB session filter. Goal Agent + Planner both live under worker="planner" (the
// single Plan session); a Worker session is one intent, keyed by its node id.
func parseActivitySession(sess string) (db.ActivitySessionFilter, bool) {
	switch {
	case sess == "" || sess == "main":
		// bare "main" = the current segment; caller resolves MainSeg via the store.
		return db.ActivitySessionFilter{Main: true}, true
	case strings.HasPrefix(sess, "main:"):
		seg, err := strconv.Atoi(strings.TrimPrefix(sess, "main:"))
		if err != nil || seg < 0 {
			return db.ActivitySessionFilter{}, false
		}
		return db.ActivitySessionFilter{Main: true, MainSeg: &seg}, true
	case sess == "plan":
		return db.ActivitySessionFilter{Worker: "planner"}, true
	case strings.HasPrefix(sess, "intent:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(sess, "intent:"), 10, 64)
		if err != nil {
			return db.ActivitySessionFilter{}, false
		}
		return db.ActivitySessionFilter{NodeID: &id}, true
	}
	return db.ActivitySessionFilter{}, false
}

// activitySessionStore resolves a worker session against the current task and
// its direct sources. Planner/main always stay local; inherited intent sessions
// are immutable history and use the source exploration only for reads.
func activitySessionStore(t *Task, filter db.ActivitySessionFilter) (*db.ExplorationStore, int64, error) {
	if filter.NodeID == nil {
		return t.Store, 0, nil
	}
	node, err := t.Store.GetNodeWithSources(*filter.NodeID)
	if err != nil {
		return nil, 0, err
	}
	if node == nil || node.Kind != db.KindIntent {
		return nil, 0, nil
	}
	if !node.Inherited {
		return t.Store, 0, nil
	}
	if !inheritedIntentResult(node.State) {
		return nil, 0, nil
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		return nil, 0, err
	}
	for _, source := range sources {
		if source.Task.TaskID == node.SourceTaskID {
			return source.Store, source.Task.TaskID, nil
		}
	}
	return nil, 0, nil
}

// activityHistory serves one reverse-paginated page of a session's activity history.
// The latest page (no ?before) opens a session; ?before=<id> pulls the older page on
// scroll-up. snapshot_cursor is the TASK-level max id at query time — the client uses
// it to open the single task SSE at since=snapshot_cursor so history (id<=cursor) and
// the live tail (id>cursor) meet with no gap and no overlap.
func (s *Server) activityHistory(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	q := r.URL.Query()
	sess := q.Get("session")
	filter, ok := parseActivitySession(sess)
	if !ok {
		writeErr(w, 400, "bad session")
		return
	}
	if filter.Main && filter.MainSeg == nil { // bare "main" → the current segment
		seg, err := t.Store.CurrentMainSeg()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		filter.MainSeg = &seg
	}
	before := int64(atoiDefault(q.Get("before"), 0))
	limit := min(atoiDefault(q.Get("limit"), 200), 500) // cap so one request can't pull an unbounded slice
	store, sourceTaskID, err := activitySessionStore(t, filter)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if store == nil {
		writeErr(w, 404, "session not found")
		return
	}
	// The browser tails only this task's broadcaster. Keep the SSE join cursor
	// local even when the opened worker transcript comes from a related task.
	snapshot, err := t.Store.ActivityMaxID()
	if err != nil {
		log.Printf("[activity/history] task=%s session=%s snapshot: %v", t.ID, sess, err)
		writeErr(w, 500, err.Error())
		return
	}
	var items []db.Activity
	var hasMore bool
	if sourceTaskID > 0 {
		// Source sessions are readable only while the intent remains terminal. The
		// DB query also removes model reasoning/accounting rows from inherited data.
		items, hasMore, err = store.ActivityPageForTerminalIntent(*filter.NodeID, before, limit)
	} else {
		items, hasMore, err = store.ActivityPage(filter, before, limit)
	}
	if err != nil {
		log.Printf("[activity/history] task=%s session=%s before=%d limit=%d: %v", t.ID, sess, before, limit, err)
		writeErr(w, 500, err.Error())
		return
	}
	if sourceTaskID > 0 {
		for i := range items {
			items[i].SourceTaskID = sourceTaskID
			items[i].Inherited = true
		}
	}
	earliest := before
	if len(items) > 0 {
		earliest = items[0].ID
	}
	writeJSON(w, 200, map[string]any{
		"items":           activityDTOs(items),
		"snapshot_cursor": snapshot,
		"earliest_cursor": earliest,
		"has_more":        hasMore,
	})
}

// tokenStats returns per-worker token usage (input/output/cache read/write) for a
// task — main agent, planner, and each work#N.
func (s *Server) tokenStats(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{
			"workers":  []db.TokenUsage{},
			"sessions": []db.SessionTokenUsage{},
			"total":    tokenTotalDTO(db.TokenUsage{}),
		})
		return
	}
	stats, err := t.Store.TokenStatsByWorker()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sessions, err := t.Store.TokenStatsBySession()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	total, err := t.Store.TokenTotal() // whole-task total (all agents)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"workers": stats, "sessions": sessions, "total": tokenTotalDTO(total)})
}

// tokenDailyStats returns global token consumption aggregated by calendar day
// (UTC) across all tasks for the past ?days=N days (default 30).
func (s *Server) tokenDailyStats(w http.ResponseWriter, r *http.Request) {
	days := atoiDefault(r.URL.Query().Get("days"), 30)
	if s.m.pg == nil {
		writeJSON(w, 200, []any{})
		return
	}
	buckets, err := s.m.pg.TokenDailyAll(days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if buckets == nil {
		buckets = []db.DailyTokenBucket{}
	}
	writeJSON(w, 200, buckets)
}

// conversationTokens returns per-conversation token summaries so the dashboard can
// merge chat (conversation) usage into its per-profile / daily token stats — which
// otherwise count only task (exploration) usage.
func (s *Server) conversationTokens(w http.ResponseWriter, r *http.Request) {
	if s.m.pg == nil {
		writeJSON(w, 200, []any{})
		return
	}
	rows, err := s.m.pg.ConversationTokenSummaries()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}

// streamActivity is the live SSE tail: it replays history after ?since=<seq>, then
// pushes each newly appended activity for the task. The seq cursor makes history +
// live join gap-free; on reconnect the client passes its last seq to catch any
// dropped events. Optional ?intent=<id> scopes the stream to one worker session.
// getLogs returns recent backend log lines (Seq > since), newest-last.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 500)
	lines, cursor := logSink.recent(since, limit)
	writeJSON(w, 200, map[string]any{"items": lines, "cursor": cursor})
}

// getLogsHistory returns older log lines from the DB (before a given db_id).
// GET /api/logs/history?before=<db_id>&limit=200
// Returns {items:[LogLine], has_more: bool}.
func (s *Server) getLogsHistory(w http.ResponseWriter, r *http.Request) {
	if s.m.pg == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "has_more": false})
		return
	}
	before := int64(atoiDefault(r.URL.Query().Get("before"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	if limit > 500 {
		limit = 500
	}
	// If no before given, return the most recent DB rows (mirrors ring restore).
	var (
		rows []*db.DBLog
		err  error
	)
	if before <= 0 {
		rows, err = s.m.pg.RecentLogs(limit)
	} else {
		rows, err = s.m.pg.ListLogsBefore(before, limit)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	items := make([]LogLine, 0, len(rows))
	for _, r := range rows {
		items = append(items, LogLine{
			DBID:  r.ID,
			TS:    r.CreatedAt.Format(time.RFC3339),
			Level: r.Level,
			Tag:   r.Tag,
			Text:  r.Text,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items, "has_more": len(rows) == limit})
}

// streamLogs is the live SSE tail of the backend log: replays history after
// ?since=<seq>, then pushes each new line.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := logSink.subscribe()
	defer unsub()
	send := func(l LogLine) {
		b, _ := json.Marshal(l)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	lines, cursor := logSink.recent(since, 1000)
	for _, l := range lines {
		send(l)
	}
	if cursor > since {
		since = cursor
	}
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			if l.Seq <= since {
				continue
			}
			since = l.Seq
			send(l)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) streamActivity(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	var intentPtr *int64
	if iv := r.URL.Query().Get("intent"); iv != "" {
		if n, err := strconv.ParseInt(iv, 10, 64); err == nil {
			intentPtr = &n
		}
	}
	// Cursor precedence: the browser's automatic reconnect sends Last-Event-ID (the
	// last id it received) — trust it over the query so an auto-reconnect resumes
	// exactly where it dropped. A fresh/manual connect has no header and passes
	// since=<snapshot_cursor> from the history page instead.
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	if le := r.Header.Get("Last-Event-ID"); le != "" {
		if n, err := strconv.ParseInt(le, 10, 64); err == nil {
			since = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	// Subscribe BEFORE replaying history so events in between aren't lost; dedup the
	// overlap by skipping channel events whose id was already replayed.
	ch, unsub := s.engine.Broadcaster().Subscribe(t.ID)
	defer unsub()

	// Emit a standard SSE id: line so the browser echoes it as Last-Event-ID on
	// auto-reconnect (see cursor precedence above).
	sendSSE := func(a db.Activity) {
		b, _ := json.Marshal(activityDTO(a))
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", a.ID, b)
		flusher.Flush()
	}

	// Compensate the DB backlog after `since` in batches until caught up. This is the
	// gap between the history snapshot and the live tail — NOT the first-page history
	// (that's the /activity/history endpoint). A long task can have far more than one
	// batch, so loop instead of a single fixed read; on a query error log it and close
	// so the client reconnects and retries from its last id (broadcast is lossy — the
	// DB is the source of truth). The Broadcaster keeps buffering live events meanwhile;
	// the id<=since skip below drops any that this replay already covered.
	const replayBatch = 500
	for {
		items, cursor, err := t.Store.ActivityList(intentPtr, since, replayBatch)
		if err != nil {
			log.Printf("[activity/stream] task=%s replay since=%d: %v", t.ID, since, err)
			return
		}
		for _, a := range items {
			sendSSE(a)
		}
		if cursor > since {
			since = cursor
		}
		if len(items) < replayBatch {
			break
		}
	}
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-ch:
			if !ok {
				return
			}
			if a.ID <= since {
				continue // already replayed
			}
			if intentPtr != nil && (a.NodeID == nil || *a.NodeID != *intentPtr) {
				continue // scoped session: only this intent's steps
			}
			since = a.ID
			sendSSE(a)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// activityDetail lazily returns the full detail blob for one step.
func (s *Server) activityDetail(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "no task")
		return
	}
	seq, _ := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	d, err := taskActivityDetail(t, seq)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"detail": d})
}

// taskActivityDetail keeps the legacy local-task behavior (including thinking
// rows), while inherited details are restricted to terminal worker intents.
// This prevents a guessed global activity id from exposing source planner/main
// or in-flight transcripts.
func taskActivityDetail(t *Task, seq int64) (string, error) {
	return t.Store.ActivityDetailWithSources(seq)
}
