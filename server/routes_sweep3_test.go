package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Every route below starts with the same guard: `pg := s.pg(w)` answers 503 when
// PostgreSQL is not connected. That branch is invisible to the rest of the suite
// (all other tests run with a database), yet it is the one the UI relies on to
// tell "backend not connected" apart from "resource not found".
//
// The server here has no data source at all, so the assertion is also a
// fail-closed check: none of these handlers may dereference a nil store.
func TestHandlersFailClosedWithoutDataSource(t *testing.T) {
	s := &Server{
		ctx:    context.Background(),
		m:      &Manager{tasks: map[string]*Task{}, dir: t.TempDir()},
		jwtKey: []byte("no-datasource-signing-key-32b"),
	}

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/auth/status"},
		{http.MethodPost, "/api/auth/init"},
		{http.MethodGet, "/api/task-categories"},
		{http.MethodPost, "/api/task-categories"},
		{http.MethodGet, "/api/task-templates"},
		{http.MethodPost, "/api/task-templates"},
		{http.MethodPatch, "/api/task-templates/{id}"},
		{http.MethodDelete, "/api/task-templates/{id}"},
		{http.MethodGet, "/api/tasks/{id}/intercept-rules"},
		{http.MethodPost, "/api/tasks/{id}/intercept-rules"},
		{http.MethodPut, "/api/tasks/{id}/intercept-rules/{rid}"},
		{http.MethodDelete, "/api/tasks/{id}/intercept-rules/{rid}"},
		{http.MethodPost, "/api/tasks/{id}/intercept-rules/{rid}/toggle"},
		{http.MethodGet, "/api/exploration/findings/{id}/retests"},
		{http.MethodGet, "/api/exploration/findings/retests/active"},
		{http.MethodPost, "/api/exploration/findings/{id}/retests"},
		{http.MethodGet, "/api/notify/meta"},
		{http.MethodGet, "/api/notify/channels"},
		{http.MethodPost, "/api/notify/channels"},
		{http.MethodPatch, "/api/notify/channels/{id}"},
		{http.MethodDelete, "/api/notify/channels/{id}"},
		{http.MethodPost, "/api/notify/channels/{id}/test"},
		{http.MethodGet, "/api/notify/deliveries"},
		{http.MethodPost, "/api/notify/deliveries/{id}/retry"},
		{http.MethodGet, "/api/conversations"},
		{http.MethodPost, "/api/conversations"},
		{http.MethodPost, "/api/conversations/delete/batch"},
		{http.MethodGet, "/api/agents"},
		{http.MethodPost, "/api/agents"},
		{http.MethodPatch, "/api/triggers/{id}"},
		{http.MethodDelete, "/api/triggers/{id}"},
		{http.MethodGet, "/api/tools"},
		{http.MethodPut, "/api/tools/{key}"},
		{http.MethodPost, "/api/tools/custom"},
		{http.MethodPost, "/api/tools/custom/test"},
		{http.MethodPut, "/api/tools/custom/{key}"},
		{http.MethodDelete, "/api/tools/custom/{key}"},
		{http.MethodPost, "/api/tools/{key}/reset"},
		{http.MethodGet, "/api/mcp"},
		{http.MethodPost, "/api/mcp"},
		{http.MethodDelete, "/api/mcp/{id}"},
		{http.MethodGet, "/api/mcp/{id}/tools"},
		{http.MethodPost, "/api/mcp/{id}/refresh"},
		{http.MethodPost, "/api/sync/scopesentry/datasource"},
		{http.MethodDelete, "/api/skills/{name}"},
		{http.MethodGet, "/api/skills/missing"},
		{http.MethodGet, "/api/skills/{name}/usage"},
		{http.MethodGet, "/api/visibility/{kind}/{id}"},
		{http.MethodPost, "/api/visibility/toggle"},
		{http.MethodGet, "/api/visibility/skill/{name}"},
		{http.MethodPost, "/api/visibility/skill/toggle"},
		{http.MethodGet, "/api/llm/profiles"},
		{http.MethodPost, "/api/llm/profiles"},
		{http.MethodDelete, "/api/llm/profiles/{id}"},
		{http.MethodPost, "/api/llm/profiles/active"},
		{http.MethodGet, "/api/llm/retry-policy"},
		{http.MethodPost, "/api/llm/retry-policy"},
		{http.MethodPost, "/api/llm/pool/reset"},
		{http.MethodGet, "/api/intercept/rules"},
		{http.MethodPost, "/api/intercept/rules"},
		{http.MethodPut, "/api/intercept/rules/{id}"},
		{http.MethodDelete, "/api/intercept/rules/{id}"},
		{http.MethodPost, "/api/intercept/rules/{id}/toggle"},
		{http.MethodGet, "/api/asset-intercept/rules"},
		{http.MethodPost, "/api/asset-intercept/rules"},
		{http.MethodPut, "/api/asset-intercept/rules/{id}"},
		{http.MethodDelete, "/api/asset-intercept/rules/{id}"},
		{http.MethodPost, "/api/asset-intercept/rules/{id}/toggle"},
		{http.MethodGet, "/api/intercept/pending"},
		{http.MethodGet, "/api/intercept/pending/{id}"},
		{http.MethodGet, "/api/intercept/history"},
		{http.MethodGet, "/api/intercept/history/{id}"},
		{http.MethodGet, "/api/intercept/history/{id}/execution"},
		{http.MethodGet, "/api/intercept/task/{taskID}"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := crudDo(t, s, rt.method, rt.path, `{}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d want 503 body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "PostgreSQL") {
				t.Fatalf("503 body should name the missing data source: %s", rec.Body.String())
			}
		})
	}
}
