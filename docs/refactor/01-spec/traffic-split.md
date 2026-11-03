# 拆分规格 · 单元 3：traffic/traffic.go

> 状态：已执行（本文档为事后留档，分组与实现一致）
> 方法：纯同包声明搬移（同单元 1/2）；脚本 `tools/split_traffic.py`（含顶层 var/const/type 独立段处理，有名常量与匿名常量块分别按名称/行号映射）。
> 注意：本包文件名无 `traffic_` 前缀（对齐既有 archive.go / evidence.go 命名）。

## 一、域分组（88 个顶层声明）

| 目标文件 | 行数 | 域 | 声明数 |
|---|---|---|---|
| `traffic.go`（保留） | 373 | 包文档 + Traffic 类型 + Open/initIndex 生命周期 + 代理配置（ProxyAddr/SetUpstreamProxy/ValidateProxyURL/CACertPath/Start/Close）+ 存储限额常量（indexSchema/ftsSchema/匿名块）+ clip | 16 |
| `delete.go` | 666 | 主机删除全链路：阶段暂存/日志/恢复（stageTrees、RecoverHostDeleteStages、HostDeleteStage）、compactIndex、回滚/提交 | 28 |
| `query.go` | 393 | 查询 API：Search/Page/Get/Hosts/Count + FTS 过滤与排序白名单 + normalizeSearchHost | 15 |
| `capture.go` | 314 | 采集写入：sink 回调、record、spill 分级存储、二进制识别、头序列化、sanitize | 17 |
| `reclaim.go` | 236 | 空间回收：reclaim/reclaimChunk/gcBlobs/legacyBlobRefs/indexBytes | 5 |
| `tools.go` | 145 | 智能体工具暴露：TrafficSearchDescription、Tools、SeedToolMetas | 3 |
| `blobs.go` | 82 | 内容寻址 blob 读取：blobPath/Blob/BlobRange | 4 |

## 二、归组决策记录

- `indexSchema`/`ftsSchema` 与匿名常量块（存储限额）保留在 traffic.go——它们是 initIndex/Open 的核心参数，且该文件保留生命周期域。
- `Deleted`（HostDeleteStage 方法）归 delete.go；方法名与类型分离映射由脚本按声明粒度天然支持。
- `_ "modernc.org/sqlite"` 空白导入仅保留在 traffic.go（驱动注册点），goimports 复制进新文件的冗余已清理。

## 三、验收命令

```bash
go build ./... && go vet ./traffic/
# 等价性：非 import 行排序集合 Counter 对比，MISSING/ADDED 必须 none
go test ./traffic/ -count=1
go test ./... -count=1            # 多包并行（提交前）
```
