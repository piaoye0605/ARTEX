package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// This file pins the one-click update surface in update.go. The real download
// logic lives in selfupdate; here the subject is the HTTP layer plus the two
// pieces of shared state it owns:
//   - releaseCache: fetch is injectable, results are cached (errors too, with a
//     shorter TTL), force bypasses the cache, and a canceled context must NOT
//     poison the cache
//   - updateHub: begin is a mutex (second apply → 409), fanout drops rather
//     than blocks, unsubscribe is idempotent and closes the channel
//
// Package-level vars (relCache, updHub, BuildVersion, bootState) are swapped
// per test and always restored.

func newFreshHub() *updateHub {
	return &updateHub{
		cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
		subs: map[chan updateProgress]struct{}{},
	}
}

func relFetch(rel *selfupdate.Release, err error) func(context.Context, *http.Client) (*selfupdate.Release, error) {
	return func(context.Context, *http.Client) (*selfupdate.Release, error) { return rel, err }
}

func TestReleaseCacheBehaviour(t *testing.T) {
	orig := relCache
	t.Cleanup(func() { relCache = orig })

	rel := &selfupdate.Release{TagName: "v9.9.9"}
	calls := 0
	relCache = &releaseCache{fetch: func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return rel, nil
	}}

	got, err := relCache.get(context.Background(), http.DefaultClient, false)
	if err != nil || got != rel || calls != 1 {
		t.Fatalf("first get rel=%v err=%v calls=%d", got, err, calls)
	}
	// Cached: the fetcher is not called again without force.
	if _, err = relCache.get(context.Background(), http.DefaultClient, false); err != nil || calls != 1 {
		t.Fatalf("cached get err=%v calls=%d", err, calls)
	}
	// force=1 always goes back to the source.
	if _, err = relCache.get(context.Background(), http.DefaultClient, true); err != nil || calls != 2 {
		t.Fatalf("forced get err=%v calls=%d", err, calls)
	}

	// Errors are cached too (short TTL) so a dead GitHub does not get hammered;
	// once the TTL lapses the next caller retries.
	errCalls := 0
	ec := &releaseCache{fetch: func(context.Context, *http.Client) (*selfupdate.Release, error) {
		errCalls++
		return nil, errors.New("github down")
	}}
	if _, err = ec.get(context.Background(), http.DefaultClient, false); err == nil || errCalls != 1 {
		t.Fatalf("error get err=%v calls=%d", err, errCalls)
	}
	if _, err = ec.get(context.Background(), http.DefaultClient, false); err == nil || errCalls != 1 {
		t.Fatalf("error must be cached err=%v calls=%d", err, errCalls)
	}
	ec.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err = ec.get(context.Background(), http.DefaultClient, false); err == nil || errCalls != 2 {
		t.Fatalf("expired error must refetch err=%v calls=%d", err, errCalls)
	}

	// A canceled context is a client hangup, not a GitHub outage: the failure
	// must not be cached, the next caller retries.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cc := &releaseCache{fetch: func(context.Context, *http.Client) (*selfupdate.Release, error) {
		errCalls++
		return nil, errors.New("boom")
	}}
	if _, err = cc.get(ctx, http.DefaultClient, false); err == nil || errCalls != 3 {
		t.Fatalf("canceled get err=%v calls=%d", err, errCalls)
	}
	if _, err = cc.get(context.Background(), http.DefaultClient, false); err == nil || errCalls != 4 {
		t.Fatalf("post-cancel get must refetch err=%v calls=%d", err, errCalls)
	}
}

func TestUpdateHubLifecycle(t *testing.T) {
	orig := updHub
	t.Cleanup(func() { updHub = orig })
	h := newFreshHub()

	ch, unsub := h.subscribe()
	defer unsub()
	if !h.begin("v2.0.0") {
		t.Fatal("begin refused the first update")
	}
	if h.begin("v3.0.0") {
		t.Fatal("begin allowed a concurrent update")
	}
	p := <-ch
	if p.Phase != selfupdate.PhaseDownload || p.Version != "v2.0.0" {
		t.Fatalf("begin progress=%+v", p)
	}

	h.publish(selfupdate.PhaseDownload, 40, "downloading")
	if p = <-ch; p.Percent != 40 {
		t.Fatalf("publish progress=%+v", p)
	}
	h.finish(nil)
	if p = <-ch; p.Phase != selfupdate.PhaseStaged || p.Percent != 100 {
		t.Fatalf("finish progress=%+v", p)
	}
	if _, running := h.snapshot(); running {
		t.Fatal("hub still running after finish")
	}

	// A failed finish records the error message for the stream snapshot.
	h.finish(errors.New("disk full"))
	if p = <-ch; p.Phase != selfupdate.PhaseFailed || p.Error != "disk full" {
		t.Fatalf("failed finish progress=%+v", p)
	}

	// Fanout must drop progress for a stalled subscriber instead of blocking:
	// fill a channel far beyond its buffer and keep publishing.
	_, unsubSlow := h.subscribe()
	defer unsubSlow()
	for i := 0; i < 300; i++ {
		h.publish(selfupdate.PhaseDownload, i, "flood")
	}
	unsub() // double unsubscribe is a no-op via sync.Once
	unsub()
	// Closing a channel does not discard its buffer: drain the flood that was
	// queued before unsubscription, then the read must report closed.
	for range ch {
	}
	if _, ok := <-ch; ok {
		t.Fatal("unsubscribe did not close the channel")
	}
}

type noFlushResponseWriter struct {
	header http.Header
	body   strings.Builder
	code   int
}

func (w *noFlushResponseWriter) Header() http.Header { return w.header }
func (w *noFlushResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}
func (w *noFlushResponseWriter) WriteHeader(code int) { w.code = code }

func TestUpdateStreamRoute(t *testing.T) {
	orig := updHub
	t.Cleanup(func() { updHub = orig })
	updHub = newFreshHub()
	s := &Server{m: &Manager{}}

	// A writer without Flusher support is a 500, not a hung request.
	w := &noFlushResponseWriter{header: http.Header{}}
	s.updateStream(w, httptest.NewRequest(http.MethodGet, "/api/update/stream", nil))
	if w.code != http.StatusInternalServerError || !strings.Contains(w.body.String(), "streaming unsupported") {
		t.Fatalf("non-flusher writer code=%d body=%s", w.code, w.body.String())
	}

	// The stream first replays the current state, then live publishes, and
	// exits when the request context is canceled.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/update/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.updateStream(rec, req); close(done) }()

	waitFor := func(substr string, what string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(rec.Body.String(), substr) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s never appeared in stream: %s", what, rec.Body.String())
	}
	waitFor(`"phase":"idle"`, "initial snapshot")
	updHub.publish(selfupdate.PhaseDownload, 42, "halfway")
	waitFor(`"percent":42`, "published progress")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not exit after context cancel")
	}
}

func TestUpdateCheckRoute(t *testing.T) {
	origCache, origVer := relCache, BuildVersion
	t.Cleanup(func() {
		relCache = origCache
		BuildVersion = origVer
		SetBootUpdateState(selfupdate.State{})
	})
	SetBootUpdateState(selfupdate.State{RolledBack: true, Detail: "上次升级已自动回滚"})
	BuildVersion = "1.2.3"

	asset := selfupdate.Asset{Name: selfupdate.AssetName("v1.3.0", runtime.GOOS, runtime.GOARCH), Size: 4096}
	rel := &selfupdate.Release{
		TagName: "v1.3.0", Body: "release notes",
		HTMLURL:     "https://github.com/Autumn-27/artex/releases/tag/v1.3.0",
		PublishedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Assets:      []selfupdate.Asset{asset},
	}
	relCache = &releaseCache{fetch: relFetch(rel, nil)}
	s := &Server{m: &Manager{}}

	get := func(query string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/update/check"+query, nil)
		rec := httptest.NewRecorder()
		s.updateCheck(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
		return rec.Code, out
	}

	code, out := get("")
	if code != http.StatusOK {
		t.Fatalf("check status=%d", code)
	}
	if out["current"] != "1.2.3" || out["latest"] != "v1.3.0" || out["notes"] != "release notes" {
		t.Fatalf("check basics=%+v", out)
	}
	if out["repo"] != selfupdate.Repo || out["mode"] == "" || out["os"] != runtime.GOOS {
		t.Fatalf("check env=%+v", out)
	}
	if out["rolled_back"] != true || out["boot_notice"] != "上次升级已自动回滚" {
		t.Fatalf("boot state=%+v", out)
	}
	if out["asset"] != asset.Name || out["asset_available"] != true || out["size"] != float64(4096) {
		t.Fatalf("asset probe=%+v", out)
	}
	if out["comparable"] != true || out["has_update"] != true || out["published_at"] == "" {
		t.Fatalf("version compare=%+v", out)
	}

	// An upstream failure is reported inline (still 200) with no latest field.
	relCache = &releaseCache{fetch: relFetch(nil, errors.New("github down"))}
	_, out = get("")
	if out["error"] != "github down" {
		t.Fatalf("error surface=%+v", out)
	}
	if _, ok := out["latest"]; ok {
		t.Fatalf("latest leaked on error=%+v", out)
	}

	// A dev build is never updatable, even when a release exists.
	BuildVersion = "dev"
	relCache = &releaseCache{fetch: relFetch(rel, nil)}
	_, out = get("")
	if out["comparable"] != false || out["has_update"] != false ||
		!strings.Contains(out["reason"].(string), "不是正式发布版本") {
		t.Fatalf("dev build=%+v", out)
	}
}

func TestUpdateApplyGuards(t *testing.T) {
	origCache, origHub, origVer := relCache, updHub, BuildVersion
	t.Cleanup(func() {
		relCache = origCache
		updHub = origHub
		BuildVersion = origVer
	})
	s := &Server{m: &Manager{}, ctx: context.Background()}

	post := func() *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.updateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
		return rec
	}
	BuildVersion = "1.2.3"

	// Upstream failure → 502.
	relCache = &releaseCache{fetch: relFetch(nil, errors.New("github down"))}
	if rec := post(); rec.Code != http.StatusBadGateway {
		t.Fatalf("fetch error status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A dev build refuses to update even with a release available.
	relCache = &releaseCache{fetch: relFetch(&selfupdate.Release{TagName: "v2.0.0"}, nil)}
	BuildVersion = "dev"
	if rec := post(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不是正式发布版本") {
		t.Fatalf("dev apply status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Same version → 400 已是最新.
	BuildVersion = "2.0.0"
	if rec := post(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "已是最新") {
		t.Fatalf("no-op apply status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A running update mutexes a second apply behind a 409.
	BuildVersion = "1.2.3"
	updHub = newFreshHub()
	if !updHub.begin("v2.0.0") {
		t.Fatal("setup begin failed")
	}
	if rec := post(); rec.Code != http.StatusConflict {
		t.Fatalf("concurrent apply status=%d body=%s", rec.Code, rec.Body.String())
	}
	updHub.finish(nil)

	// Accepted apply runs in the background; a failing download ends the hub
	// in PhaseFailed without ever requesting a restart.
	updHub = newFreshHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	rel := &selfupdate.Release{TagName: "v2.0.0", Assets: []selfupdate.Asset{{
		Name: selfupdate.AssetName("v2.0.0", runtime.GOOS, runtime.GOARCH),
		URL:  srv.URL + "/artex.zip", Size: 10,
	}}}
	relCache = &releaseCache{fetch: relFetch(rel, nil)}
	rec := post()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accepted status=%d body=%s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		OK     bool   `json:"ok"`
		Target string `json:"target"`
	}
	assetsDecode(t, rec, &accepted)
	if !accepted.OK || accepted.Target != "v2.0.0" {
		t.Fatalf("accepted body=%+v", accepted)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, running := updHub.snapshot()
		if !running && cur.Phase == selfupdate.PhaseFailed {
			if cur.Error == "" {
				t.Fatal("failed phase without an error message")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub never failed: %+v running=%v", cur, running)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A failed update must not request a restart.
	select {
	case <-RestartRequested():
		t.Fatal("a failed update must not request a restart")
	default:
	}
}

func TestUpdateRollbackRoute(t *testing.T) {
	origHub := updHub
	t.Cleanup(func() { updHub = origHub })
	s := &Server{m: &Manager{}}

	// A running update blocks rollback with 409.
	updHub = newFreshHub()
	if !updHub.begin("v2.0.0") {
		t.Fatal("setup begin failed")
	}
	rec := httptest.NewRecorder()
	s.updateRollback(rec, httptest.NewRequest(http.MethodPost, "/api/update/rollback", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("running rollback status=%d body=%s", rec.Code, rec.Body.String())
	}
	updHub.finish(nil)

	// Without a backup on disk the store refuses and the route reports 400.
	updHub = newFreshHub()
	rec = httptest.NewRecorder()
	s.updateRollback(rec, httptest.NewRequest(http.MethodPost, "/api/update/rollback", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-backup rollback status=%d body=%s", rec.Code, rec.Body.String())
	}
}
