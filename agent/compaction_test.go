package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// fakeCompactionProvider returns a fixed body for every compression call, or an
// error after `failAfter` successful calls (0 = never fail). calls counts
// stream invocations so tests can assert the LLM was (or was not) consulted —
// the signature-reuse path must skip it entirely.
type fakeCompactionProvider struct {
	body      string
	failAfter int32
	calls     atomic.Int32
}

func (p *fakeCompactionProvider) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.calls.Add(1)
		if n := p.calls.Load(); p.failAfter > 0 && n > p.failAfter {
			yield(llm.StreamEvent{}, errors.New("compaction provider exhausted"))
			return
		}
		events := []llm.StreamEvent{
			{Type: llm.SETextDelta, Text: p.body},
			{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (p *fakeCompactionProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

// compactionFixture builds a task + exploration with a settled chain:
// intent a → fact b → fact c. Everything is done/confirmed, so after enough
// rounds the whole chain ages into the eligible-cold set.
func compactionFixture(t *testing.T) (*db.DB, *db.ExplorationStore, int64, [3]int64) {
	t.Helper()
	d := testDB(t)
	task, err := d.CreateTask("compaction fixture", "fold cold chain", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	ts := d.Exploration(task.ExplorationID)
	a, err := ts.AddNode(db.KindIntent, map[string]any{"summary": "attempt admin panel"}, 0, "done", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ts.AddNode(db.KindFact, map[string]any{"summary": "panel reachable at /admin"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ts.AddNode(db.KindFact, map[string]any{"summary": "admin login bypassed"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ from, to int64 }{{a, b}, {b, c}} {
		if err := ts.Link(e.from, db.RelYields, e.to); err != nil {
			t.Fatal(err)
		}
	}
	return d, ts, task.ID, [3]int64{a, b, c}
}

// ageCold runs enough planner rounds for the fixture's nodes to become eligible:
// the first maintain stamps cold_since_round=1 and eligibleCold requires
// round - coldSince ≥ R(6), so seven synchronous maintain calls are enough.
func ageCold(t *testing.T, ts *db.ExplorationStore, c *Compactor) int64 {
	t.Helper()
	var round int64
	for i := 0; i < 7; i++ {
		r, _, _, err := c.maintain(ts)
		if err != nil {
			t.Fatal(err)
		}
		round = r
	}
	return round
}

func TestCompactorNilGuards(t *testing.T) {
	// A nil Compactor, a nil provider and a nil store must all be safe no-ops —
	// callers (planner wake-ups) never branch on them.
	var nilCompactor *Compactor
	nilCompactor.OnPlannerRound(context.Background(), nil)
	c := NewCompactor(nil, "test-model")
	c.OnPlannerRound(context.Background(), nil)
	c.OnPlannerRound(context.Background(), nil) // nil store again — no panic twice
}

func TestCompactorStampOpsAndPayloadHelpers(t *testing.T) {
	if got := toDBStampOps(nil); len(got) != 0 {
		t.Fatalf("nil ops → empty, got %v", got)
	}
	ops := []stampOp{{ID: 7, Set: true, Round: 3}, {ID: 8, Set: false, Round: 4}}
	dbOps := toDBStampOps(ops)
	if len(dbOps) != 2 || dbOps[0].ID != 7 || !dbOps[0].Set || dbOps[0].Round != 3 || dbOps[1].Set {
		t.Fatalf("stamp op conversion wrong: %+v", dbOps)
	}

	stamps := map[int64]*int64{}
	applyStampsInPlace(stamps, ops)
	if stamps[7] == nil || *stamps[7] != 3 {
		t.Fatalf("set op missing from in-memory map: %+v", stamps[7])
	}
	if stamps[8] != nil {
		t.Fatalf("clear op must nil the entry, got %v", *stamps[8])
	}

	factNode := &db.Node{ID: 11, Kind: db.KindFact, Payload: json.RawMessage(`{"summary":"s","confidence":"observed"}`)}
	intentNode := &db.Node{ID: 12, Kind: db.KindIntent, Payload: json.RawMessage(`{"summary":"i"}`)}
	payload := digestPayload("body", block{Members: []int64{11, 12}, Anchors: []int64{5}}, map[int64]*db.Node{11: factNode, 12: intentNode}, 2, "sig-1")
	memberIDs := payload["member_ids"].(map[string]any)
	if len(memberIDs["facts"].([]int64)) != 1 || len(memberIDs["intents"].([]int64)) != 1 {
		t.Fatalf("payload kind split wrong: %v", memberIDs)
	}
	if payload["body"] != "body" || payload["generation"] != 2 || payload["signature"] != "sig-1" {
		t.Fatalf("payload scalars wrong: %v", payload)
	}

	// Round-trip: the digest node written by AddDigest must decode back to the
	// signature/generation/members the payload declared.
	node := &db.Node{Payload: mustJSON(t, payload)}
	if sig, gen := digestSigGen(node); sig != "sig-1" || gen != 2 {
		t.Fatalf("digestSigGen roundtrip: sig=%q gen=%d", sig, gen)
	}
	if ids := digestMemberIDs(node); len(ids) != 2 || ids[0] != 11 || ids[1] != 12 {
		t.Fatalf("digestMemberIDs roundtrip: %v", ids)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNodeSummaryAndConfidence(t *testing.T) {
	if nodeSummary(nil) != "" || nodeConfidence(nil) != "" {
		t.Fatalf("nil node must yield empty strings")
	}
	bare := &db.Node{Payload: json.RawMessage(`{"state":"done"}`)}
	if nodeSummary(bare) != "" || nodeConfidence(bare) != "" {
		t.Fatalf("missing fields must yield empty strings")
	}
	summarized := &db.Node{Payload: json.RawMessage(`{"text":"from text field","confidence":"inferred"}`)}
	if nodeSummary(summarized) != "from text field" {
		t.Fatalf("text fallback broken: %q", nodeSummary(summarized))
	}
	if nodeConfidence(summarized) != "inferred" {
		t.Fatalf("confidence missing: %q", nodeConfidence(summarized))
	}
}

func TestGenerationFor(t *testing.T) {
	c := NewCompactor(nil, "m")
	b := block{Members: []int64{3, 4}}
	if got := c.generationFor(b, nil); got != 1 {
		t.Fatalf("fresh fold must be generation 1, got %d", got)
	}
	// Active digest covering member 3 with generation 4 → overlap lifts to 5.
	active := &db.Node{Payload: mustJSON(t, map[string]any{
		"signature": "d1", "generation": 4,
		"member_ids": map[string]any{"facts": []int64{3}, "intents": []int64{}},
	})}
	if got := c.generationFor(b, []*db.Node{active}); got != 5 {
		t.Fatalf("merge must be max(overlap)+1, got %d", got)
	}
	// A digest that does not touch the block's members must be ignored.
	unrelated := &db.Node{Payload: mustJSON(t, map[string]any{
		"signature": "d2", "generation": 9,
		"member_ids": map[string]any{"facts": []int64{99}, "intents": []int64{}},
	})}
	if got := c.generationFor(b, []*db.Node{active, unrelated}); got != 5 {
		t.Fatalf("non-overlapping digest must not raise generation, got %d", got)
	}
}

func TestBuildCompressionInput(t *testing.T) {
	g := newColdGraph(
		[]cgNode{
			{ID: 1, Kind: db.KindIntent, State: "done"},
			{ID: 2, Kind: db.KindFact, State: "confirmed"},
			{ID: 3, Kind: db.KindFact, State: "confirmed"},
		},
		[]cgEdge{{From: 1, Rel: db.RelYields, To: 2}},
	)
	nodes := map[int64]*db.Node{
		1: {ID: 1, Kind: db.KindIntent, State: "done", Payload: json.RawMessage(`{"summary":"probe admin"}`)},
		2: {ID: 2, Kind: db.KindFact, State: "confirmed", Payload: json.RawMessage(`{"summary":"admin open","confidence":"observed"}`)},
	}
	in := buildCompressionInput(g, block{Members: []int64{1, 2, 3}, Anchors: []int64{9}}, nodes)
	for _, want := range []string{
		"【成员节点", "- #1 [intent/done] probe admin", "- #2 [fact/confirmed] admin open (confidence=observed)",
		"- #3 [fact/]", "【成员之间的血缘边", "- #1 产出/派生→ #2", "【共同父 / 上下文锚", "- #9 []",
	} {
		if !strings.Contains(in, want) {
			t.Fatalf("compression input missing %q:\n%s", want, in)
		}
	}
}

func TestCompactorTryStartHonorsCooldown(t *testing.T) {
	c := NewCompactor(nil, "m")
	c.cooldown = 50 * time.Millisecond
	if !c.tryStart(1) {
		t.Fatalf("first start must acquire")
	}
	if c.tryStart(1) {
		t.Fatalf("second start while running must be refused")
	}
	if !c.tryStart(2) {
		t.Fatalf("other exploration must be independent")
	}
	c.finish(1)
	if c.tryStart(1) {
		t.Fatalf("start within cooldown must be refused")
	}
	time.Sleep(60 * time.Millisecond)
	if !c.tryStart(1) {
		t.Fatalf("start after cooldown must acquire")
	}
	c.finish(1)
	// Concurrent attempts: exactly one winner per exploration.
	wins := 0
	for i := 0; i < 4; i++ {
		if c.tryStart(3) {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent tryStart must have exactly one winner, got %d", wins)
	}
}

func TestCompactorCompressFailsOnEmptyBody(t *testing.T) {
	c := NewCompactor(&fakeCompactionProvider{body: "   "}, "m")
	_, ts, _, ids := compactionFixture(t)
	g, byID, err := loadColdGraph(ts)
	if err != nil {
		t.Fatal(err)
	}
	b := block{Members: ids[:]}
	if _, err := c.compress(context.Background(), g, b, byID); err == nil || err.Error() != "empty body from model" {
		t.Fatalf("blank body must surface as error, got %v", err)
	}
}

func TestCompactorMaintainAgesStampsAndCounts(t *testing.T) {
	_, ts, _, ids := compactionFixture(t)
	c := NewCompactor(nil, "m")
	round := ageCold(t, ts, c)
	stamps, err := ts.ColdStamps()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if stamps[id] == nil || *stamps[id] < 1 {
			t.Fatalf("node %d must be stamped cold (round %d), got %v", id, round, stamps[id])
		}
	}
}

func TestCompactorMinorFoldsColdBlock(t *testing.T) {
	prov := &fakeCompactionProvider{body: "综合结论：管理面板可达且已被绕过（#1,#2,#3）。"}
	_, ts, _, ids := compactionFixture(t)
	c := NewCompactor(prov, "m")
	ageCold(t, ts, c)
	eligible, err := coldEligibleCount(ts, c)
	if err != nil {
		t.Fatal(err)
	}
	if eligible < 3 {
		t.Fatalf("fixture chain must become eligible, got %d", eligible)
	}
	c.minor(context.Background(), ts)
	if prov.calls.Load() == 0 {
		t.Fatalf("minor must consult the LLM for the fold")
	}
	covered, err := ts.CoveredMembers()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if covered[id] == 0 {
			t.Fatalf("member %d must be covered by a digest", id)
		}
	}
	digests, err := ts.ActiveDigests()
	if err != nil || len(digests) != 1 {
		t.Fatalf("exactly one active digest expected: %d %v", len(digests), err)
	}
	if sig, gen := digestSigGen(digests[0]); sig == "" || gen != 1 {
		t.Fatalf("digest payload must carry signature and generation 1, got %q/%d", sig, gen)
	}
	if mem := digestMemberIDs(digests[0]); len(mem) != 3 {
		t.Fatalf("digest must cover all three members, got %v", mem)
	}
}

func coldEligibleCount(ts *db.ExplorationStore, c *Compactor) (int, error) {
	round, err := ts.RoundNo()
	if err != nil {
		return 0, err
	}
	g, _, err := loadColdGraph(ts)
	if err != nil {
		return 0, err
	}
	stamps, err := ts.ColdStamps()
	if err != nil {
		return 0, err
	}
	hot := g.hotSet()
	elig := g.eligibleCold(g.structuralCold(hot), stamps, round, c.params)
	return len(elig), nil
}

func TestCompactorMajorReusesUnchangedAndSupersedesStale(t *testing.T) {
	prov := &fakeCompactionProvider{body: "块综合：该链已探明（signature 相同则不调模型）。"}
	_, ts, _, ids := compactionFixture(t)
	c := NewCompactor(prov, "m")
	ageCold(t, ts, c)
	c.minor(context.Background(), ts)
	if prov.calls.Load() != 1 {
		t.Fatalf("minor folds exactly one block, got %d LLM calls", prov.calls.Load())
	}
	digests, err := ts.ActiveDigests()
	if err != nil || len(digests) != 1 {
		t.Fatalf("one digest after minor: %d %v", len(digests), err)
	}
	oldID := digests[0].ID

	// Major over an unchanged graph: every block's signature still matches the
	// stored digest → reuse, zero LLM calls.
	c.major(context.Background(), ts)
	if prov.calls.Load() != 1 {
		t.Fatalf("unchanged blocks must skip the LLM entirely, calls=%d", prov.calls.Load())
	}
	still, err := ts.ActiveDigests()
	if err != nil || len(still) != 1 || still[0].ID != oldID {
		t.Fatalf("unchanged digest must survive major: %d %v", len(still), err)
	}

	// Grow the chain: a new fact joins the block → signature changes → major must
	// supersede the stale digest and compress a new one (generation 2).
	d, err := ts.AddNode(db.KindFact, map[string]any{"summary": "dumped admin db"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Link(ids[2], db.RelYields, d); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetNodeState(d, "confirmed"); err != nil {
		t.Fatal(err)
	}
	ageCold(t, ts, c) // stamp the new node cold too
	c.major(context.Background(), ts)
	if prov.calls.Load() != 2 {
		t.Fatalf("stale signature must trigger exactly one new compression, calls=%d", prov.calls.Load())
	}
	after, err := ts.ActiveDigests()
	if err != nil || len(after) != 1 {
		t.Fatalf("exactly one digest after supersede: %d %v", len(after), err)
	}
	if after[0].ID == oldID {
		t.Fatalf("stale digest must have been replaced")
	}
	if _, gen := digestSigGen(after[0]); gen != 2 {
		t.Fatalf("merged digest must be generation 2, got %d", gen)
	}
	if mem := digestMemberIDs(after[0]); len(mem) != 4 {
		t.Fatalf("new digest must cover all four members, got %v", mem)
	}
	covered, err := ts.CoveredMembers()
	if err != nil {
		t.Fatal(err)
	}
	if covered[d] == 0 {
		t.Fatalf("newly grown member %d must be covered", d)
	}
}

func TestCompactorOnPlannerRoundTriggersBackgroundFold(t *testing.T) {
	prov := &fakeCompactionProvider{body: "后台压缩产物。"}
	_, ts, _, _ := compactionFixture(t)
	c := NewCompactor(prov, "m")
	c.n = 2 // fixture has 3 cold candidates → minor threshold met
	c.m = 100
	ageCold(t, ts, c) // stamps are maintained by these rounds already
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.OnPlannerRound(context.Background(), ts)
		if prov.calls.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if prov.calls.Load() == 0 {
		t.Fatalf("OnPlannerRound must launch the background compaction")
	}
	// The fold is async — wait for the digest to land.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ad, err := ts.ActiveDigests()
		if err == nil && len(ad) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("digest never landed within timeout")
}

func TestCompactorCompressProviderError(t *testing.T) {
	prov := &fakeCompactionProvider{body: "x", failAfter: 1}
	c := NewCompactor(prov, "m")
	_, ts, _, ids := compactionFixture(t)
	g, byID, err := loadColdGraph(ts)
	if err != nil {
		t.Fatal(err)
	}
	b := block{Members: ids[:]}
	if _, err := c.compress(context.Background(), g, b, byID); err != nil {
		t.Fatalf("first call should succeed, got %v", err)
	}
	if _, err := c.compress(context.Background(), g, b, byID); err == nil {
		t.Fatalf("exhausted provider must surface an error")
	}
}
