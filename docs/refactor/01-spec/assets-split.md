# 拆分规格 · 单元 6：db/assets.go

> 状态：已执行（本文档为事后留档，分组与实现一致）
> 方法：纯同包声明搬移（同单元 1~5）；脚本 `tools/split_assets.py`。
> 原文件：1536 行、56 个顶层声明 → 拆分后 `assets.go` 186 行，新增 6 个同包文件。

## 一、域分组（56 个顶层声明）

| 目标文件 | 行数 | 域 | 声明数 |
|---|---|---|---|
| `assets.go`（保留） | 186 | 资产域类型基座（`Asset`/`AuthItem`/`ParamItem`/`PortService`）+ `AssetStore` 入口（`Assets`/`Companies`）+ 列清单 `assetSelectCols` 与扫描助手（`scanAssets`/`scanTypeCounts`） | 10 |
| `assets_service.go` | 388 | 服务与端点写入：`UpsertHTTPService`（含证书/指纹/技术栈合并）、`UpsertOtherService`、`UpsertEndpoint` | 6 |
| `assets_domain.go` | 290 | 域名与应用写入：`UpsertRootDomain`、`UpsertSubdomain`、`UpsertApp` 及其 Req 结构 | 6 |
| `assets_delete.go` | 213 | 删除与主机清单：`DeleteByTaskID`/`DeleteByCompanyID`/`DeleteByHost`/`DeleteByIDs`、`HostsByTask`/`HostsForTaskDeletion`（含 `rowsQuerier` 接口以复用事务内外查询） | 8 |
| `assets_query.go` | 181 | 查询与计数：`QueryByType`/`QueryByCompany`/`QueryByTask` 及对应 Count、`pageClause` 分页片段、`GetByIDs`、`CountsByType*` | 10 |
| `assets_host.go` | 162 | 主机/IP 写入：`ValidateAssetIP`/`ErrAssetIPInvalid`、`UpsertIP`、`AppendIPPort`、`AppendIPBoundDomain`、`linkHostAssets` | 7 |
| `assets_scope.go` | 154 | 归属与序列化助手：`withCompanyScopeMutation`、`resolveCompanyWithICP`、`calcCSegment`、`normalizeURL`/`parseURL`、`marshalJSONBArray`/`marshalStringArray`/`marshalPortServices`、`nullableInt` | 9 |

## 二、归组决策记录

- **`Asset` 等类型 + 扫描器留在 `assets.go`**：所有 6 个域文件都以 `*Asset` 为输入输出，`assetSelectCols` 是查询/删除共用的列清单；扫描器与列清单必须同文件，否则改列要跨文件找。
- **写入按"资产类型"切，查询按"查询维度"切**：这是本文件最核心的取舍。写入路径（`Upsert*`）每种类型有完全不同的合并逻辑（HTTP 服务的证书指纹、子域名的 ICP 归属、端点的参数合并），彼此不共享代码，按类型切最自然；查询路径则共享 `pageClause` 与 `assetSelectCols`，按维度切（type/company/task/ids）能一眼看出"有哪几种取数方式"。
- **`assets_scope.go` 独立**：`withCompanyScopeMutation` 是所有写入的公共前置（企业归属解析 + 串行化），`resolveCompanyWithICP`/`calcCSegment`/`parseURL` 是它的下游助手，单独成文件后"写入为什么要先算归属"这件事有了唯一落点。
- **`rowsQuerier` 接口留在 delete 域**：它只服务 `hostsForTaskDeletion` 在事务内外复用同一条 SQL；放 delete 域避免基础文件出现业务性接口。
- **不动 `asset_dsl.go` / `asset_intercept*.go` / `finding_assets.go`**：均是既有独立文件，本次不纳入。

## 三、验收命令

```bash
go build ./... && go vet ./db/
# 等价性：非 import/package/空行 的行集合 Counter 对比，MISSING/ADDED 必须均为 0
go test ./db/ -count=1
go test ./... -count=1
```

## 四、等价性实测

拆分前后非空非导入行 **1419 = 1419，MISSING 0 / ADDED 0**。

> **脚本坑（本次新增）**：首个声明前若存在与声明之间隔着空行的文件横幅注释（本文件的 `// ===== / // 统一资产表 / // =====`），脚本按"声明前连续 `//` 行"回溯时取不到它，重建 `assets.go` 时会随头部一起丢弃。首轮等价性校验 MISSING 3 行即由此暴露，已手工补回。后续单元若首轮差值为 2~3 行纯注释，先查这里。
