package traffic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadEvidenceInlineBodies 是证据导出的常态路径：正文就在 SQLite 里，读出来
// 必须与抓到的完全一致，头部也要带上原始请求行/状态行。
func TestReadEvidenceInlineBodies(t *testing.T) {
	tr, _ := openTraffic(t)
	reqBody := []byte(`{"user":"admin"}`)
	respBody := []byte(`{"ok":true}`)
	tr.record(newFlow("ev.example", "POST", "/login", reqBody, respBody))
	id := onlyExchangeID(t, tr)

	var got EvidenceExchange
	err := tr.ReadEvidence(context.Background(), []string{id}, func(e EvidenceExchange) error {
		got = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.URL != "http://ev.example/login" || got.Method != "POST" || got.Status != 200 {
		t.Fatalf("证据元数据不对: %+v", got)
	}
	if got.ReqLen != int64(len(reqBody)) || got.RespLen != int64(len(respBody)) {
		t.Fatalf("正文长度不对: req=%d resp=%d", got.ReqLen, got.RespLen)
	}
	if !strings.Contains(got.ReqHead, "POST /login") || !strings.Contains(got.RespHead, "HTTP 200") {
		t.Fatalf("报文头不完整: req=%q resp=%q", got.ReqHead, got.RespHead)
	}
	reqRaw, _ := io.ReadAll(got.Request)
	respRaw, _ := io.ReadAll(got.Response)
	if !bytes.Equal(reqRaw, reqBody) || !bytes.Equal(respRaw, respBody) {
		t.Fatalf("正文回读不一致: req=%q resp=%q", reqRaw, respRaw)
	}
	if got.ReqHash != "" || got.RespHash != "" {
		t.Fatalf("内联正文不应有 blob 哈希: %+v", got)
	}
}

// TestReadEvidenceBlobBodies: 超过内联阈值的正文落在内容寻址的 blob 里，证据导出
// 必须流式读到完整内容，并给出哈希供冻结时校验。
func TestReadEvidenceBlobBodies(t *testing.T) {
	tr, _ := openTraffic(t)
	big := bytes.Repeat([]byte("A"), maxInlineBody+128)
	tr.record(newFlow("blob.example", "GET", "/big", big, big, withRespType("text/plain")))
	id := onlyExchangeID(t, tr)

	// 阅读器只在回调期间有效（读完后立刻关闭），所以必须在回调里读完。
	var reqRaw, respRaw []byte
	var reqHash, respHash string
	if err := tr.ReadEvidence(context.Background(), []string{id}, func(e EvidenceExchange) error {
		reqHash, respHash = e.ReqHash, e.RespHash
		reqRaw, _ = io.ReadAll(e.Request)
		respRaw, _ = io.ReadAll(e.Response)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reqHash == "" || reqHash != respHash {
		t.Fatalf("相同正文应共用同一个 blob 哈希: req=%q resp=%q", reqHash, respHash)
	}
	if len(reqRaw) != len(big) || len(respRaw) != len(big) {
		t.Fatalf("blob 正文长度 = %d/%d, want %d", len(reqRaw), len(respRaw), len(big))
	}
	if !bytes.Equal(reqRaw, big) || !bytes.Equal(respRaw, big) {
		t.Fatal("blob 正文内容与原始不一致")
	}
}

// TestReadEvidenceMultipleIDsAndErrors 覆盖遍历语义：一个 id 失败要能定位到具体
// 流量，context 取消要立刻停下，消费方报错要原样带出。
func TestReadEvidenceMultipleIDsAndErrors(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("multi.example", "GET", "/1", nil, []byte("one")))
	tr.record(newFlow("multi.example", "GET", "/2", nil, []byte("two")))
	rows, err := tr.Search("multi.example", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("准备数据失败，得到 %d 条", len(rows))
	}
	ids := []string{rows[0].ID, rows[1].ID}

	seen := 0
	if err := tr.ReadEvidence(context.Background(), ids, func(EvidenceExchange) error {
		seen++
		return nil
	}); err != nil || seen != 2 {
		t.Fatalf("多个 id 应逐个回调，seen=%d err=%v", seen, err)
	}

	// 不存在的 id：错误里要带上是哪一条流量。
	err = tr.ReadEvidence(context.Background(), []string{"no-such-id"}, func(EvidenceExchange) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "流量 no-such-id") {
		t.Fatalf("缺失 id 应报出具体流量: %v", err)
	}

	// context 已取消：一个都不该回调。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seen = 0
	if err := tr.ReadEvidence(ctx, ids, func(EvidenceExchange) error {
		seen++
		return nil
	}); !errors.Is(err, context.Canceled) || seen != 0 {
		t.Fatalf("取消后应直接返回 context.Canceled，seen=%d err=%v", seen, err)
	}

	// 消费方报错：包装后原样带出。
	sentinel := errors.New("consumer boom")
	err = tr.ReadEvidence(context.Background(), ids, func(EvidenceExchange) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("消费方错误应可 errors.Is 识别: %v", err)
	}
}

func TestReadEvidenceNilReceiver(t *testing.T) {
	var tr *Traffic
	if err := tr.ReadEvidence(context.Background(), []string{"x"}, func(EvidenceExchange) error { return nil }); err == nil {
		t.Fatal("空存储应返回错误而不是 panic")
	}
}

// TestReadEvidenceLegacyTree 覆盖升级前写入的目录式报文：索引行有 path、body 表
// 没有对应行时，必须回落到磁盘上的 request.http / response.http。
func TestReadEvidenceLegacyTree(t *testing.T) {
	tr, dir := openTraffic(t)
	const legacy = "legacy.example/0001"
	if err := os.MkdirAll(filepath.Join(dir, legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, legacy, "request.http"), "GET /old HTTP/1.1\nHost: legacy.example\n\nlegacy-request")
	writeFile(t, filepath.Join(dir, legacy, "response.http"), "HTTP/1.1 200 OK\n\nlegacy-response")
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('legacy-1',1,'legacy.example','GET','/old','http://legacy.example/old',200,'text/plain',14,15,?)`, legacy); err != nil {
		t.Fatal(err)
	}

	var got EvidenceExchange
	if err := tr.ReadEvidence(context.Background(), []string{"legacy-1"}, func(e EvidenceExchange) error {
		got = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.ReqHead, "Host: legacy.example") {
		t.Fatalf("旧报文头未读取: %q", got.ReqHead)
	}
	if !strings.Contains(got.RespHead, "200 OK") {
		t.Fatalf("旧响应头未读取: %q", got.RespHead)
	}
	reqRaw, _ := io.ReadAll(got.Request)
	if string(reqRaw) != "legacy-request" {
		t.Fatalf("旧请求正文 = %q", reqRaw)
	}

	// 越界路径必须拒绝：path 来自索引，不能让它指到流量目录之外。
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('legacy-escape',1,'x','GET','/','http://x/',200,'text/plain',0,0,'../escape')`); err != nil {
		t.Fatal(err)
	}
	err := tr.ReadEvidence(context.Background(), []string{"legacy-escape"}, func(EvidenceExchange) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "旧流量路径无效") {
		t.Fatalf("越界路径应被拒绝: %v", err)
	}

	// 索引说有旧报文、磁盘上却没了：报错而不是静默返回空正文。
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('legacy-gone',1,'x','GET','/','http://x/',200,'text/plain',0,0,'legacy.example/missing')`); err != nil {
		t.Fatal(err)
	}
	err = tr.ReadEvidence(context.Background(), []string{"legacy-gone"}, func(EvidenceExchange) error { return nil })
	if err == nil {
		t.Fatal("旧报文缺失应报错")
	}
}

func TestSplitLegacyEvidence(t *testing.T) {
	head, body, err := splitLegacyEvidence(strings.NewReader("GET / HTTP/1.1\nHost: a\n\npayload"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(head, "Host: a") {
		t.Fatalf("头部分割不对: %q", head)
	}
	rest, _ := io.ReadAll(body)
	if string(rest) != "payload" {
		t.Fatalf("正文分割不对: %q", rest)
	}

	// 只有头部、没有空行（文件在 header 处就结束）：头部照收，正文为空。
	head, body, err = splitLegacyEvidence(strings.NewReader("GET / HTTP/1.1\nHost: a"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(head, "Host: a") {
		t.Fatalf("EOF 前的内容也应算头部: %q", head)
	}
	if rest, _ := io.ReadAll(body); len(rest) != 0 {
		t.Fatalf("EOF 场景不应有正文: %q", rest)
	}

	// 头部没有终止空行且超大：必须截断报错，不能把整个文件读进内存。
	_, _, err = splitLegacyEvidence(strings.NewReader(strings.Repeat("X", 1<<20+16)))
	if err == nil || !strings.Contains(err.Error(), "旧流量报文头过大") {
		t.Fatalf("超大头部应报错: %v", err)
	}

	// 读取中途出错：错误原样返回。
	if _, _, err := splitLegacyEvidence(errReader{}); err == nil {
		t.Fatal("读取错误应向上返回")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
