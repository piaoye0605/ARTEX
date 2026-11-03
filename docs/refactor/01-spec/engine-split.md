# 拆分规格 · 单元 7：server/engine.go

> 状态：已执行（本文档为事后留档，分组与实现一致）
> 方法：纯同包声明搬移（同单元 1~6）；脚本 `tools/split_engine.py`（本单元起脚本会保留 import 块与首个声明之间的头部注释）。
> 原文件：1250 行、64 个顶层声明 → 拆分后 `engine.go` 208 行，新增 7 个同包文件。

## 一、域分组（64 个顶层声明）

| 目标文件 | 行数 | 域 | 声明数 |
|---|---|---|---|
| `engine.go`（保留） | 208 | `Engine` 结构（含全部锁/通道/计数器字段）、`taskRuntime`/`workExecution`、`NewEngine`、运行期状态查询（`IsPaused`/`Started`/`LastActivity`/`BeginLLMCall`/`EndLLMCall`/`ActiveLLMCalls`/`touch`）、resolver 与就绪装配（`SetAgentResolver`/`SetAuthoritativeAgentResolver`/`snapshotFor`/`Ready`/`ReadyFor`/`SetReadiness`）、`Broadcaster`/`nextPlannerRound` | 19 |
| `engine_intent.go` | 289 | 单条意图的完整执行回合：`runIntent`（领取→执行→写回→收场归类）、`runDetachedIntent`、`claimNext`、`taskExecutionPaused`、`sleepCtx` | 5 |
| `engine_loop.go` | 241 | 两条常驻循环：`plannerLoop`（含心跳间隔/计时器重置）、`workerLoop`、`runWorkerStep`、`Run`（任务入口） | 6 |
| `engine_lifecycle.go` | 173 | 任务生命周期：`Pause`/`Resume`/`StopTask`/`cancelExec`/`execContextFor`、删除闸门（`BeginDelete`/`AbortDelete`/`IsDeleting`）、协程登记（`registerTaskRoutines`/`runTaskRoutine`） | 10 |
| `engine_work.go` | 154 | 工作控制面：`ControlWork`（stop/retry 等动作）、`registerWork`/`detachWork`/`releaseWorkControl`、`SteerWork`/`drainSteer`、`KillWork`、`transitionIntentState`（意图状态 CAS 封装） | 8 |
| `engine_steer.go` | 107 | 转向钩子与空转防护：`steerHooks` 的 `PreToolUse`/`PostToolUse`/`Stop`、`isThinkingOnlyTurn`、`defaultEmptyTurnNudges`/`emptyTurnNudge` 文案 | 7 |
| `engine_errors.go` | 75 | 错误分类与重试策略：`isFKViolation`/`dropReason`/`bumpDrop`/`preview`、`errWorkControlConflict`、`retryableWorkerModelError`、常量块（`modelErrorRetries`/`modelErrorRetryBackoff`/`workControlWaitTimeout`） | 7 |
| `engine_activity.go` | 70 | 活动流发射：`emitActivity`（内存广播 + 落库）、`appendActivity` | 2 |

## 二、归组决策记录

- **`Engine` 结构与状态查询留在 `engine.go`**：7 个文件全部以 `*Engine` 为接收者，结构体是共同语汇；把字段定义与"读状态"的方法放在一起，读文件先知道"引擎握着哪些状态"，再看各个动作怎么改它。
- **`engine_intent.go` 是最大的域但不再拆**：`runIntent` 本身就是一条 190 行的线性回合（领取 → 建上下文 → 调 LLM → 处理工具结果 → 归类收场 → 写回状态），拆开只会让"一次意图执行"这件事碎裂在多处，反而更难读。它与 `engine_loop.go` 的关系是"循环调度"与"单次执行"，边界清晰。
- **`engine_lifecycle.go` 与 `engine_work.go` 分开**：前者管**任务**（暂停/停止/删除整个 task），后者管**单条工作**（某条 intent 的 stop/retry/steer/kill）。两者都用 `e.mu`，但作用对象不同层级，混在一起会让"停任务"和"停一条意图"的边界模糊。
- **`engine_steer.go` 独立**：`steerHooks` 是注入给 agent 运行时的回调（工具前后、回合结束），属于"执行期干预"，与 `SteerWork` 的"投递转向消息"是一条链的两端——但钩子会被 worker 侧高频调用，单独成文件便于定位空转防护逻辑。
- **常量块进 `engine_errors.go`**：`modelErrorRetries`/`modelErrorRetryBackoff` 是 model_error 重试策略的参数，`workControlWaitTimeout` 是控制面等待上限，与错误分类同源；原本它紧邻 `retryableWorkerModelError`，语义上本就属于这里。
- **不动 `server/engine_timeout.go`（301 行）与 4 个 `engine_*_test.go`**：前者是既有独立文件（超时/看门狗），后者是测试，本次不纳入。

## 三、验收命令

```bash
go build ./... && go vet ./server/
# 等价性：非 import/package/空行 的行集合 Counter 对比，MISSING/ADDED 必须均为 0
go test ./server/ -count=1
go test ./... -count=1
```

## 四、等价性实测

拆分前后非空非导入行 **1148 = 1148，MISSING 0 / ADDED 0**。

> 上一单元（assets.go）暴露的"头部横幅注释丢失"坑，本单元在脚本里加了通用修复：重建主文件时保留 `import` 块与首个声明之间的全部内容（`head` 段），不再依赖"注释是否紧贴声明"的回溯规则。
