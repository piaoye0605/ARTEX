# 拆分规格 · 单元 2：server/server_mgmt.go

> 状态：已执行（本文档为事后留档，分组与实现一致）
> 方法：与单元 1（`server-split.md`）同构——纯同包声明搬移，不改签名与逻辑。
> 脚本：`tools/split_mgmt.py`（本版把顶层 var/const/type 也作为独立段处理，常量块按行号 `block:<line>` 映射）。

## 一、域分组（80 个顶层声明）

| 目标文件 | 行数 | 域 | 声明数 |
|---|---|---|---|
| `server_mgmt.go`（保留） | 208 | 通用 HTTP 小工具（pg/pathInt/decode）+ 任务删除域（canonicalTaskID、begin/abortTaskDelete、pgDeleteTask、waitTaskQuiescent 等）+ 通用可见性（pgResourceVisibility/pgToggleVisibility） | 12 |
| `server_skills.go` | 835 | 技能包全生命周期：列表/创建/上传(zip)/元信息/文件树读写/删除/可见性 + 路径校验（skillRelPath/walkSkillFiles/skillPathRune 等）+ zip 限额常量 | 26 |
| `server_agents.go` | 345 | 智能体 CRUD/配置/密钥（agentByKey、reAgentKey）/可见性 | 10 |
| `server_prompts.go` | 321 | 提示词与模板：保存/重置/wrapup/版本/变量/预览 + globalPromptVars + 模板校验渲染（validateTemplate/renderPrompt/templateFields） | 14 |
| `server_tools_mcp.go` | 229 | 工具开关与 MCP：列表/更新/重置/CRUD/刷新/工具发现 | 8 |
| `server_profiles.go` | 357 | LLM Profile：CRUD/激活/重试策略/资源池状态与重置/模型列表/restoreTasksAfterProfileDelete | 10 |

## 二、归组决策记录

- `validSkillName` 归 skills（技能名校验）；`reAgentKey` 归 agents（agent key 正则）——两者相邻但域不同，脚本按声明粒度拆段保证不串。
- `pgResourceVisibility`/`pgToggleVisibility` 为通用资源可见性（kind 路径参数），不属于任何单一域，保留在 mgmt。
- 1257 行 const 块（zip 限额）按行号映射进 skills。

## 三、验收命令

```bash
go build ./... && go vet ./server/
# 等价性：非 import 行排序集合 Counter 对比，MISSING/ADDED 必须 none
go test ./server/ -count=1        # PG 实库
go test ./... -count=1            # 多包并行（提交前）
```
