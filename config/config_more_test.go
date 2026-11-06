package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDSNFillsDefaults(t *testing.T) {
	// 只给用户名：其余全部走默认值，否则拼出来的 DSN 会连不上。
	got := Database{User: "artex"}.buildDSN()
	want := "postgres://artex@127.0.0.1:5432/?sslmode=disable"
	if got != want {
		t.Fatalf("buildDSN()=%q want %q", got, want)
	}

	// 有密码时走 userinfo 的 password 形式（并做 URL 转义）。
	got = Database{User: "u", Password: "p@ss word", DBName: "db", Port: 6543, SSLMode: "require"}.buildDSN()
	if !strings.Contains(got, "u:p%40ss%20word@") {
		t.Errorf("密码未转义: %q", got)
	}
	if !strings.Contains(got, ":6543/db?") || !strings.Contains(got, "sslmode=require") {
		t.Errorf("端口/库名/sslmode 拼装错误: %q", got)
	}

	// 完全没有用户名时不应带 userinfo 段。
	if got := (Database{DBName: "d"}).buildDSN(); strings.Contains(got, "@") {
		t.Errorf("无用户名时不该有 userinfo: %q", got)
	}
}

func TestRedactMasksPassword(t *testing.T) {
	got := Redact("postgres://u:secret@h:5432/d?sslmode=disable")
	if strings.Contains(got, "secret") {
		t.Fatalf("密码必须被打码: %q", got)
	}
	if !strings.Contains(got, "u:****@h:5432") {
		t.Errorf("打码格式不符合预期: %q", got)
	}

	// 没有密码（或没有 userinfo）时原样返回。
	for _, dsn := range []string{
		"postgres://u@h:5432/d",
		"postgres://h:5432/d",
	} {
		if got := Redact(dsn); got != dsn {
			t.Errorf("Redact(%q)=%q，不应改动", dsn, got)
		}
	}

	// 解析不了的串不能因为打码而丢信息（日志里要能看出用户填了什么）。
	const broken = "://bad"
	if got := Redact(broken); got != broken {
		t.Errorf("无法解析时应原样返回，得到 %q", got)
	}
}

func TestIsGoRunDir(t *testing.T) {
	// 系统临时目录下的可执行文件 = `go run` 的缓存未命中产物。
	if !isGoRunDir(os.TempDir()) {
		t.Error("临时目录应判定为 go run 目录")
	}
	if !isGoRunDir(filepath.Join(os.TempDir(), "go-build123", "b001")) {
		t.Error("临时目录子路径也应判定为 go run")
	}
	// 缓存命中时二进制直接跑在 GOCACHE 里，不在 TempDir 下——只认 go-build 段。
	// 这里用非临时目录构造，否则第一道 TempDir 判断就直接返回 true 了。
	if !isGoRunDir("/opt/artex/go-build/00/abcdef-d") {
		t.Error("路径中出现 go-build 应判定为 go run")
	}
	if isGoRunDir("/opt/artex/bin") {
		t.Error("普通目录不应判定为 go run")
	}
}

func TestBaseDirUsesExecutableDirUnlessGoRun(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("无法定位测试二进制: %v", err)
	}
	dir := filepath.Dir(exe)
	got := BaseDir()
	if isGoRunDir(dir) {
		// go test 的二进制同样在临时目录里，此时必须回退到 CWD，
		// 否则 data/ 与 config.json 会落到 build cache 里。
		if got != "." {
			t.Fatalf("go run/go test 场景应回退到 CWD，得到 %q", got)
		}
		return
	}
	if got != dir {
		t.Fatalf("BaseDir()=%q want %q", got, dir)
	}
}

func TestPathResolutionOrder(t *testing.T) {
	t.Run("环境变量优先", func(t *testing.T) {
		t.Setenv("ARTEX_CONFIG", "/explicit/config.json")
		if got := Path(); got != "/explicit/config.json" {
			t.Fatalf("Path()=%q", got)
		}
	})

	t.Run("当前目录的 config.json 优先于可执行文件旁", func(t *testing.T) {
		t.Setenv("ARTEX_CONFIG", "  ") // 空白视同未设置
		dir := t.TempDir()
		t.Chdir(dir)
		want := filepath.Join(dir, "config.json")
		if err := os.WriteFile(want, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Path(); got != want {
			t.Fatalf("Path()=%q want %q", got, want)
		}
	})

	t.Run("都不存在时返回当前目录候选", func(t *testing.T) {
		t.Setenv("ARTEX_CONFIG", "")
		dir := t.TempDir()
		t.Chdir(dir)
		if got := Path(); got != filepath.Join(dir, "config.json") {
			t.Fatalf("Path()=%q，应指向当前目录（错误提示才对得上用户预期）", got)
		}
	})
}

func TestLoadTolerantOfMissingAndBrokenFiles(t *testing.T) {
	t.Setenv("ARTEX_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	if c := Load(); c.Database.DSN != "" || c.SkillDir != "" {
		t.Fatalf("缺失配置文件应返回零值，得到 %+v", c)
	}

	dir := t.TempDir()
	broken := filepath.Join(dir, "config.json")
	if err := os.WriteFile(broken, []byte(`{"database":{"port":"not-a-number"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", broken)
	// 解析失败不能报错：调用方一律退到默认值/环境变量。
	if c := Load(); c.Database.Port != 0 {
		t.Fatalf("损坏的配置文件应退化为零值，得到 %+v", c)
	}

	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte(`{"skill_dir":"/skills/x","database":{"dsn":"postgres://x/y"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", ok)
	c := Load()
	if c.SkillDir != "/skills/x" || c.Database.DSN != "postgres://x/y" {
		t.Fatalf("正常配置未解析: %+v", c)
	}
}

func TestSkillDirPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	t.Setenv("ARTEX_CONFIG", cfg)

	// 1) 环境变量最高
	fromEnv := filepath.Join(dir, "env-skills")
	t.Setenv("ARTEX_SKILL_DIR", fromEnv)
	if got := SkillDir(); got != fromEnv {
		t.Fatalf("env 应优先，得到 %q", got)
	}
	// 目录不存在时必须自动创建，否则后续写 skill 会失败。
	if info, err := os.Stat(fromEnv); err != nil || !info.IsDir() {
		t.Fatalf("SkillDir 应创建目录: %v", err)
	}

	// 2) 配置文件次之
	fromFile := filepath.Join(dir, "file-skills")
	t.Setenv("ARTEX_SKILL_DIR", "  ")
	if err := os.WriteFile(cfg, []byte(`{"skill_dir":"`+strings.ReplaceAll(fromFile, `\`, `\\`)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SkillDir(); got != fromFile {
		t.Fatalf("配置文件次之，得到 %q", got)
	}

	// 3) 都没有 → BaseDir()/skills
	t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "absent.json"))
	got := SkillDir()
	if filepath.Base(got) != "skills" {
		t.Fatalf("默认应落在 <base>/skills，得到 %q", got)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("默认 skill 目录应被创建: %v", err)
	}
}

func TestPostgresDSNSourceLabels(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	t.Setenv("ARTEX_CONFIG", cfg)
	t.Setenv("ARTEX_PG_DSN", "postgres://env/db")
	if _, source, err := PostgresDSN(); err != nil || source != "环境变量 ARTEX_PG_DSN" {
		t.Fatalf("source=%q err=%v", source, err)
	}

	t.Setenv("ARTEX_PG_DSN", " ")
	if err := os.WriteFile(cfg, []byte(`{"database":{"dsn":"postgres://file/db"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn, source, err := PostgresDSN()
	if err != nil || dsn != "postgres://file/db" || !strings.Contains(source, "database.dsn") {
		t.Fatalf("dsn=%q source=%q err=%v", dsn, source, err)
	}

	// 字段拼装：source 要指明来自 database 字段，报错信息才会指向正确的配置段。
	if err := os.WriteFile(cfg, []byte(`{"database":{"host":"h","dbname":"d"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn, source, err = PostgresDSN()
	if err != nil || dsn != "postgres://h:5432/d?sslmode=disable" || !strings.Contains(source, "database 字段") {
		t.Fatalf("dsn=%q source=%q err=%v", dsn, source, err)
	}

	// 什么都没有：错误信息必须点名查过的文件，否则用户不知道去改哪里。
	if err := os.WriteFile(cfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = PostgresDSN()
	if err == nil || !strings.Contains(err.Error(), cfg) {
		t.Fatalf("错误信息应包含配置路径 %q，得到 %v", cfg, err)
	}
}
