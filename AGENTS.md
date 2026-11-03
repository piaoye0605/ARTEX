# ARTEX 项目约定（AI 编程智能体必读）

> 一句话：**AI 自主渗透测试系统** —— Go 1.26.3 单体后端 + Next.js 16.2.9 前端 + PostgreSQL（pgx v5）。
> 本仓库是上游 `Autumn-27/ARTEX` 的**终版源码备份**（459 次提交，2026-07-26 → 2026-10-10），上游已停止维护，重构改动不再需要跟进上游。

## 1. 代码地图

| 目录 | 职责 | 备注 |
|------|------|------|
| `cmd/artex/` | 程序入口、flag 解析 | 单二进制 |
| `server/` | HTTP 路由 + SSE + `Engine`（探索调度中枢） | `engine.go` 事件驱动 LLM 循环 |
| `agent/` | LLM agent、工具注册与执行 | `tools.go` 2054 行 |
| `db/` | **PostgreSQL 访问层**（pgx v5） | `exploration.go` 2021 行 |
| `traffic/` | 流量录制与检索索引 | **本地 sqlite `_index/index.sqlite`，非业务库** |
| `intercept/` | MITM 代理与证书 | 8788 端口 |
| `llmpool/` `enrich/` `guard/` `evidence/` `report/` `notify/` `mcphttp/` `config/` | 领域能力 | — |
| `web/` | Next.js 16 App Router 前端 | npm + Biome |

**热点文件（>2,000 行，重构主战场）**：`server/server.go` 3908、`server/server_mgmt.go` 2232、`traffic/traffic.go` 2130、`agent/tools.go` 2054、`db/exploration.go` 2021、`server/manager.go` 1860。

## 2. 硬规则（任何改动都不得违反）

1. **存储边界**：业务数据**只走 PostgreSQL**（`db/` 用 pgx）。`modernc.org/sqlite` 仅用于 `traffic/` 的本地检索索引，**禁止在 `db/` 引入 sqlite 分支或双方言抽象**。
2. **SQL 方言**：允许 PG 特性（`ILIKE`、`ON CONFLICT`、`RETURNING`、`JSONB`、`$1` 占位符）。新增 SQL 必须用参数化，不得字符串拼接用户输入。
3. **迁移**：新增表/列必须**幂等 + 可回滚**，改动后跑 `go test ./db/...`。
4. **测试**：现有 153 个测试文件**禁止删除、跳过或注释掉**。`enrich/` 与 `report/` 目前**零测试**，改动这两个包必须同步补测试。
5. **前端**：包管理器用 **npm**；lint/format 唯一工具是 **Biome**（`npm run check`），**禁止引入 ESLint / Prettier**；页面放在 `web/src/app/` 的 App Router 路由组下。
6. **端口**：`8787` 前端 + API + SSE（全网卡）；`8788` MITM 代理**仅绑 `127.0.0.1`**，不得改成 `0.0.0.0`。
7. **刻意设计不要"顺手修复"**：`enrich.Engine` 的 `InsecureSkipVerify: true`、MITM 证书、`traffic/` 的 sqlite 高水位保留等，都是渗透测试场景的刻意选择，改动前必须先说明理由并取得确认。
8. **重构纪律**：**搬运式重构** —— 只搬代码、不改行为；不顺手修 bug（发现就记下来，单独开 PR）；每个重构单元一个分支/一个 PR，附"行为未变"的证据（同一组测试在前后都绿）。

## 3. 环境与配置

| 变量 | 用途 |
|------|------|
| `ARTEX_PG_DSN` | PostgreSQL 连接串（唯一业务库） |
| `ARTEX_LLM_PROVIDER` / `ARTEX_LLM_BASE_URL` / `ARTEX_LLM_MODEL` | 引擎自身的 LLM 端点（Anthropic / OpenAI 兼容） |
| `ARTEX_LLM_PROXY` | 引擎出网代理 |
| `ARTEX_TARGETS` | 初始目标范围 |

参考：`.env.example`、`config.example.json`、`docker-compose.yml`（`POSTGRES_PASSWORD` 必填，缺失即启动失败）。

## 4. 验收命令（提交前必须全绿）

```bash
gofmt -l .                 # 应无输出
go build ./...             # 编译通过
go vet ./...               # 静态检查
go test ./...              # 全部测试通过（重构安全网）
cd web && npm run check    # Biome 检查
cd web && npm run build    # 前端构建
```

## 5. 与智能体协作的方式

- 回答用**中文**；结论先行，再给证据（`文件:行号`）。
- 动手前先给**改动文件清单 + 影响面 + 验收命令**，确认后再改。
- 不确定就**读代码**，禁止凭猜测编造函数名、字段名或行号。
- 涉及并发（`server/engine.go` 的 planner/worker/UI 三方并发、`sync.Map`、context 取消）的改动，必须先输出 diff 让人工确认。
- 不要修改 `data/`、`skills/` 下的运行时产物，也不要提交任何密钥。
