package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// This file pins the deliverable-report renderer (report.go): severity ordering,
// placeholder fallbacks, and the evidence/PoC blocks. It is pure string building —
// no database, no clock — so every assertion here is exact.

var reportAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func findingNode(payload string) *db.Node {
	return &db.Node{Kind: "finding", Payload: json.RawMessage(payload)}
}

func TestMarkdownRendersOrderedFindings(t *testing.T) {
	in := Input{
		Title:       "acme 渗透",
		Goal:        "拿到 admin",
		GeneratedAt: reportAt,
		AssetCounts: map[string]int{"ip": 1, "domain": 2},
		Findings: []*db.Node{
			findingNode(`{"vulnclass":"xss","name":"反射型 XSS","severity":"low","summary":"低危汇总"}`),
			findingNode(`{"vulnclass":"sqli","name":"SQL 注入","severity":"critical","summary":"严重汇总","evidence":{"poc":"curl -d id=1 http://x"}}`),
			findingNode(`{"name":"无等级发现","summary":"没有 severity 字段"}`),
		},
	}
	out := Markdown(in)

	wantHeader := "# 渗透测试报告 — acme 渗透\n\n" +
		"- **任务目标**：拿到 admin\n" +
		"- **生成时间**：2026-01-02 03:04:05\n\n" +
		"## 摘要\n\n" +
		"- 确认发现：**3** 个\n" +
		"- 资产：domain 2、ip 1\n\n" +
		"## 发现\n\n"
	if !strings.HasPrefix(out, wantHeader) {
		t.Fatalf("header mismatch:\n--- got ---\n%q\n--- want ---\n%q", out, wantHeader)
	}

	// Severity order is sevRank (critical→high→medium→low→""); a payload without
	// severity ranks as "" (least severe, after low) and falls back to the INFO label.
	crit := strings.Index(out, "### 1. [CRITICAL] SQL 注入")
	low := strings.Index(out, "### 2. [LOW] 反射型 XSS")
	info := strings.Index(out, "### 3. [INFO] 无等级发现")
	if crit < 0 || low < 0 || info < 0 || !(crit < low && low < info) {
		t.Fatalf("finding order wrong (crit=%d low=%d info=%d):\n%s", crit, low, info, out)
	}

	// PoC renders as a fenced block only when present.
	if !strings.Contains(out, "**PoC / 证据：**\n\n```\ncurl -d id=1 http://x\n```\n\n") {
		t.Fatalf("poc block missing:\n%s", out)
	}
	if got := strings.Count(out, "**PoC / 证据：**"); got != 1 {
		t.Fatalf("poc block count=%d", got)
	}
}

func TestMarkdownFallsBackForBlankFields(t *testing.T) {
	out := Markdown(Input{Title: "   ", Goal: "\t", GeneratedAt: reportAt})
	for _, want := range []string{
		"# 渗透测试报告 — 未命名任务",
		"- **任务目标**：（未指定）",
		"- 确认发现：**0** 个",
		"- 资产：\n\n",
		"## 发现\n\n_本次未确认漏洞。_\n\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// No findings → no detail headings at all.
	if strings.Contains(out, "### 1.") {
		t.Fatalf("unexpected finding section:\n%s", out)
	}
}

func TestMarkdownFindingTitleFallbackChain(t *testing.T) {
	// name wins, then vulnclass, then the literal 未分类.
	out := Markdown(Input{
		GeneratedAt: reportAt,
		Findings: []*db.Node{
			findingNode(`{"vulnclass":"sqli","name":"名称优先","severity":"high"}`),
			findingNode(`{"vulnclass":"仅类别","severity":"medium"}`),
			findingNode(`{"severity":"low"}`),
		},
	})
	if !strings.Contains(out, "[HIGH] 名称优先") {
		t.Fatalf("name not preferred:\n%s", out)
	}
	if !strings.Contains(out, "[MEDIUM] 仅类别") || !strings.Contains(out, "[LOW] 未分类") {
		t.Fatalf("fallback chain broken:\n%s", out)
	}
}

func TestParseFindingToleratesBadPayload(t *testing.T) {
	if v := parseFinding(findingNode(`not-json`)); v != (findingView{}) {
		t.Fatalf("bad payload decoded to %+v", v)
	}
	// A nil payload must not panic and yields the empty view.
	if v := parseFinding(&db.Node{}); v != (findingView{}) {
		t.Fatalf("nil payload decoded to %+v", v)
	}
	v := parseFinding(findingNode(`{"vulnclass":"xss","name":"n","severity":"high","summary":"s","evidence":{"poc":"p"}}`))
	if v.VulnClass != "xss" || v.Name != "n" || v.Severity != "high" || v.Summary != "s" || v.PoC != "p" {
		t.Fatalf("decoded %+v", v)
	}
}

func TestNZTrimSemantics(t *testing.T) {
	cases := []struct{ in, def, want string }{
		{"", "d", "d"},
		{"   ", "d", "d"},
		{"\n\t", "d", "d"},
		{"x", "d", "x"},
		{"  x  ", "d", "  x  "}, // only blankness is trimmed, not the value itself
	}
	for _, c := range cases {
		if got := nz(c.in, c.def); got != c.want {
			t.Fatalf("nz(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
