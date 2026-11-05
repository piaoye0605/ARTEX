package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/traffic"
)

// Coverage for server_traffic.go with a real (sqlite-backed) ledger. The
// disabled branches are pinned by TestTrafficRouteGuards; here we seed the
// ledger directly through tr.DB() and walk every read/delete route.

func TestTrafficRoutesWithLedger(t *testing.T) {
	m, s := newTestStack(t)
	tr := m.Traffic()
	if tr == nil {
		// NewManager only opens traffic when a proxy address is configured;
		// the test stack runs proxy-less, so open a ledger directly. The
		// routes never touch the proxy — only the index — so this exercises
		// the same handler paths as production.
		var err error
		tr, err = traffic.Open(filepath.Join(m.dir, "traffic"), "127.0.0.1:0")
		if err != nil {
			t.Skipf("traffic unavailable (%v) — skipping", err)
		}
		m.traffic = tr
		t.Cleanup(func() { _ = tr.Close() })
	}

	// The capture toggle lives in settings (independent of the ledger existing);
	// switch it on so the list payload reflects capture-on state.
	if err := m.SetTrafficEnabled(true); err != nil {
		t.Fatalf("enable traffic: %v", err)
	}
	t.Cleanup(func() { _ = m.SetTrafficEnabled(false) })

	// Seed three exchanges (two on the same host) with inline bodies.
	seed := func(id, host, url, status string, reqBody, respBody string) {
		t.Helper()
		var st int
		if _, err := fmt.Sscanf(status, "%d", &st); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, time.Now().Unix(), host, "GET", url, "https://"+host+url, st,
			"application/json", len(reqBody), len(respBody), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,?,?,?,?)`, id, "GET "+url+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n", []byte(reqBody),
			"HTTP/1.1 "+status+"\r\nContent-Type: application/json\r\n\r\n", []byte(respBody)); err != nil {
			t.Fatal(err)
		}
	}
	seed("ex-a-1", "target.example.com", "/api/v1/users", "200", `{"q":1}`, `{"ok":true}`)
	seed("ex-a-2", "target.example.com", "/api/v1/keys", "404", `{}`, `missing`)
	seed("ex-b-1", "other.example.net", "/", "500", `{}`, `boom`)
	t.Cleanup(func() { _, _, _ = tr.DeleteAll() })

	// Paged list reflects the ledger and the capture toggle.
	rec := crudDo(t, s, http.MethodGet, "/api/traffic?page=0&size=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Enabled   bool   `json:"enabled"`
		Proxy     string `json:"proxy"`
		Count     int    `json:"count"`
		Total     int    `json:"total"`
		Exchanges []struct {
			ID     string `json:"id"`
			Host   string `json:"host"`
			Status int    `json:"status"`
			TS     string `json:"ts"`
		} `json:"exchanges"`
	}
	crudJSON(t, rec, &page)
	if !page.Enabled || page.Count != 3 || page.Total != 3 || len(page.Exchanges) != 3 {
		t.Fatalf("page=%+v", page)
	}
	// Newest first: the last-seeded exchange leads, and every row carries its
	// RFC3339 timestamp.
	if page.Exchanges[0].ID != "ex-b-1" || page.Exchanges[0].TS == "" {
		t.Fatalf("first row=%+v", page.Exchanges[0])
	}

	// Host filter narrows the result set.
	rec = crudDo(t, s, http.MethodGet, "/api/traffic?page=0&size=10&host=target.example", "")
	crudJSON(t, rec, &page)
	if page.Total != 2 || len(page.Exchanges) != 2 {
		t.Fatalf("filtered page=%+v", page)
	}

	// Hosts picker aggregates per-host counts.
	rec = crudDo(t, s, http.MethodGet, "/api/traffic/hosts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("hosts: status=%d", rec.Code)
	}
	var hosts struct {
		Hosts []struct {
			Host  string `json:"host"`
			Count int    `json:"count"`
		} `json:"hosts"`
	}
	crudJSON(t, rec, &hosts)
	if len(hosts.Hosts) != 2 {
		t.Fatalf("hosts=%+v", hosts.Hosts)
	}

	// Single exchange detail rebuilds the raw request/response.
	rec = crudDo(t, s, http.MethodGet, "/api/traffic/exchange?id=ex-a-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Req  string `json:"req"`
		Resp string `json:"resp"`
	}
	crudJSON(t, rec, &detail)
	if !strings.Contains(detail.Req, "GET /api/v1/users") || !strings.Contains(detail.Resp, `{"ok":true}`) {
		t.Fatalf("detail=%+v", detail)
	}

	// Missing id is 400; an exchange with no body row and no file path is 404.
	if rec := crudDo(t, s, http.MethodGet, "/api/traffic/exchange", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/traffic/exchange?id=nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Blob download: spill one body to the on-disk blob store and stream it.
	body := []byte(strings.Repeat("PAYLOAD", 1000))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(s.m.dir, "traffic", "_blobs", "sha256", hash[:2])
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hash+".bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	rec = crudDo(t, s, http.MethodGet, "/api/traffic/blob?hash="+hash, "")
	if rec.Code != http.StatusOK || rec.Body.Len() != len(body) {
		t.Fatalf("blob: status=%d len=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Fatalf("content-length=%q", rec.Header().Get("Content-Length"))
	}
	// Validation branches: missing hash 400, malformed/unknown hash 404.
	if rec := crudDo(t, s, http.MethodGet, "/api/traffic/blob", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("blob missing hash: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/traffic/blob?hash=deadbeef", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("blob bad hash: status=%d", rec.Code)
	}
	if rec := crudDo(t, s, http.MethodGet, "/api/traffic/blob?hash="+hash, ""); rec.Code != http.StatusOK {
		t.Fatalf("blob uppercase-hash path: status=%d", rec.Code)
	}

	// Substring host delete: "target.example" sweeps both target exchanges.
	rec = crudDo(t, s, http.MethodDelete, "/api/traffic?host=target.example", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("host delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var del struct {
		Deleted int64 `json:"deleted"`
	}
	crudJSON(t, rec, &del)
	if del.Deleted != 2 {
		t.Fatalf("deleted=%+v", del)
	}
	if rec := crudDo(t, s, http.MethodDelete, "/api/traffic", ""); rec.Code != http.StatusBadRequest {
		// deleteTraffic with an empty host query is 400.
		t.Fatalf("empty host delete: status=%d", rec.Code)
	}

	// Batch exact-host delete, then the full purge.
	rec = crudDo(t, s, http.MethodDelete, "/api/traffic/hosts", `{"hosts":["other.example.net"]}`)
	crudJSON(t, rec, &del)
	if del.Deleted != 1 {
		t.Fatalf("batch deleted=%+v", del)
	}
	rec = crudDo(t, s, http.MethodDelete, "/api/traffic/all", "")
	crudJSON(t, rec, &del)
	if del.Deleted != 0 {
		t.Fatalf("purge deleted=%+v", del)
	}

	// The ledger is empty again.
	rec = crudDo(t, s, http.MethodGet, "/api/traffic?page=0&size=10", "")
	crudJSON(t, rec, &page)
	if page.Count != 0 || len(page.Exchanges) != 0 {
		t.Fatalf("post-purge page=%+v", page)
	}
}
