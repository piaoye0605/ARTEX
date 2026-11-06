package traffic

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func trafficTool(t *testing.T, tr *Traffic, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range tr.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("未找到工具 %s", name)
	return nil
}

func callTool(t *testing.T, tool actool.CoreTool, args string) actool.Result {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("%s 调用异常: %v", tool.Name(), err)
	}
	return res
}

// TestToolsCatalog: 三个工具都要有名字与描述（描述会写进工具目录，供模型选）。
func TestToolsCatalog(t *testing.T) {
	tr, _ := openTraffic(t)
	tools := tr.Tools()
	if len(tools) != 3 {
		t.Fatalf("工具数 = %d, want 3", len(tools))
	}
	for _, tool := range tools {
		if tool.Name() == "" || tool.Description() == "" {
			t.Fatalf("工具元数据缺失: %q / %q", tool.Name(), tool.Description())
		}
		if !tool.IsReadOnly(nil) {
			t.Fatalf("%s 应声明为只读", tool.Name())
		}
		if d := tool.CheckPermissions(context.Background(), nil, permission.Context{}); d.Behavior != permission.Allow {
			t.Fatalf("%s 应直接放行（只读查询），得到 %+v", tool.Name(), d)
		}
	}

	// 目录播种用零值接收者，只有元数据会被使用。
	seeded := SeedToolMetas()
	if len(seeded) != len(tools) {
		t.Fatalf("播种工具数 = %d, want %d", len(seeded), len(tools))
	}
	for i := range seeded {
		if seeded[i].Name() != tools[i].Name() {
			t.Fatalf("播种顺序不一致: %q vs %q", seeded[i].Name(), tools[i].Name())
		}
	}
	// 目录里的描述必须与运行期一致，否则升级后提示词会漂移。
	for _, tool := range seeded {
		if tool.Name() == "traffic_search" && tool.Description() != TrafficSearchDescription {
			t.Fatal("traffic_search 目录描述与运行期描述不一致")
		}
	}
}

func TestTrafficSearchTool(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("search.example", "GET", "/api/users", nil, []byte(`{"secret":"needle-in-haystack"}`)))
	tr.record(newFlow("search.example", "POST", "/login", []byte("user=admin&pass=P@ssw0rd"), []byte("ok")))
	tr.record(newFlow("other.example", "GET", "/", nil, []byte("other")))

	search := trafficTool(t, tr, "traffic_search")

	// host 必填：不带 host 会退化成全库扫描，必须挡在前面。
	res := callTool(t, search, `{}`)
	if !res.IsError || !strings.Contains(res.Flatten(), "host 为必填参数") {
		t.Fatalf("缺 host 应报错: %v %q", res.IsError, res.Flatten())
	}
	// 无法解析的 host 也要报错，而不是静默返回空。
	res = callTool(t, search, `{"host":"http:///nohost"}`)
	if !res.IsError || !strings.Contains(res.Flatten(), "无法解析 host") {
		t.Fatalf("非法 host 应报错: %v %q", res.IsError, res.Flatten())
	}
	// 端口越界。
	res = callTool(t, search, `{"host":"search.example:99999"}`)
	if !res.IsError || !strings.Contains(res.Flatten(), "端口无效") {
		t.Fatalf("非法端口应报错: %v %q", res.IsError, res.Flatten())
	}

	type lite struct {
		ID      string `json:"id"`
		Method  string `json:"method"`
		URL     string `json:"url"`
		Status  int    `json:"status"`
		RespLen int    `json:"resp_len"`
	}
	res = callTool(t, search, `{"host":"search.example","limit":10}`)
	if res.IsError {
		t.Fatalf("查询失败: %q", res.Flatten())
	}
	var rows []lite
	if err := json.Unmarshal([]byte(res.Flatten()), &rows); err != nil {
		t.Fatalf("结果不是合法 JSON: %v / %q", err, res.Flatten())
	}
	if len(rows) != 2 {
		t.Fatalf("search.example 应命中 2 条，得到 %d", len(rows))
	}
	// 极轻量索引：不能把响应正文塞进结果。
	if strings.Contains(res.Flatten(), "needle-in-haystack") {
		t.Fatal("搜索结果不应包含正文")
	}

	// URL 子串过滤。
	res = callTool(t, search, `{"host":"search.example","contains":"login"}`)
	if err := json.Unmarshal([]byte(res.Flatten()), &rows); err != nil || len(rows) != 1 || rows[0].Method != "POST" {
		t.Fatalf("contains 过滤不对: %d rows, err=%v", len(rows), err)
	}

	// 正文全文搜索（trigram，至少 3 个字符）。
	res = callTool(t, search, `{"host":"search.example","body_contains":"P@ssw0rd"}`)
	if err := json.Unmarshal([]byte(res.Flatten()), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("正文搜索应命中请求体: %d rows, err=%v / %q", len(rows), err, res.Flatten())
	}

	// 无匹配时给明确文案，而不是空数组。
	res = callTool(t, search, `{"host":"nobody.example"}`)
	if res.IsError || !strings.Contains(res.Flatten(), "无匹配流量") {
		t.Fatalf("无匹配应给出提示: %v %q", res.IsError, res.Flatten())
	}
}

func TestTrafficGetAndBlobTools(t *testing.T) {
	tr, _ := openTraffic(t)
	small := newFlow("get.example", "GET", "/small", nil, []byte("small-body"))
	tr.record(small)
	rows, err := tr.Search("get.example", 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("准备数据失败: %d err=%v", len(rows), err)
	}
	id := rows[0].ID

	get := trafficTool(t, tr, "traffic_get")
	res := callTool(t, get, `{"id":"`+id+`"}`)
	text := res.Flatten()
	if res.IsError || !strings.Contains(text, "=== REQUEST ===") || !strings.Contains(text, "small-body") {
		t.Fatalf("traffic_get 输出不完整: %v %q", res.IsError, text)
	}
	res = callTool(t, get, `{"id":"missing"}`)
	if !res.IsError {
		t.Fatalf("不存在的 id 应报错: %q", res.Flatten())
	}

	// 大正文落到 blob，用 traffic_blob 分段读。
	big := bytes.Repeat([]byte("0123456789"), 40*1024) // 400KB > maxInlineBody
	tr.record(newFlow("blobtool.example", "GET", "/bin", nil, big, withRespType("text/plain")))
	var hash string
	if err := tr.DB().QueryRow(`SELECT resp_blob FROM exchange_bodies WHERE resp_blob IS NOT NULL LIMIT 1`).Scan(&hash); err != nil {
		t.Fatalf("没有生成 blob: %v", err)
	}

	blob := trafficTool(t, tr, "traffic_blob")
	res = callTool(t, blob, `{"hash":"`+hash+`"}`)
	text = res.Flatten()
	if res.IsError || !strings.Contains(text, "[offset=0") || !strings.Contains(text, "总长=") {
		t.Fatalf("blob 首段读取失败: %v %q", res.IsError, text)
	}
	// length 超上限要被夹到 maxBlobRead，不能把整段 400KB 倒出来。
	if strings.Count(text, "0123456789") > maxBlobRead/10+1 {
		t.Fatalf("单次读取没有被限额: %d 次重复", strings.Count(text, "0123456789"))
	}
	// 偏移越界给明确提示。
	res = callTool(t, blob, `{"hash":"`+hash+`","offset":99999999}`)
	if res.IsError || !strings.Contains(res.Flatten(), "已超出内容长度") {
		t.Fatalf("越界偏移应提示: %v %q", res.IsError, res.Flatten())
	}
	// 非法哈希。
	res = callTool(t, blob, `{"hash":"not-a-hash"}`)
	if !res.IsError {
		t.Fatalf("非法哈希应报错: %q", res.Flatten())
	}

	// 二进制 blob 以十六进制预览，不能把乱码塞进上下文。
	binaryBody := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0x00, 0x01}, maxInlineBody)...)
	tr.record(newFlow("bintool.example", "GET", "/img", nil, binaryBody, withRespType("image/png")))
	var binHash string
	if err := tr.DB().QueryRow(`SELECT resp_blob FROM exchange_bodies WHERE resp_blob IS NOT NULL ORDER BY rowid DESC LIMIT 1`).Scan(&binHash); err != nil {
		t.Fatalf("没有生成二进制 blob: %v", err)
	}
	res = callTool(t, blob, `{"hash":"`+binHash+`"}`)
	if res.IsError || !strings.Contains(res.Flatten(), "二进制内容") {
		t.Fatalf("二进制 blob 应以十六进制展示: %v %q", res.IsError, res.Flatten())
	}
}
