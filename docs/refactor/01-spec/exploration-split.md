# 拆分规格 · 单元 5：db/exploration.go

> 状态：已执行（本文档为事后留档，分组与实现一致）
> 方法：纯同包声明搬移（同单元 1~4）；脚本 `tools/split_exploration.py`。
> 原文件：2021 行、97 个顶层声明 → 拆分后 `exploration.go` 159 行，新增 7 个同包文件。
> 难度：P2 中最高——`ExplorationStore` 的 80+ 个方法跨 8 个文件分布，但都同属 `db` 包、共用同一接收者类型，搬移不产生任何接口变化，调用方（server/agent）零改动。

## 一、域分组（97 个顶层声明）

| 目标文件 | 行数 | 域 | 声明数 |
|---|---|---|---|
| `exploration.go`（保留） | 159 | 节点图类型基座（`Node`/`Activity`/`TokenUsage`/`SessionTokenUsage`/`DailyTokenBucket`/`ExplorationStore`）+ 入口（`CreateExploration`/`Exploration`/`ID`/`Root`）+ payload 净化助手（`utf8Clean`/`jsonbClean`）+ `ErrIntentStateConflict` | 13 |
| `exploration_nodes.go` | 369 | 节点写读与分页：`AddNode`/`AddIntent`/`AddGoal`/`Anchor`/`Link`/`SetNodeState`/`UpdateGoalPayload`/`DeleteGoal`/`OriginFactID`、`nodeCols`+`scanNode`/`scanNodes`、`ListByKind*`/`Nodes*`/`GetNode`/`NodeFilter`/计数、`AddStandaloneFinding` | 24 |
| `exploration_intents.go` | 300 | 意图状态机：`SetIntentState`/`CompareAndSetIntentState`（CAS）、`ResetRunningIntents`/`ReopenIntent`/`ReopenBlockedIntents`、`SoftDeleteIntent`/`CancelIntent`/`IntentCleanup` | 8 |
| `exploration_activity.go` | 299 | 活动流：`AppendActivity`、`ActivityList*`、`ActivityPage*`（`ActivitySessionFilter.cond`）、`ActivityMaxID`/`ActivityDetail`、主会话分段（`MainSession`/`CurrentMainSeg`/`ListMainSessions`/`NewMainSession`） | 13 |
| `exploration_trace.go` | 271 | 轨迹检索：`traceCols`+`scanTrace`、`ActivityTrace*`（含 terminal-intent 变体与跨节点排除）、`AssetRef(s)`、`ActivityByIDs*` | 12 |
| `exploration_graph.go` | 267 | 图谱关系与前沿：`Edge`/`Edges`/`EdgesTouching`、`FactsYielded`/`FindingLineage`、`FindingIntents*`、`Frontier`、`HasActiveIntent`/`HasOpenGoal`/`ClaimIntent`、`Stats` | 12 |
| `exploration_tokens.go` | 235 | token 计量：`tokenUsageBucket`/`intentTokenRollup`/`TokenUsage.add`、`TokenDailyAll`/`TokenTotal`/`TokenTotalsAll`、`TokenStatsByWorker`/`TokenStatsBySession` | 8 |
| `exploration_metrics.go` | 165 | 列表页聚合指标：`GoalCounts`/`FindingSeverityCounts`/`TaskListMetrics`、`TaskListMetricsAll`/`GoalCountsAll`/`LastActivityAll`、`ExplorationDiag` | 7 |

## 二、归组决策记录

- **类型基座留在 `exploration.go`**：`Node`/`Activity`/`TokenUsage`/`Edge` 等类型被全部 7 个域文件引用，是包的公共语汇；入口函数（`CreateExploration`/`Root`）也留在这里，读文件先见"这是什么"、再见"怎么用"。
- **`AddStandaloneFinding` 归 nodes 而非 findings.go**：它的语义是"直接建一个 finding 节点 + 锚点"，操作对象是图节点，与 `AddNode`/`Anchor` 同源；`findings.go` 放的是查询/晋级/重测逻辑。
- **`tokenUsageBucket`/`intentTokenRollup` 与 token 查询同域**：rollup 是 `CancelIntent` 的私有依赖，但计量域自洽更重要——它只被 token 统计消费，`CancelIntent` 通过同包调用不受影响。
- **activity 与 trace 分开**：两者都读 `activities` 表但用途不同——activity 是"给人看的时间线"（分页、会话分段、详情），trace 是"给模型看的工具轨迹"（摘要列、搜索、跨节点排除）。混在一起会让 300+300 行的两套游标逻辑纠缠。
- **metrics 独立**：`TaskListMetricsAll` 一条 SQL 聚合 5 类计数，只服务任务列表页，改动频率低，单独成文件避免被别处的查询改动牵连。
- **不动 `exploration_sources.go`**：既有独立文件（来源/继承关系），本次不纳入。

## 三、验收命令

```bash
go build ./... && go vet ./db/
# 等价性：非 import/package/空行 的行集合 Counter 对比，MISSING/ADDED 必须均为 0
go test ./db/ -count=1
go test ./... -count=1
```

## 四、等价性实测

拆分前后非空非导入行 **1895 = 1895，MISSING 0 / ADDED 0**——本次连单元 4 那种 gofmt 对齐差异都没有，逐行完全一致。

## 五、执行环境备注

本次执行时 C 盘已 100% 满（构建缓存写入失败导致 `[build failed]`，非代码问题）。规避方式：`GOCACHE=D:/tmp/gocache go test ./...`（构建缓存移到 D 盘，临时目录保持默认——`GOTMPDIR` 指到 D 盘会让 `TestTaskArchivePackageSkipsSymlink` 的符号链接清理报 Access denied，属环境副作用，默认 TMPDIR 下该用例正常）。
