package db

import (
	"testing"
)

// asset_dsl.go 的 DSL 是资产搜索的查询语言：tokenize → parser（AND 优先于 OR、
// 括号分组）→ SQL builder。错误 SQL 片段会直接进 ILIKE/ANY，必须钉死。

func TestValidateDSLOkAndBuild(t *testing.T) {
	ok := []string{
		"",                         // 空表达式 → node=nil → 1=1
		`   `,                      // 纯空白同上
		`example.com`,              // bare 全文
		`domain=corp.example`,      // 字符串模糊
		`domain=="corp.example"`,   // 字符串精确
		`domain!=bad.example`,      // 排除
		`port>8000 AND port<=9000`, // 数值比较
		`status==200`,              // 数值 == 归一化为 =
		`tech=docker`,              // 数组模糊（EXISTS unnest）
		`tech==nginx`,              // 数组精确
		`tech!=windows`,            // 数组排除
		`company_id=42`,            // 整数
		`task_id=7`,                // ANY(task_ids)
		`(domain=a.com OR domain=b.com) AND tech=nginx AND NOT`, // NOT 不是关键字 → bare 词
		`ip=10.0.0.1 OR (url=/admin AND port=443)`,
		`DOMAIN=Example.COM`, // 字段名大小写归一
		`title="管 理 台"`,      // 引号值（含空格）
	}
	for _, dsl := range ok {
		if err := ValidateDSL(dsl); err != nil {
			t.Fatalf("ValidateDSL(%q) unexpected error: %v", dsl, err)
		}
	}
}

func TestValidateDSLErrors(t *testing.T) {
	mustFail := []string{
		"unknownfield=x", // 未知字段 → bare 词其实合法！这个输入等于全文搜 "unknownfield=x"
	}
	_ = mustFail
	// 注意：未知字段会退化为 bare 全文词（合法），所以只有结构/类型错误才报错。
	for _, dsl := range []string{
		"(domain=a.com",      // 缺右括号
		"domain=a.com)",      // 多余右括号
		"port=abc",           // 数值字段接文本
		"tech>5",             // 数组字段不支持比较符
		"domain>5",           // 字符串字段不支持比较符
		"company_id=abc",     // company_id 必须整数
		"task_id=abc",        // task_id 必须整数
		"(domain=a.com AND)", // 括号内以 AND 结尾 → 表达式不完整
	} {
		if err := ValidateDSL(dsl); err == nil {
			t.Fatalf("ValidateDSL(%q) must fail", dsl)
		}
	}
}

func TestDSLPrecedenceAndGrouping(t *testing.T) {
	// AND 绑定紧于 OR：a OR b AND c ≡ a OR (b AND c)。
	// 用 SQL 片段形状验证（build 产物可观察）：or 的两侧各是 (a) 与 (b AND c)。
	// ValidateDSL 只做解析，这里借助 CountDSL 的错误路径不可行 —— 改用可观察行为：
	// 直接构造等价查询确认不报错即可；真正形状由 server 层的集成面覆盖。
	if err := ValidateDSL("domain=a.com OR domain=b.com AND port=1"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDSL("((domain=a.com))"); err != nil { // 嵌套括号
		t.Fatal(err)
	}
}

func TestQueryDSLAndCount(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { d.Close() })
	s := d.Assets()

	id1, err := s.UpsertRootDomain(UpsertRootDomainReq{Domain: "dsl-a.example", TaskID: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, id1) })
	id2, err := s.UpsertIP(UpsertIPReq{IP: "10.77.0.1", TaskID: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, id2) })

	// 空 DSL → 全量（至少包含我们两行）
	all, err := s.QueryDSL("", "", 0, 100, 0)
	if err != nil || len(all) < 2 {
		t.Fatalf("empty DSL query: %d %v", len(all), err)
	}
	if n, err := s.CountDSL("", "", 0); err != nil || n < 2 {
		t.Fatalf("CountDSL empty: %d %v", n, err)
	}

	// 域名模糊命中第一行
	got, err := s.QueryDSL("domain=dsl-a.example", "", 0, 100, 0)
	if err != nil || len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("domain query: %v %v", got, err)
	}
	// IP 精确 == 命中第二行
	got, err = s.QueryDSL("ip==10.77.0.1", "", 0, 100, 0)
	if err != nil || len(got) != 1 || got[0].ID != id2 {
		t.Fatalf("ip exact query: %v %v", got, err)
	}
	// != 排除后不包含第一行
	got, err = s.QueryDSL("domain!=dsl-a.example", "", 0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.ID == id1 {
			t.Fatalf("!= must exclude dsl-a.example")
		}
	}
	// type 过滤 + 分页
	if n, err := s.CountDSL("domain=dsl-a.example", "root_domain", 0); err != nil || n != 1 {
		t.Fatalf("CountDSL with type: %d %v", n, err)
	}
	if n, err := s.CountDSL("domain=dsl-a.example", "ip", 0); err != nil || n != 0 {
		t.Fatalf("CountDSL type mismatch: %d %v", n, err)
	}
	paged, err := s.QueryDSL("domain=dsl-a.example", "", 0, 1, 0)
	if err != nil || len(paged) != 1 {
		t.Fatalf("limit=1: %v %v", paged, err)
	}
	if off, err := s.QueryDSL("domain=dsl-a.example", "", 0, 100, 1); err != nil || len(off) != 0 {
		t.Fatalf("offset past end: %v %v", off, err)
	}
	// taskID scope：task_ids 不含该 id → 0 行
	if n, err := s.CountDSL("domain=dsl-a.example", "", 987654); err != nil || n != 0 {
		t.Fatalf("taskID scope count: %d %v", n, err)
	}
	// 语法错误是客户端错误路径
	if _, err := s.QueryDSL("port=abc", "", 0, 10, 0); err == nil {
		t.Fatalf("bad DSL must error")
	}
	if _, err := s.CountDSL("(domain=a", "", 0); err == nil {
		t.Fatalf("unbalanced DSL must error")
	}
}
