# 拆分规格 · 单元 4：agent/tools.go

> 状态：已执行（本文档为事后留档，分组与实现一致）
> 方法：纯同包声明搬移（同单元 1/2/3）；脚本 `tools/split_tools.py`。
> 原文件：2054 行 / 93 KB、85 个顶层声明 → 拆分后 `tools.go` 224 行，新增 7 个同包域文件。

## 一、域分组（85 个顶层声明）

| 目标文件 | 行数 | 域 | 声明数 |
|---|---|---|---|
| `tools.go`（保留） | 224 | `ToolSet` 类型与构造、五组回调 setter、覆盖开关（coverageOnlyTools/DropCoverageTools）、工具访问器（7 个 `XxxTool()`）、Owner 锚点、`PlannerTools` 装配入口 | 28 |
| `tools_overview.go` | 607 | 图概览域：`graphOverview`/`graphOverviewData`、关联任务概览（`relatedTaskOverviews`）与其文本预算（匿名常量块 + `overviewTextBudget` + `take`）、节点/结论压缩（`compactNode`/`compactFinding`）、`compactIntents` | 13 |
| `tools_writes.go` | 468 | worker 侧写工具：结论（`addFinding`）、事实（`recordFact`）、提示（`addHint`）、目标（`setGoals`）、约束（`setConstraints`）及各自的 `addOne*` 与条目类型 | 13 |
| `tools_trace.go` | 243 | 轨迹域：`traceSteps`、`getWorkerTrace`、`searchAllWorkerTraces`、`listWorkerTraces` | 4 |
| `tools_intents.go` | 211 | planner 侧意图/目标工具：`addIntent`、`addOneIntent`、`listGoals`、`proveGoal`、`goalMet`、`intentItem` | 6 |
| `tools_control.go` | 136 | worker 生命周期控制：`killWorkTool`、`steerWorkTool`、`getWorkerOutput` | 3 |
| `tools_read.go` | 130 | 只读工具：`listFindings`、`listFacts`、`nodeDetail` 与其分页/摘要常量、`compactFact` | 6 |
| `tools_schema.go` | 118 | 参数与装配助手：id 解析（`pid`/`pidList`）、JSON Schema 构造（`obj`/`str`/`intp`/`idp`）、工具包装（`readTool`/`writeTool`/`readExpTool`/`writeExpTool`）、`needExploration`、`jsonResult` | 12 |

## 二、归组决策记录

- **`ToolSet` 留在 `tools.go`**：它是 7 个域文件的共同接收者类型，也是唯一对外装配面（`PlannerTools`）。域文件只放方法实现，不回迁类型定义，避免循环心理负担。
- **`tools_overview.go` 而非 `tools_graph.go`**：与既有 `tools_overview_test.go` 配对，测试文件名不变即可继续定位。
- **`compactIntents` 进 overview 域**：它只被 `graphOverviewData`/`relatedTaskOverviews` 消费，是概览渲染的一部分，不是独立工具。
- **匿名常量块（`relatedOverview*` 文本预算）整体搬移**：脚本对 `const (` 按行号生成 `block:N` 键，避免 9 个常量逐个映射；该块与 `overviewTextBudget.take` 强耦合，必须同文件。
- **`listGoals` 留在 intents 域**：语义是"看意图/目标的当前状态"，与 `proveGoal`/`goalMet` 的判定闭环同域；`setGoals`（写）在 writes 域。
- **`tools_insert.go` / `tools_digest.go` 不动**：它们是早先已独立的域文件（插入类工具、摘要工具），本次不纳入。

## 三、验收命令

```bash
go build ./... && go vet ./agent/
# 等价性：非 import/package/空行 的行集合 Counter 对比，MISSING/ADDED 必须 none
go test ./agent/ -count=1
go test ./... -count=1            # 多包并行（提交前）
```

## 四、等价性实测

拆分前后非空非导入行 **1947 = 1947**，Counter 差值仅 6 条 `XxxTool()` 单行访问器的 gofmt 列对齐空格（原先同组对齐，拆分后组变小重新对齐），无任何语句增删。
