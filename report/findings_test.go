package report

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// This file pins the export renderers (findings.go) used by the findings page:
// summary/single Markdown, the CSV sheet, the zip-safe filename, and the traffic
// evidence block (including the stale-report warning).

var exportAt = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

func sampleFinding() *db.DBFinding {
	return &db.DBFinding{
		ID:                    7,
		VulnClass:             "sqli",
		Name:                  "登录处 SQL 注入",
		Severity:              "high",
		Status:                "confirmed",
		Summary:               "布尔盲注可绕过鉴权",
		Evidence:              "POST /login id=1' AND 1=1--",
		Report:                "## 复现\n\npayload 见证据。",
		CreatedAt:             exportAt,
		TaskDescription:       "acme 黑盒",
		EvidenceVersion:       3,
		ReportEvidenceVersion: 3,
	}
}

func TestSortFindingsForExportOrdering(t *testing.T) {
	older := exportAt.Add(-time.Hour)
	fs := []*db.DBFinding{
		{ID: 1, Severity: "low", CreatedAt: exportAt},
		{ID: 2, Severity: "critical", CreatedAt: older},
		{ID: 3, Severity: "", CreatedAt: exportAt},
		{ID: 4, Severity: "high", CreatedAt: older},
		{ID: 5, Severity: "high", CreatedAt: exportAt},
	}
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	// Rank first (critical→high→low→""), then newest-first inside the same rank.
	wantIDs := []int64{2, 5, 4, 1, 3}
	for i, want := range wantIDs {
		if items[i].ID != want {
			t.Fatalf("position %d: got id=%d want %d (order=%v)", i, items[i].ID, want, idsOf(items))
		}
	}
	// The caller's slice must not be reordered in place.
	if idsOf(fs)[0] != 1 || idsOf(fs)[1] != 2 {
		t.Fatalf("source slice mutated: %v", idsOf(fs))
	}
}

func idsOf(fs []*db.DBFinding) []int64 {
	out := make([]int64, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func TestFindingsMarkdownSummaryAndDetail(t *testing.T) {
	low := &db.DBFinding{
		ID: 9, VulnClass: "info-leak", Severity: "low",
		CreatedAt: exportAt.Add(-time.Hour),
	}
	out := FindingsMarkdown([]*db.DBFinding{low, sampleFinding()}, exportAt)

	head := "# 漏洞发现汇总报告\n\n" +
		"- **生成时间**：2026-02-03 04:05:06\n" +
		"- **发现总数**：2 个\n\n" +
		"## 摘要\n\n" +
		"| 严重等级 | 数量 |\n| --- | --- |\n" +
		"| 严重 | 0 |\n| 高危 | 1 |\n| 中危 | 0 |\n| 低危 | 1 |\n| 信息 | 0 |\n\n" +
		"## 漏洞明细\n\n"
	if !strings.HasPrefix(out, head) {
		t.Fatalf("header mismatch:\n--- got ---\n%q\n--- want ---\n%q", out, head)
	}

	// high before low; each entry carries 类别/状态/所属任务/时间, then the body blocks.
	if strings.Index(out, "### 1. [HIGH] 登录处 SQL 注入") > strings.Index(out, "### 2. [LOW] info-leak") {
		t.Fatalf("detail order wrong:\n%s", out)
	}
	for _, want := range []string{
		"- **类别**：sqli\n",
		"- **状态**：confirmed\n",
		"- **所属任务**：acme 黑盒\n",
		"- **发现时间**：2026-02-03 04:05:06\n\n",
		"布尔盲注可绕过鉴权\n\n",
		"**证据：**\n\n```\nPOST /login id=1' AND 1=1--\n```\n\n",
		"**详细报告：**\n\n## 复现\n\npayload 见证据。\n\n",
		"---\n\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// The bare low-severity finding has no task; status defaults to pending.
	if !strings.Contains(out, "### 2. [LOW] info-leak\n\n- **类别**：info-leak\n- **状态**：pending\n") {
		t.Fatalf("defaults missing:\n%s", out)
	}
	if strings.Contains(out, "- **类别**：\n") {
		t.Fatalf("empty class line rendered:\n%s", out)
	}
}

func TestFindingsMarkdownEmptySet(t *testing.T) {
	out := FindingsMarkdown(nil, exportAt)
	if !strings.Contains(out, "- **发现总数**：0 个") {
		t.Fatalf("count missing:\n%s", out)
	}
	if !strings.HasSuffix(out, "_无匹配的漏洞。_\n") {
		t.Fatalf("empty tail missing:\n%q", out)
	}
	if strings.Contains(out, "## 漏洞明细") {
		t.Fatalf("detail section rendered for empty set:\n%s", out)
	}
}

func TestFindingTrafficMarkdownBranches(t *testing.T) {
	// Nothing to say: no bindings and no stale report.
	if got := findingTrafficMarkdown(&db.DBFinding{}, true); got != "" {
		t.Fatalf("empty traffic block=%q", got)
	}

	binding := db.FindingTrafficBinding{
		ID:   42,
		Role: "request",
		Note: "重放时\n改 host",
		Snapshot: db.TrafficEvidenceSnapshot{
			Method: "POST",
			URL:    "http://a/`x`",
			Status: 302,
		},
	}
	f := &db.DBFinding{ID: 7, EvidenceVersion: 5, TrafficBindings: []db.FindingTrafficBinding{binding}}
	out := findingTrafficMarkdown(f, false)
	for _, want := range []string{
		"\n## 关联流量证据\n\n",
		"证据版本：5；绑定数量：1。\n\n",
		"1. **证据 #42 · request** — `POST http://a/%60x%60`，状态码 302\n",
		"   重放时\n   改 host\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%q", want, out)
		}
	}
	if strings.Contains(out, "evidence/7/42") {
		t.Fatalf("attachment links rendered for attachments=false:\n%s", out)
	}
	if strings.Contains(out, "证据已变更") {
		t.Fatalf("stale warning for a fresh report:\n%s", out)
	}

	// Stale: a report exists but the evidence moved on since it was written.
	f.Report = "旧报告"
	f.ReportEvidenceVersion = 4
	f.TrafficBindings = nil
	stale := findingTrafficMarkdown(f, true)
	// Report non-empty + version drift is enough to emit the section, even with 0 bindings.
	if !strings.Contains(stale, "证据版本：5；绑定数量：0。") || !strings.Contains(stale, "证据已变更，详细报告待更新。") {
		t.Fatalf("stale block=%q", stale)
	}
}

func TestSingleFindingMarkdownWithAttachments(t *testing.T) {
	f := sampleFinding()
	f.TrafficBindings = []db.FindingTrafficBinding{{
		ID: 11, Role: "response", Snapshot: db.TrafficEvidenceSnapshot{Method: "GET", URL: "http://a/x", Status: 200},
	}}
	out := SingleFindingMarkdown(f, exportAt)

	head := "# [HIGH] 登录处 SQL 注入\n\n" +
		"- **类别**：sqli\n" +
		"- **严重等级**：high\n" +
		"- **状态**：confirmed\n" +
		"- **所属任务**：acme 黑盒\n" +
		"- **发现时间**：2026-02-03 04:05:06\n" +
		"- **生成时间**：2026-02-03 04:05:06\n\n" +
		"## 概述\n\n布尔盲注可绕过鉴权\n\n"
	if !strings.HasPrefix(out, head) {
		t.Fatalf("header mismatch:\n--- got ---\n%q\n--- want ---\n%q", out, head)
	}
	if !strings.Contains(out, "## 证据\n\n```\nPOST /login id=1' AND 1=1--\n```\n\n") {
		t.Fatalf("evidence section:\n%s", out)
	}
	// Single-file rendering uses a newline (not a blank line) after the report body.
	if !strings.Contains(out, "## 详细报告\n\n## 复现\n\npayload 见证据。\n") {
		t.Fatalf("report section:\n%s", out)
	}
	for _, want := range []string{
		"   [请求报文](evidence/7/11/request.http) · [响应报文](evidence/7/11/response.http)\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSingleFindingMarkdownDefaults(t *testing.T) {
	out := SingleFindingMarkdown(&db.DBFinding{ID: 1}, exportAt)
	if !strings.HasPrefix(out, "# [INFO] 未分类\n\n") {
		t.Fatalf("defaults not applied:\n%s", out)
	}
	for _, want := range []string{"- **严重等级**：info\n", "- **状态**：pending\n", "- **发现时间**：0001-01-01 00:00:00\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Blank optional blocks are skipped entirely.
	if strings.Contains(out, "## 概述") || strings.Contains(out, "## 证据") || strings.Contains(out, "## 详细报告") {
		t.Fatalf("empty sections rendered:\n%s", out)
	}
}

func TestFindingFilenameSanitises(t *testing.T) {
	cases := []struct {
		name  string
		in    db.DBFinding
		exact string
	}{
		// The `#id` suffix survives the sanitiser (it is on the allow-list), which is
		// what the doc comment above FindingFilename promises.
		{name: "plain", in: db.DBFinding{ID: 3, Severity: "high", Name: "a b"}, exact: "high_a_b_#3.md"},
		{name: "path traversal", in: db.DBFinding{ID: 4, Severity: "critical", Name: "../../etc/passwd"}, exact: "critical_.._.._etc_passwd_#4.md"},
		{name: "defaults", in: db.DBFinding{ID: 0}, exact: "info_未分类_#0.md"},
	}
	for _, c := range cases {
		in := c.in
		if got := FindingFilename(&in); got != c.exact {
			t.Fatalf("%s: got %q want %q", c.name, got, c.exact)
		}
	}

	// Whatever the title, the result stays a single path segment ending in .md.
	nasty := &db.DBFinding{ID: 8, Severity: "low", Name: "a/b\\c\nd\x00e"}
	got := FindingFilename(nasty)
	if strings.ContainsAny(strings.TrimSuffix(got, ".md"), `/\`) || strings.Contains(got, "\x00") {
		t.Fatalf("unsafe filename %q", got)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Fatalf("missing extension: %q", got)
	}

	// Long titles are truncated so a zip entry stays under the filesystem limit.
	long := &db.DBFinding{ID: 9, Severity: "high", Name: strings.Repeat("A", 400)}
	trimmed := FindingFilename(long)
	if body := strings.TrimSuffix(trimmed, ".md"); len(body) != 120 {
		t.Fatalf("truncation length=%d (%q)", len(body), trimmed)
	}
}

func TestFindingsCSVLayout(t *testing.T) {
	f := sampleFinding()
	f.TrafficBindings = []db.FindingTrafficBinding{{ID: 11}, {ID: 12}}
	empty := &db.DBFinding{ID: 2, CreatedAt: exportAt, Summary: "  带,逗号的和\"引号\""}

	data := FindingsCSV([]*db.DBFinding{empty, f})
	if !bytes.HasPrefix(data, []byte("\xEF\xBB\xBF")) {
		t.Fatal("missing UTF-8 BOM (Excel would mangle the Chinese headers)")
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")))).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d (%v)", len(rows), rows)
	}
	header := []string{"ID", "名称", "类别", "严重等级", "状态", "所属任务", "发现时间", "概述", "流量证据数量", "流量证据ID"}
	for i, want := range header {
		if rows[0][i] != want {
			t.Fatalf("header[%d]=%q want %q", i, rows[0][i], want)
		}
	}
	// high (id 7) sorts before the severity-less row (id 2).
	if rows[1][0] != "7" || rows[2][0] != "2" {
		t.Fatalf("row order=%v", rows[1:])
	}
	wantRow := []string{"7", "登录处 SQL 注入", "sqli", "high", "confirmed", "acme 黑盒", "2026-02-03 04:05:06", "布尔盲注可绕过鉴权", "2", "11,12"}
	for i, want := range wantRow {
		if rows[1][i] != want {
			t.Fatalf("row[%d]=%q want %q (row=%v)", i, rows[1][i], want, rows[1])
		}
	}
	// Defaults + quoting: the summary keeps its comma/quote through the CSV escaping.
	if rows[2][1] != "未分类" || rows[2][3] != "info" || rows[2][4] != "pending" || rows[2][7] != `带,逗号的和"引号"` {
		t.Fatalf("default row=%v", rows[2])
	}
	if rows[2][8] != "0" || rows[2][9] != "" {
		t.Fatalf("empty traffic row=%v", rows[2])
	}
}

func TestFindingsCSVEmptySet(t *testing.T) {
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(FindingsCSV(nil)), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected header only, got %v", rows)
	}
}

func TestFindingsMarkdownSeverityBucketsMatchTotal(t *testing.T) {
	mk := func(id int64, sev string) *db.DBFinding {
		return &db.DBFinding{ID: id, Severity: sev, CreatedAt: exportAt}
	}
	out := FindingsMarkdown([]*db.DBFinding{
		mk(1, "critical"),
		mk(2, "INFO"),     // case-insensitive
		mk(3, ""),         // blank folds onto the info bucket
		mk(4, "  high  "), // trimmed
		mk(5, "hihg"),     // typo → 未分级
	}, exportAt)

	want := "| 严重 | 1 |\n| 高危 | 1 |\n| 中危 | 0 |\n| 低危 | 0 |\n| 信息 | 2 |\n| 未分级 | 1 |\n"
	if !strings.Contains(out, want) {
		t.Fatalf("summary table mismatch:/n%s", out)
	}
	// The table must account for every finding in the header total.
	if !strings.Contains(out, "- **发现总数**：5 个") {
		t.Fatalf("total missing:/n%s", out)
	}
	// Normalised labels: padding/case never leak into the heading.
	if !strings.Contains(out, "### 2. [HIGH] ") {
		t.Fatalf("padded severity not normalised:/n%s", out)
	}

	// 未分级 disappears once every finding lands in a known bucket.
	clean := FindingsMarkdown([]*db.DBFinding{mk(1, "critical"), mk(2, ""), mk(3, "info")}, exportAt)
	if strings.Contains(clean, "未分级") {
		t.Fatalf("leftover row rendered for a fully-classified set:/n%s", clean)
	}
	if !strings.Contains(clean, "| 信息 | 2 |\n") {
		t.Fatalf("blank severity not counted as info:/n%s", clean)
	}
}

func TestSortFindingsForExportNormalisesSeverity(t *testing.T) {
	items := []*db.DBFinding{
		{ID: 1, Severity: "hihg", CreatedAt: exportAt},   // unknown → last
		{ID: 2, Severity: "INFO", CreatedAt: exportAt},   // known, lowest bucket
		{ID: 3, Severity: " High ", CreatedAt: exportAt}, // normalises onto high
		{ID: 4, Severity: "critical", CreatedAt: exportAt},
	}
	sortFindingsForExport(items)
	for i, want := range []int64{4, 3, 2, 1} {
		if items[i].ID != want {
			t.Fatalf("position %d: got %d want %d (%v)", i, items[i].ID, want, idsOf(items))
		}
	}
}

func TestFindingFilenameKeepList(t *testing.T) {
	// `#` is kept (the `#id` suffix is part of the documented shape); the other
	// characters a title may legitimately contain get folded to one underscore.
	f := &db.DBFinding{ID: 5, Severity: "high", Name: "a?b*c:d\"e<f>g|h#i", VulnClass: "sqli"}
	got := FindingFilename(f)
	if got != "high_a_b_c_d_e_f_g_h#i_#5.md" {
		t.Fatalf("filename=%q", got)
	}
}
